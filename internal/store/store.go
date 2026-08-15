package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// Migrate applies the schema and folds a pre-rooms installation forward.
func (s *Store) Migrate(ctx context.Context) error {
	if s.Ephemeral() {
		return nil
	}
	if err := s.pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("store: applying schema: %w", err)
	}
	if err := s.pool.Exec(ctx, migrateV1); err != nil {
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
		r, err := scanRoom(row)
		if err != nil {
			return nil, err
		}
		n, _ := pg.Int64(row[13])
		out = append(out, RoomSummary{Room: r, Placements: n})
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
		r, err := scanRoom(row)
		if err != nil {
			return nil, err
		}
		n, _ := pg.Int64(row[13])
		out = append(out, RoomSummary{Room: r, Placements: n})
	}
	return out, nil
}

// ClaimRoom attaches an unowned room to an account, so a moderator key can be
// upgraded into something that survives losing the cookie.
func (s *Store) ClaimRoom(ctx context.Context, roomID, userID int64) error {
	if s.Ephemeral() {
		return nil
	}
	_, err := s.pool.Query(ctx, `update rooms set owner_user = $2 where id = $1`, roomID, userID)
	return err
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
	const page = 20000
	total, cursor := 0, after
	for {
		res, err := s.pool.Query(ctx, `
			select room_seq, x, y, color
			  from room_placements
			 where room_id = $1 and room_seq > $2 and not undone
			 order by room_seq asc
			 limit $3`, roomID, cursor, page)
		if err != nil {
			return total, fmt.Errorf("store: replaying placements: %w", err)
		}
		if len(res.Rows) == 0 {
			return total, nil
		}
		for _, r := range res.Rows {
			seq, _ := pg.Int64(r[0])
			x, _ := pg.Int(r[1])
			y, _ := pg.Int(r[2])
			c, _ := pg.Int(r[3])
			fn(seq, x, y, uint8(c))
			cursor = seq
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
