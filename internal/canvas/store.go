package canvas

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/Har103/pixelforge/internal/pg"
)

// schema is applied on every boot. Every statement is idempotent so a restart,
// a rollback, or a second replica starting at the same moment is harmless.
const schema = `
create table if not exists placements (
    seq        bigserial primary key,
    x          integer     not null,
    y          integer     not null,
    color      smallint    not null,
    uid        text        not null,
    created_at timestamptz not null default now()
);

create index if not exists placements_created_at_idx on placements (created_at);

create table if not exists snapshots (
    id         integer primary key,
    width      integer     not null,
    height     integer     not null,
    pixels     bytea       not null,
    seq        bigint      not null,
    updated_at timestamptz not null default now(),
    constraint snapshots_singleton check (id = 1)
);
`

// Store persists the canvas. Placements are appended to a durable log and the
// full grid is snapshotted periodically, so recovery is "load the snapshot,
// replay anything newer" rather than replaying all of history.
type Store struct {
	pool   *pg.Pool
	canvas *Canvas
	log    *slog.Logger

	queue chan Pixel

	// batchSize caps rows per INSERT. PostgreSQL allows 65535 bind parameters
	// per statement and each row uses five, so this stays well inside the limit.
	batchSize int

	flushInterval    time.Duration
	snapshotInterval time.Duration
}

// NewStore wires a canvas to a connection pool. Nothing touches the database
// until Migrate or Restore is called.
func NewStore(pool *pg.Pool, c *Canvas, log *slog.Logger) *Store {
	if log == nil {
		log = slog.Default()
	}
	return &Store{
		pool:             pool,
		canvas:           c,
		log:              log,
		queue:            make(chan Pixel, 4096),
		batchSize:        400,
		flushInterval:    250 * time.Millisecond,
		snapshotInterval: 20 * time.Second,
	}
}

// Ephemeral reports whether the store is running without a database. In that
// mode the canvas lives only in memory and is lost on restart; the UI shows a
// badge so nobody mistakes it for durable.
func (s *Store) Ephemeral() bool { return s.pool == nil }

// Migrate creates the tables if they are missing.
func (s *Store) Migrate(ctx context.Context) error {
	if s.Ephemeral() {
		return nil
	}
	if err := s.pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("canvas: applying schema: %w", err)
	}
	return nil
}

// Restore loads the newest snapshot and replays every placement recorded after
// it, leaving the in-memory canvas exactly where the last process left off.
func (s *Store) Restore(ctx context.Context) error {
	if s.Ephemeral() {
		s.log.Warn("no database configured, canvas will not survive a restart")
		return nil
	}
	row, err := s.pool.QueryRow(ctx, `select width, height, pixels, seq from snapshots where id = 1`)
	if err != nil {
		return fmt.Errorf("canvas: reading snapshot: %w", err)
	}

	var fromSeq int64
	if row != nil {
		w, _ := pg.Int(row[0])
		h, _ := pg.Int(row[1])
		blob, err := pg.Bytea(row[2])
		if err != nil {
			return fmt.Errorf("canvas: decoding snapshot: %w", err)
		}
		seq, _ := pg.Int64(row[3])

		if w == s.canvas.Width() && h == s.canvas.Height() {
			if err := s.canvas.Load(blob, seq); err != nil {
				return err
			}
			fromSeq = seq
			s.log.Info("restored canvas snapshot", "seq", seq, "width", w, "height", h)
		} else {
			// The grid was resized between deploys. Starting from an empty
			// canvas and replaying the whole log rebuilds whatever still fits.
			s.log.Warn("snapshot dimensions differ from configuration, replaying full history",
				"snapshot", fmt.Sprintf("%dx%d", w, h),
				"configured", fmt.Sprintf("%dx%d", s.canvas.Width(), s.canvas.Height()))
		}
	} else {
		s.log.Info("no snapshot found, starting from an empty canvas")
	}

	replayed, err := s.replayFrom(ctx, fromSeq)
	if err != nil {
		return err
	}
	if replayed > 0 {
		s.log.Info("replayed placements newer than the snapshot", "count", replayed)
	}
	return nil
}

// replayFrom applies placements with seq greater than from, in pages so a long
// history never has to fit in one result set.
func (s *Store) replayFrom(ctx context.Context, from int64) (int, error) {
	const page = 20000
	total := 0
	cursor := from
	for {
		res, err := s.pool.Query(ctx,
			`select seq, x, y, color from placements where seq > $1 order by seq asc limit $2`,
			cursor, page)
		if err != nil {
			return total, fmt.Errorf("canvas: replaying placements: %w", err)
		}
		if len(res.Rows) == 0 {
			return total, nil
		}
		for _, r := range res.Rows {
			seq, _ := pg.Int64(r[0])
			x, _ := pg.Int(r[1])
			y, _ := pg.Int(r[2])
			colour, _ := pg.Int(r[3])
			s.canvas.Apply(x, y, uint8(colour), seq)
			cursor = seq
		}
		total += len(res.Rows)
		if len(res.Rows) < page {
			return total, nil
		}
	}
}

// Record queues a placement for durable storage. It never blocks: if the writer
// has fallen far enough behind to fill the buffer, the placement is dropped
// from history and logged. The in-memory canvas and every connected client
// still show it, and the next snapshot captures it - losing a line of history
// is much better than stalling the paint path behind a slow database.
func (s *Store) Record(p Pixel) {
	if s.Ephemeral() {
		return
	}
	select {
	case s.queue <- p:
	default:
		s.log.Warn("placement history buffer full, dropping log entry", "seq", p.Seq)
	}
}

