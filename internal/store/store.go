package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Har103/pixelforge/internal/pg"
)

// ErrNotFound is returned by every lookup that can miss, so callers can tell a
// missing row from a broken database.
var ErrNotFound = errors.New("store: not found")

// ErrSlugTaken is returned when a room slug collides.
var ErrSlugTaken = errors.New("store: slug already in use")

// ErrUsernameTaken is returned when a registration collides.
var ErrUsernameTaken = errors.New("store: username already registered")

// ErrAlreadyClaimed is returned when a room is claimed by an account and it
// already belongs to one. It is a distinct error rather than a silent no-op so
// the caller can say "this canvas already has an owner" instead of reporting a
// success that changed nothing.
var ErrAlreadyClaimed = errors.New("store: this room already belongs to an account")

// Store is the only thing in the program that writes SQL.
type Store struct {
	pool *pg.Pool
	log  *slog.Logger
}

// New wraps a pool. A nil pool puts the whole application in ephemeral mode:
// every read returns empty and every write is dropped, which lets the service
// boot and serve without a database instead of crash-looping in front of
// whoever is looking at it.
func New(pool *pg.Pool, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	return &Store{pool: pool, log: log}
}

// Ephemeral reports whether there is no database behind this store.
func (s *Store) Ephemeral() bool { return s == nil || s.pool == nil }

// migrationLock is an arbitrary but fixed advisory lock key, taken for the
// length of each migration statement.
//
// "create table if not exists" is not atomic against a concurrent creator: it
// looks, finds nothing, and then loses the race inserting into the catalogue,
// which surfaces as "duplicate key value violates unique constraint
// pg_type_typname_nsp_index" and fails the whole migration. Two processes
// starting against one empty database at the same moment is not hypothetical -
// it is what a rolling deploy does, and what a test suite running several
// packages against one database does. Ten concurrent migrations against an
// empty schema fail five times without this lock and never with it.
const migrationLock = 4467831641293411

// Migrate applies the schema and folds a pre-rooms installation forward.
func (s *Store) Migrate(ctx context.Context) error {
	if s.Ephemeral() {
		return nil
	}
	// Each Exec is a single simple-protocol query, which PostgreSQL runs as one
	// implicit transaction, so a transaction-scoped lock covers the whole batch
	// and is released without anything having to remember to unlock it - which
	// matters, because the alternative leaks the lock when a migration fails.
	lock := fmt.Sprintf("select pg_advisory_xact_lock(%d);\n", migrationLock)

	if err := s.pool.Exec(ctx, lock+schema); err != nil {
		return fmt.Errorf("store: applying schema: %w", err)
	}
	// The v1 fold has to serialise for a different reason: its guard is "the old
	// tables exist and rooms is empty", which two callers can both see at once.
	if err := s.pool.Exec(ctx, lock+migrateV1); err != nil {
		return fmt.Errorf("store: migrating the pre-rooms canvas: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- rooms -----

// Room is a room's configuration and ownership, without its pixels.
type Room struct {
	ID         int64
	Slug       string
	Name       string
	Width      int
	Height     int
	Palette    string
	CooldownMs int
	Visibility string // public | unlisted
	OwnerHash  string // HMAC of the moderator key; never the key itself
	OwnerUser  int64  // 0 when the room belongs to no account
	Paused     bool
	CreatedAt  time.Time
	LastActive time.Time
}

// RoomSummary is a room plus the counts the browse page needs.
type RoomSummary struct {
	Room
	Placements int64
	Painted    int
}

// The two column lists are written out rather than derived from each other.
// A helper that prefixed a comma-separated list with a table alias looked
// tidier and quietly corrupted `coalesce(owner_user, 0)` by splitting it on its
// own comma - so this is spelled out on purpose.
const roomColumns = `id, slug, name, width, height, palette, cooldown_ms,
                     visibility, owner_hash, coalesce(owner_user, 0), paused,
                     created_at, last_active`

const roomColumnsAliased = `r.id, r.slug, r.name, r.width, r.height, r.palette,
                            r.cooldown_ms, r.visibility, r.owner_hash,
                            coalesce(r.owner_user, 0), r.paused,
                            r.created_at, r.last_active`

func scanRoom(row [][]byte) (Room, error) {
	if len(row) < 13 {
		return Room{}, fmt.Errorf("store: room row has %d columns, want 13", len(row))
	}
	id, _ := pg.Int64(row[0])
	w, _ := pg.Int(row[3])
	h, _ := pg.Int(row[4])
	cd, _ := pg.Int(row[6])
	owner, _ := pg.Int64(row[9])
	created, _ := pg.Time(row[11])
	active, _ := pg.Time(row[12])
	return Room{
		ID:         id,
		Slug:       pg.Text(row[1]),
		Name:       pg.Text(row[2]),
		Width:      w,
		Height:     h,
		Palette:    pg.Text(row[5]),
		CooldownMs: cd,
		Visibility: pg.Text(row[7]),
		OwnerHash:  pg.Text(row[8]),
		OwnerUser:  owner,
		Paused:     pg.Bool(row[10]),
		CreatedAt:  created,
		LastActive: active,
	}, nil
}

// scanRoomSummary reads a room row that carries a placement count in a
// fourteenth column.
//
// It exists because both callers of that query used to scan the room with
// scanRoom - whose guard only requires thirteen columns - and then reach
// straight for row[13]. A row with exactly thirteen columns passed the check
// and panicked on the very next line, taking the request handler with it. One
// function that knows it needs fourteen cannot drift apart from the guard that
// proves it has them.
func scanRoomSummary(row [][]byte) (RoomSummary, error) {
	if len(row) < 14 {
		return RoomSummary{}, fmt.Errorf("store: room summary row has %d columns, want 14", len(row))
	}
	r, err := scanRoom(row)
	if err != nil {
		return RoomSummary{}, err
	}
	n, _ := pg.Int64(row[13])
	return RoomSummary{Room: r, Placements: n}, nil
}

// CreateRoom inserts a room and returns it with its assigned id.
func (s *Store) CreateRoom(ctx context.Context, r Room) (Room, error) {
	if s.Ephemeral() {
		r.ID = time.Now().UnixNano() // enough to key an in-memory room
		r.CreatedAt = time.Now()
		r.LastActive = r.CreatedAt
		return r, nil
	}
	var ownerUser any
	if r.OwnerUser != 0 {
		ownerUser = r.OwnerUser
	}
	res, err := s.pool.Query(ctx, `
		insert into rooms (slug, name, width, height, palette, cooldown_ms,
		                   visibility, owner_hash, owner_user)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		returning `+roomColumns,
		r.Slug, r.Name, r.Width, r.Height, r.Palette, r.CooldownMs,
		r.Visibility, r.OwnerHash, ownerUser)
	if err != nil {
		var pe *pg.Error
		if errors.As(err, &pe) && pe.SQLState() == "23505" {
			return Room{}, ErrSlugTaken
		}
		return Room{}, fmt.Errorf("store: creating room: %w", err)
	}
	if len(res.Rows) == 0 {
		return Room{}, errors.New("store: insert returned no row")
	}
	return scanRoom(res.Rows[0])
}

// RoomBySlug looks a room up by its URL slug.
func (s *Store) RoomBySlug(ctx context.Context, slug string) (Room, error) {
	if s.Ephemeral() {
		return Room{}, ErrNotFound
	}
	row, err := s.pool.QueryRow(ctx, `select `+roomColumns+` from rooms where slug = $1`, slug)
	if err != nil {
		return Room{}, fmt.Errorf("store: loading room: %w", err)
	}
	if row == nil {
		return Room{}, ErrNotFound
	}
	return scanRoom(row)
}

// UpdateRoom persists the mutable parts of a room's configuration.
func (s *Store) UpdateRoom(ctx context.Context, r Room) error {
	if s.Ephemeral() {
		return nil
	}
	_, err := s.pool.Query(ctx, `
		update rooms
		   set name = $2, visibility = $3, cooldown_ms = $4, paused = $5
		 where id = $1`,
		r.ID, r.Name, r.Visibility, r.CooldownMs, r.Paused)
	if err != nil {
		return fmt.Errorf("store: updating room: %w", err)
	}
	return nil
}

// TouchRoom records activity so the browse page can order by liveliness. It is
// deliberately fire-and-forget: a failure here must never fail a paint.
func (s *Store) TouchRoom(ctx context.Context, id int64) {
	if s.Ephemeral() {
		return
	}
	if _, err := s.pool.Query(ctx, `update rooms set last_active = now() where id = $1`, id); err != nil {
		s.log.Debug("touching room", "room", id, "err", err)
	}
}

// ListRooms returns public rooms, most recently active first.
func (s *Store) ListRooms(ctx context.Context, limit int) ([]RoomSummary, error) {
	if s.Ephemeral() {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 60
	}
	res, err := s.pool.Query(ctx, `
		select `+roomColumnsAliased+`, coalesce(p.n, 0)
		  from rooms r
		  left join (
		        select room_id, count(*) as n
		          from room_placements
		         where not undone
		         group by room_id
		  ) p on p.room_id = r.id
		 where r.visibility = 'public'
		 order by r.last_active desc
		 limit $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: listing rooms: %w", err)
	}
	out := make([]RoomSummary, 0, len(res.Rows))
	for _, row := range res.Rows {
		summary, err := scanRoomSummary(row)
		if err != nil {
			return nil, err
		}
		out = append(out, summary)
	}
	return out, nil
}

// RoomsForUser lists the rooms an account owns.
func (s *Store) RoomsForUser(ctx context.Context, userID int64) ([]RoomSummary, error) {
	if s.Ephemeral() || userID == 0 {
		return nil, nil
	}
	res, err := s.pool.Query(ctx, `
		select `+roomColumnsAliased+`, coalesce(p.n, 0)
		  from rooms r
		  left join (
		        select room_id, count(*) as n
		          from room_placements where not undone group by room_id
		  ) p on p.room_id = r.id
		 where r.owner_user = $1
		 order by r.last_active desc
		 limit 200`, userID)
	if err != nil {
		return nil, fmt.Errorf("store: listing owned rooms: %w", err)
	}
	out := make([]RoomSummary, 0, len(res.Rows))
	for _, row := range res.Rows {
		summary, err := scanRoomSummary(row)
		if err != nil {
			return nil, err
		}
		out = append(out, summary)
	}
	return out, nil
}

// ClaimRoom attaches an unowned room to an account, so a moderator key can be
// upgraded into something that survives losing the cookie.
// It attaches an *unowned* room, and the "unowned" is load-bearing. Without it
// the moderator key becomes a transfer of ownership: anybody who has ever been
// given a recovery link - which is designed to be saved and shared - could
// reassign the room to their own account and lock the original owner out of
// something they made. A room that already has an owner is left alone and the
// caller is told, so the interface can say so instead of reporting a success
// that did nothing.
func (s *Store) ClaimRoom(ctx context.Context, roomID, userID int64) error {
	if s.Ephemeral() {
		return nil
	}
	res, err := s.pool.Query(ctx,
		`update rooms set owner_user = $2 where id = $1 and owner_user is null returning id`,
		roomID, userID)
	if err != nil {
		return fmt.Errorf("store: claiming room: %w", err)
	}
	if len(res.Rows) == 0 {
		return ErrAlreadyClaimed
	}
	return nil
}

// ------------------------------------------------------------ snapshots -----

// Snapshot is a room's grid at a point in its history.
type Snapshot struct {
	Width  int
	Height int
	Pixels []byte
	Seq    int64
}

// LoadSnapshot returns the stored grid for a room, or ErrNotFound.
func (s *Store) LoadSnapshot(ctx context.Context, roomID int64) (Snapshot, error) {
	if s.Ephemeral() {
		return Snapshot{}, ErrNotFound
	}
	row, err := s.pool.QueryRow(ctx,
		`select width, height, pixels, seq from room_snapshots where room_id = $1`, roomID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("store: reading snapshot: %w", err)
	}
	if row == nil {
		return Snapshot{}, ErrNotFound
	}
	w, _ := pg.Int(row[0])
	h, _ := pg.Int(row[1])
	blob, err := pg.Bytea(row[2])
	if err != nil {
		return Snapshot{}, fmt.Errorf("store: decoding snapshot: %w", err)
	}
	seq, _ := pg.Int64(row[3])
	return Snapshot{Width: w, Height: h, Pixels: blob, Seq: seq}, nil
}

// SaveSnapshot replaces a room's stored grid.
func (s *Store) SaveSnapshot(ctx context.Context, roomID int64, snap Snapshot) error {
	if s.Ephemeral() {
		return nil
	}
	_, err := s.pool.Query(ctx, `
		insert into room_snapshots (room_id, width, height, pixels, seq, updated_at)
		values ($1,$2,$3,$4,$5, now())
		on conflict (room_id) do update
		   set width = excluded.width, height = excluded.height,
		       pixels = excluded.pixels, seq = excluded.seq,
		       updated_at = excluded.updated_at`,
		roomID, snap.Width, snap.Height, snap.Pixels, snap.Seq)
	if err != nil {
		return fmt.Errorf("store: writing snapshot: %w", err)
	}
	return nil
}

// ----------------------------------------------------------- placements -----

// Placement is one row of the append-only log.
type Placement struct {
	Seq   int64
	X     int
	Y     int
	Color uint8
	UID   string
	At    time.Time
}

// AppendPlacements writes many placements in a single multi-row INSERT.
// PostgreSQL allows 65535 bind parameters per statement and each row uses six,
// so callers should keep batches well under ten thousand.
func (s *Store) AppendPlacements(ctx context.Context, roomID int64, batch []Placement) error {
	if s.Ephemeral() || len(batch) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString("insert into room_placements (room_id, room_seq, x, y, color, uid, created_at) values ")
	args := make([]any, 0, len(batch)*7)
	for i, p := range batch {
		if i > 0 {
			sb.WriteByte(',')
		}
		base := i * 7
		sb.WriteString("($")
		for j := 1; j <= 7; j++ {
			if j > 1 {
				sb.WriteString(",$")
			}
			sb.WriteString(strconv.Itoa(base + j))
		}
		sb.WriteByte(')')
		args = append(args, roomID, p.Seq, p.X, p.Y, int(p.Color), p.UID, p.At)
	}
	if _, err := s.pool.Query(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("store: appending placements: %w", err)
	}
	return nil
}

// ReplayAfter streams every live placement newer than a sequence, in order,
// calling fn for each. Paging keeps a long history off the heap.
func (s *Store) ReplayAfter(ctx context.Context, roomID, after int64,
	fn func(seq int64, x, y int, colour uint8)) (int, error) {

	if s.Ephemeral() {
		return 0, nil
	}
	// Paged on (room_seq, id) rather than room_seq alone. Sequence numbers are
	// handed out by the in-memory canvas, so nothing in the database stops two
	// rows sharing one - two replicas serving the same room would produce that
	// routinely, and the pre-rooms fold-forward could too. With a cursor of
	// "room_seq > n" the duplicate that happened to land at the end of a page
	// took its twin with it, and the pixel simply never appeared after a
	// restart. Ordering by the primary key as well makes the position in the
	// stream unique whether or not the sequence is.
	//
	// The id half of the cursor starts at its maximum so that the first page is
	// exactly the old condition, room_seq > after, and nothing at the snapshot's
	// own sequence is replayed on top of it. Only the pages after the first use
	// the id to break a tie.
	const page = 20000
	total := 0
	cursorSeq, cursorID := after, int64(math.MaxInt64)
	for {
		res, err := s.pool.Query(ctx, `
			select room_seq, x, y, color, id
			  from room_placements
			 where room_id = $1 and not undone
			   and (room_seq > $2 or (room_seq = $2 and id > $3))
			 order by room_seq asc, id asc
			 limit $4`, roomID, cursorSeq, cursorID, page)
		if err != nil {
			return total, fmt.Errorf("store: replaying placements: %w", err)
		}
		if len(res.Rows) == 0 {
			return total, nil
		}
		for _, r := range res.Rows {
			if len(r) < 5 {
				return total, fmt.Errorf("store: placement row has %d columns, want 5", len(r))
			}
			seq, _ := pg.Int64(r[0])
			x, _ := pg.Int(r[1])
			y, _ := pg.Int(r[2])
			c, _ := pg.Int(r[3])
			id, _ := pg.Int64(r[4])
			fn(seq, x, y, uint8(c))
			cursorSeq, cursorID = seq, id
		}
		total += len(res.Rows)
		if len(res.Rows) < page {
			return total, nil
		}
	}
}

// HistoryEntry is one row of the time-lapse feed.
type HistoryEntry struct {
	Seq   int64 `json:"s"`
	X     int   `json:"x"`
	Y     int   `json:"y"`
	Color uint8 `json:"c"`
	At    int64 `json:"t"` // unix milliseconds
}

// History returns up to limit live placements after a sequence, oldest first.
func (s *Store) History(ctx context.Context, roomID, after int64, limit int) ([]HistoryEntry, error) {
	if s.Ephemeral() {
		return nil, nil
	}
	if limit <= 0 || limit > 200000 {
		limit = 50000
	}
	res, err := s.pool.Query(ctx, `
		select room_seq, x, y, color, extract(epoch from created_at) * 1000
		  from room_placements
		 where room_id = $1 and room_seq > $2 and not undone
		 order by room_seq asc
		 limit $3`, roomID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("store: reading history: %w", err)
	}
	out := make([]HistoryEntry, 0, len(res.Rows))
	for _, r := range res.Rows {
		seq, _ := pg.Int64(r[0])
		x, _ := pg.Int(r[1])
		y, _ := pg.Int(r[2])
		c, _ := pg.Int(r[3])
		ms, _ := pg.Float64(r[4])
		out = append(out, HistoryEntry{Seq: seq, X: x, Y: y, Color: uint8(c), At: int64(ms)})
	}
	return out, nil
}

// CountPlacements counts live placements in a room.
func (s *Store) CountPlacements(ctx context.Context, roomID int64) (int64, error) {
	if s.Ephemeral() {
		return 0, nil
	}
	row, err := s.pool.QueryRow(ctx,
		`select count(*) from room_placements where room_id = $1 and not undone`, roomID)
	if err != nil || row == nil {
		return 0, err
	}
	return pg.Int64(row[0])
}

// LeaderRow is one entry of a room's most-prolific-painter list.
type LeaderRow struct {
	UID   string `json:"uid"`
	Count int64  `json:"count"`
}

// Leaderboard returns the busiest painters in a room.
func (s *Store) Leaderboard(ctx context.Context, roomID int64, limit int) ([]LeaderRow, error) {
	if s.Ephemeral() {
		return nil, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	res, err := s.pool.Query(ctx, `
		select uid, count(*) as n
		  from room_placements
		 where room_id = $1 and not undone
		 group by uid
		 order by n desc, uid asc
		 limit $2`, roomID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]LeaderRow, 0, len(res.Rows))
	for _, r := range res.Rows {
		n, _ := pg.Int64(r[1])
		out = append(out, LeaderRow{UID: pg.Text(r[0]), Count: n})
	}
	return out, nil
}

// UndoUser marks every placement by one painter as undone and reports how many
// were affected. The rows stay so the action is auditable and reversible.
func (s *Store) UndoUser(ctx context.Context, roomID int64, uid string) (int64, error) {
	if s.Ephemeral() {
		return 0, nil
	}
	res, err := s.pool.Query(ctx,
		`update room_placements set undone = true
		  where room_id = $1 and uid = $2 and not undone`, roomID, uid)
	if err != nil {
		return 0, fmt.Errorf("store: undoing placements: %w", err)
	}
	return res.Affected, nil
}

// ClearRoom marks a room's entire history undone, which is what "clear the
// canvas" means when history has to stay reconstructable.
func (s *Store) ClearRoom(ctx context.Context, roomID int64) error {
	if s.Ephemeral() {
		return nil
	}
	_, err := s.pool.Query(ctx,
		`update room_placements set undone = true where room_id = $1 and not undone`, roomID)
	return err
}

// DeleteRoom removes a room and everything belonging to it.
//
// This is the only destructive operation in the store, and it exists because
// there was no way to take a canvas back. Somebody who created one by mistake,
// or tested with a name they regret, or simply finished with it, had made a
// permanent entry on a public browse page. Clear only blanks the grid and leaves
// the room; pausing hides nothing.
//
// One statement, so a half-deleted room cannot exist even briefly - the children
// go in data-modifying CTEs and the room itself in the outer delete, which
// PostgreSQL runs as a single transaction. The order does not matter for
// correctness here because there are no foreign keys, but it does matter that
// nothing can observe a room whose pixels have already gone.
func (s *Store) DeleteRoom(ctx context.Context, roomID int64) error {
	if s.Ephemeral() {
		return nil
	}
	res, err := s.pool.Query(ctx, `
		with p as (delete from room_placements where room_id = $1),
		     s as (delete from room_snapshots  where room_id = $1),
		     b as (delete from bans            where room_id = $1),
		     l as (delete from locks           where room_id = $1)
		delete from rooms where id = $1 returning id`, roomID)
	if err != nil {
		return fmt.Errorf("store: deleting room: %w", err)
	}
	if len(res.Rows) == 0 {
		return ErrNotFound
	}
	return nil
}

// ------------------------------------------------------------------ bans ----

// Bans returns the banned painter ids for a room.
func (s *Store) Bans(ctx context.Context, roomID int64) (map[string]bool, error) {
	if s.Ephemeral() {
		return map[string]bool{}, nil
	}
	res, err := s.pool.Query(ctx, `select uid from bans where room_id = $1`, roomID)
	if err != nil {
		return nil, fmt.Errorf("store: loading bans: %w", err)
	}
	out := make(map[string]bool, len(res.Rows))
	for _, r := range res.Rows {
		out[pg.Text(r[0])] = true
	}
	return out, nil
}

// LockRect is one frozen rectangle, in the shape the locks table stores.
type LockRect struct {
	X1, Y1, X2, Y2 int
}

// Locks loads a room's frozen rectangles.
//
// The table has existed since the first schema and nothing read or wrote it,
// so freezing a region was an in-memory decision only: the room was evicted
// twenty minutes after everyone left, reloaded without its locks, and the area
// a moderator had deliberately protected quietly became paintable again with
// nothing on screen to say it had changed.
func (s *Store) Locks(ctx context.Context, roomID int64) ([]LockRect, error) {
	if s.Ephemeral() {
		return nil, nil
	}
	res, err := s.pool.Query(ctx,
		`select x1, y1, x2, y2 from locks where room_id = $1 order by id`, roomID)
	if err != nil {
		return nil, fmt.Errorf("store: loading locks: %w", err)
	}
	out := make([]LockRect, 0, len(res.Rows))
	for _, r := range res.Rows {
		if len(r) < 4 {
			return nil, fmt.Errorf("store: lock row has %d columns, want 4", len(r))
		}
		x1, _ := pg.Int(r[0])
		y1, _ := pg.Int(r[1])
		x2, _ := pg.Int(r[2])
		y2, _ := pg.Int(r[3])
		out = append(out, LockRect{X1: x1, Y1: y1, X2: x2, Y2: y2})
	}
	return out, nil
}

// SetLocks replaces a room's frozen rectangles wholesale, which is how the
// moderation UI thinks about them: it sends the set it wants to exist.
//
// Delete and insert rather than a diff, in a single statement so that a reader
// can never observe the moment where the old set is gone and the new one has
// not arrived - which would be a window in which a protected area is paintable.
// The delete rides in a data-modifying CTE precisely because it has to be one
// statement: the extended query protocol this driver uses for anything carrying
// parameters will not accept two.
func (s *Store) SetLocks(ctx context.Context, roomID int64, locks []LockRect) error {
	if s.Ephemeral() {
		return nil
	}
	if len(locks) == 0 {
		if _, err := s.pool.Query(ctx, `delete from locks where room_id = $1`, roomID); err != nil {
			return fmt.Errorf("store: clearing locks: %w", err)
		}
		return nil
	}

	var sb strings.Builder
	sb.WriteString(`with cleared as (delete from locks where room_id = $1)
		insert into locks (room_id, x1, y1, x2, y2) values `)
	args := make([]any, 0, 1+len(locks)*4)
	args = append(args, roomID)
	for i, l := range locks {
		if i > 0 {
			sb.WriteByte(',')
		}
		base := i*4 + 1
		sb.WriteString("($1,$" + strconv.Itoa(base+1) +
			",$" + strconv.Itoa(base+2) +
			",$" + strconv.Itoa(base+3) +
			",$" + strconv.Itoa(base+4) + ")")
		args = append(args, l.X1, l.Y1, l.X2, l.Y2)
	}

	if _, err := s.pool.Query(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("store: saving locks: %w", err)
	}
	return nil
}

// Ban blocks a painter from a room.
func (s *Store) Ban(ctx context.Context, roomID int64, uid string) error {
	if s.Ephemeral() {
		return nil
	}
	_, err := s.pool.Query(ctx,
		`insert into bans (room_id, uid) values ($1,$2) on conflict do nothing`, roomID, uid)
	return err
}

// Unban lifts a block.
func (s *Store) Unban(ctx context.Context, roomID int64, uid string) error {
	if s.Ephemeral() {
		return nil
	}
	_, err := s.pool.Query(ctx, `delete from bans where room_id = $1 and uid = $2`, roomID, uid)
	return err
}

// ----------------------------------------------------------------- users ----

// User is an account.
type User struct {
	ID        int64
	Username  string
	PwHash    string
	CreatedAt time.Time
}

// CreateUser registers an account. The caller supplies an already-hashed
// password; this package does not do cryptography.
func (s *Store) CreateUser(ctx context.Context, username, pwHash string) (User, error) {
	if s.Ephemeral() {
		return User{}, errors.New("store: accounts need a database")
	}
	res, err := s.pool.Query(ctx, `
		insert into users (username, username_key, pw_hash)
		values ($1, lower($1), $2)
		returning id, username, pw_hash, created_at`, username, pwHash)
	if err != nil {
		var pe *pg.Error
		if errors.As(err, &pe) && pe.SQLState() == "23505" {
			return User{}, ErrUsernameTaken
		}
		return User{}, fmt.Errorf("store: creating user: %w", err)
	}
	// "insert ... returning" always yields a row when it succeeds, so this is
	// unreachable today. It is here because the alternative to an error is an
	// index out of range, and an assumption about someone else's software is a
	// poor thing to stake a panic on.
	if len(res.Rows) == 0 {
		return User{}, fmt.Errorf("store: creating user: insert returned no row")
	}
	return scanUser(res.Rows[0])
}

// UserByName looks an account up case-insensitively.
func (s *Store) UserByName(ctx context.Context, username string) (User, error) {
	if s.Ephemeral() {
		return User{}, ErrNotFound
	}
	row, err := s.pool.QueryRow(ctx,
		`select id, username, pw_hash, created_at from users where username_key = lower($1)`, username)
	if err != nil {
		return User{}, fmt.Errorf("store: loading user: %w", err)
	}
	if row == nil {
		return User{}, ErrNotFound
	}
	return scanUser(row)
}

// UserByID looks an account up by primary key.
func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	if s.Ephemeral() || id == 0 {
		return User{}, ErrNotFound
	}
	row, err := s.pool.QueryRow(ctx,
		`select id, username, pw_hash, created_at from users where id = $1`, id)
	if err != nil {
		return User{}, fmt.Errorf("store: loading user: %w", err)
	}
	if row == nil {
		return User{}, ErrNotFound
	}
	return scanUser(row)
}

func scanUser(row [][]byte) (User, error) {
	if len(row) < 4 {
		return User{}, errors.New("store: user row is too short")
	}
	id, _ := pg.Int64(row[0])
	created, _ := pg.Time(row[3])
	return User{ID: id, Username: pg.Text(row[1]), PwHash: pg.Text(row[2]), CreatedAt: created}, nil
}

// Ready reports whether the database answers, for the readiness probe.
func (s *Store) Ready(ctx context.Context) error {
	if s.Ephemeral() {
		return nil
	}
	return s.pool.Do(ctx, func(c *pg.Conn) error { return c.Ping(ctx) })
}

// -------------------------------------------------------------- one cell ----

// CellPlacement is one entry in a single cell's history.
type CellPlacement struct {
	Seq    int64     `json:"seq"`
	Color  uint8     `json:"c"`
	UID    string    `json:"uid"`
	At     time.Time `json:"-"`
	AtMs   int64     `json:"t"`
	Undone bool      `json:"undone"`
}

// CellHistory returns the most recent placements at one cell, newest first.
// This is what powers "who painted this, and what was underneath" — the
// question that turns a grid into something people argue about.
func (s *Store) CellHistory(ctx context.Context, roomID int64, x, y, limit int) ([]CellPlacement, error) {
	if s.Ephemeral() {
		return nil, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 12
	}
	res, err := s.pool.Query(ctx, `
		select room_seq, color, uid, extract(epoch from created_at) * 1000, undone
		  from room_placements
		 where room_id = $1 and x = $2 and y = $3
		 order by room_seq desc
		 limit $4`, roomID, x, y, limit)
	if err != nil {
		return nil, fmt.Errorf("store: reading cell history: %w", err)
	}
	out := make([]CellPlacement, 0, len(res.Rows))
	for _, r := range res.Rows {
		seq, _ := pg.Int64(r[0])
		c, _ := pg.Int(r[1])
		ms, _ := pg.Float64(r[3])
		out = append(out, CellPlacement{
			Seq: seq, Color: uint8(c), UID: pg.Text(r[2]),
			AtMs: int64(ms), Undone: pg.Bool(r[4]),
		})
	}
	return out, nil
}

// LatestOwnPlacement returns a painter's most recent live placement in a room,
// or ErrNotFound.
func (s *Store) LatestOwnPlacement(ctx context.Context, roomID int64, uid string) (Placement, error) {
	if s.Ephemeral() {
		return Placement{}, ErrNotFound
	}
	row, err := s.pool.QueryRow(ctx, `
		select room_seq, x, y, color
		  from room_placements
		 where room_id = $1 and uid = $2 and not undone
		 order by room_seq desc
		 limit 1`, roomID, uid)
	if err != nil {
		return Placement{}, fmt.Errorf("store: finding your last placement: %w", err)
	}
	if row == nil {
		return Placement{}, ErrNotFound
	}
	seq, _ := pg.Int64(row[0])
	x, _ := pg.Int(row[1])
	y, _ := pg.Int(row[2])
	c, _ := pg.Int(row[3])
	return Placement{Seq: seq, X: x, Y: y, Color: uint8(c), UID: uid}, nil
}

// TopPlacementAt returns the placement currently showing at a cell, or
// ErrNotFound if the cell has never been painted.
func (s *Store) TopPlacementAt(ctx context.Context, roomID int64, x, y int) (Placement, error) {
	if s.Ephemeral() {
		return Placement{}, ErrNotFound
	}
	row, err := s.pool.QueryRow(ctx, `
		select room_seq, color, uid
		  from room_placements
		 where room_id = $1 and x = $2 and y = $3 and not undone
		 order by room_seq desc
		 limit 1`, roomID, x, y)
	if err != nil {
		return Placement{}, fmt.Errorf("store: reading cell: %w", err)
	}
	if row == nil {
		return Placement{}, ErrNotFound
	}
	seq, _ := pg.Int64(row[0])
	c, _ := pg.Int(row[1])
	return Placement{Seq: seq, X: x, Y: y, Color: uint8(c), UID: pg.Text(row[2])}, nil
}

// ColourBeneath returns the colour a cell would revert to if the placement at
// seq were undone: the newest surviving placement older than it, or the
// background when there is none.
func (s *Store) ColourBeneath(ctx context.Context, roomID int64, x, y int, seq int64) (uint8, error) {
	if s.Ephemeral() {
		return 0, nil
	}
	row, err := s.pool.QueryRow(ctx, `
		select color
		  from room_placements
		 where room_id = $1 and x = $2 and y = $3 and not undone and room_seq < $4
		 order by room_seq desc
		 limit 1`, roomID, x, y, seq)
	if err != nil {
		return 0, fmt.Errorf("store: reading what is underneath: %w", err)
	}
	if row == nil {
		return 0, nil // nothing underneath, so the background
	}
	c, _ := pg.Int(row[0])
	return uint8(c), nil
}

// MarkUndone retires a single placement.
func (s *Store) MarkUndone(ctx context.Context, roomID int64, seq int64) error {
	if s.Ephemeral() {
		return nil
	}
	_, err := s.pool.Query(ctx,
		`update room_placements set undone = true where room_id = $1 and room_seq = $2`, roomID, seq)
	return err
}