// Run drives the write-behind loop until ctx is cancelled, then drains and
// writes a final snapshot.
func (s *Store) Run(ctx context.Context) {
	if s.Ephemeral() {
		<-ctx.Done()
		return
	}
	flush := time.NewTicker(s.flushInterval)
	defer flush.Stop()
	snap := time.NewTicker(s.snapshotInterval)
	defer snap.Stop()

	pending := make([]Pixel, 0, s.batchSize)

	writePending := func() {
		if len(pending) == 0 {
			return
		}
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		if err := s.insertBatch(writeCtx, pending); err != nil {
			s.log.Error("writing placement batch", "count", len(pending), "err", err)
		}
		cancel()
		pending = pending[:0]
	}

	for {
		select {
		case <-ctx.Done():
			// Drain whatever is still queued before the process exits.
			for {
				select {
				case p := <-s.queue:
					pending = append(pending, p)
					if len(pending) >= s.batchSize {
						writePending()
					}
					continue
				default:
				}
				break
			}
			writePending()
			shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			if err := s.WriteSnapshot(shutdownCtx); err != nil {
				s.log.Error("writing shutdown snapshot", "err", err)
			} else {
				s.log.Info("wrote shutdown snapshot")
			}
			cancel()
			return

		case p := <-s.queue:
			pending = append(pending, p)
			if len(pending) >= s.batchSize {
				writePending()
			}

		case <-flush.C:
			writePending()

		case <-snap.C:
			snapCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
			if err := s.WriteSnapshot(snapCtx); err != nil {
				s.log.Error("writing periodic snapshot", "err", err)
			}
			cancel()
		}
	}
}

// insertBatch writes many placements in a single multi-row INSERT.
func (s *Store) insertBatch(ctx context.Context, batch []Pixel) error {
	if len(batch) == 0 {
		return nil
	}
	var sb strings.Builder
	sb.WriteString("insert into placements (x, y, color, uid, created_at) values ")
	args := make([]any, 0, len(batch)*5)
	for i, p := range batch {
		if i > 0 {
			sb.WriteByte(',')
		}
		base := i * 5
		sb.WriteString("($")
		sb.WriteString(strconv.Itoa(base + 1))
		sb.WriteString(",$")
		sb.WriteString(strconv.Itoa(base + 2))
		sb.WriteString(",$")
		sb.WriteString(strconv.Itoa(base + 3))
		sb.WriteString(",$")
		sb.WriteString(strconv.Itoa(base + 4))
		sb.WriteString(",$")
		sb.WriteString(strconv.Itoa(base + 5))
		sb.WriteByte(')')
		args = append(args, p.X, p.Y, int(p.Color), p.UID, p.At)
	}
	_, err := s.pool.Query(ctx, sb.String(), args...)
	return err
}

// WriteSnapshot persists the current grid, replacing the previous snapshot.
func (s *Store) WriteSnapshot(ctx context.Context) error {
	if s.Ephemeral() {
		return nil
	}
	pixels, seq := s.canvas.Snapshot()
	_, err := s.pool.Query(ctx, `
		insert into snapshots (id, width, height, pixels, seq, updated_at)
		values (1, $1, $2, $3, $4, now())
		on conflict (id) do update
		   set width = excluded.width,
		       height = excluded.height,
		       pixels = excluded.pixels,
		       seq = excluded.seq,
		       updated_at = excluded.updated_at`,
		s.canvas.Width(), s.canvas.Height(), pixels, seq)
	if err != nil {
		return fmt.Errorf("canvas: writing snapshot: %w", err)
	}
	return nil
}

// HistoryEntry is one row of the time-lapse feed.
type HistoryEntry struct {
	Seq   int64 `json:"s"`
	X     int   `json:"x"`
	Y     int   `json:"y"`
	Color uint8 `json:"c"`
	At    int64 `json:"t"` // unix milliseconds
}

// History returns up to limit placements after seq, oldest first. The client
// uses it to scrub through how the canvas was built.
func (s *Store) History(ctx context.Context, after int64, limit int) ([]HistoryEntry, error) {
	if s.Ephemeral() {
		return nil, nil
	}
	if limit <= 0 || limit > 50000 {
		limit = 50000
	}
	res, err := s.pool.Query(ctx,
		`select seq, x, y, color, extract(epoch from created_at) * 1000
		   from placements
		  where seq > $1
		  order by seq asc
		  limit $2`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("canvas: reading history: %w", err)
	}
	out := make([]HistoryEntry, 0, len(res.Rows))
	for _, r := range res.Rows {
		seq, _ := pg.Int64(r[0])
		x, _ := pg.Int(r[1])
		y, _ := pg.Int(r[2])
		colour, _ := pg.Int(r[3])
		ms, _ := pg.Float64(r[4])
		out = append(out, HistoryEntry{Seq: seq, X: x, Y: y, Color: uint8(colour), At: int64(ms)})
	}
	return out, nil
}

// TotalPlacements counts every placement ever recorded.
func (s *Store) TotalPlacements(ctx context.Context) (int64, error) {
	if s.Ephemeral() {
		return 0, nil
	}
	row, err := s.pool.QueryRow(ctx, `select count(*) from placements`)
	if err != nil || row == nil {
		return 0, err
	}
	return pg.Int64(row[0])
}

// Leaderboard is the top painters by placement count.
type LeaderRow struct {
	UID   string `json:"uid"`
	Count int64  `json:"count"`
}

// Leaderboard returns the most prolific painters.
func (s *Store) Leaderboard(ctx context.Context, limit int) ([]LeaderRow, error) {
	if s.Ephemeral() {
		return nil, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	res, err := s.pool.Query(ctx,
		`select uid, count(*) as n from placements group by uid order by n desc, uid asc limit $1`, limit)
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
