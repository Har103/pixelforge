package pg

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests need a real PostgreSQL, because the thing under test is agreement
// with a real server: what it puts on the wire for a timestamp, what it accepts
// as a bytea literal, what it does to a connection after an error. A fake can
// only ever repeat what this driver already believes. Point PIXELFORGE_TEST_DSN
// at a throwaway database to run them.
//
// The server here authenticates with SCRAM-SHA-256, so every one of these also
// runs the real handshake end to end.
func liveConfig(t *testing.T) *Config {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("PIXELFORGE_TEST_DSN"))
	if dsn == "" {
		t.Skip("set PIXELFORGE_TEST_DSN to run the tests that need a real PostgreSQL")
	}
	cfg, err := ParseDSN(dsn)
	if err != nil {
		t.Fatalf("parsing PIXELFORGE_TEST_DSN: %v", err)
	}
	return cfg
}

func liveConn(t *testing.T) *Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Connect(ctx, liveConfig(t))
	if err != nil {
		t.Fatalf("connecting to the test database: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// liveTable creates a scratch table with a name unique to the test and drops it
// afterwards, so tests never read each other's rows.
func liveTable(t *testing.T, c *Conn, columns string) string {
	t.Helper()
	name := fmt.Sprintf("pgtest_%d", time.Now().UnixNano())
	ctx := bg(t)
	if err := c.Exec(ctx, "create table "+name+" ("+columns+")"); err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := c.Exec(ctx, "drop table if exists "+name); err != nil {
			t.Logf("dropping %s: %v", name, err)
		}
	})
	return name
}

// TestLiveTextValuesRoundTrip pushes the awkward strings through a real server
// and back. Quoting is the server's problem because these go as parameters, not
// as literals - which is the point: if a value ever came back changed, some
// caller would be building SQL by hand somewhere.
func TestLiveTextValuesRoundTrip(t *testing.T) {
	c := liveConn(t)
	table := liveTable(t, c, "id int, v text")
	ctx := bg(t)

	values := []string{
		"",
		" ",
		"plain",
		"it's got a quote",
		`back\slash`,
		`'; drop table students; --`,
		"tab\there",
		"newline\nhere",
		"canvas ✏️ 🎨",
		strings.Repeat("x", 10000),
		"%s %d %%",
	}
	for i, v := range values {
		if err := c.Exec(ctx, "begin"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Query(ctx, "insert into "+table+" (id, v) values ($1, $2)", i, v); err != nil {
			t.Fatalf("inserting %q: %v", v, err)
		}
		if err := c.Exec(ctx, "commit"); err != nil {
			t.Fatal(err)
		}
	}

	res, err := c.Query(ctx, "select id, v from "+table+" order by id")
	if err != nil {
		t.Fatalf("selecting: %v", err)
	}
	if len(res.Rows) != len(values) {
		t.Fatalf("got %d rows, want %d", len(res.Rows), len(values))
	}
	for i, row := range res.Rows {
		if got := Text(row[1]); got != values[i] {
			t.Errorf("row %d round-tripped %q as %q", i, values[i], got)
		}
		if row[1] == nil {
			t.Errorf("row %d: %q came back as SQL NULL", i, values[i])
		}
	}
}

// TestLiveNullIsNotAnEmptyString is the distinction the wire carries as a length
// of -1 versus 0, and the one a caller uses to tell "no value recorded" from
// "recorded as blank".
func TestLiveNullIsNotAnEmptyString(t *testing.T) {
	c := liveConn(t)
	table := liveTable(t, c, "k text, v text")
	ctx := bg(t)

	if _, err := c.Query(ctx, "insert into "+table+" values ($1, $2), ($3, $4)",
		"null", nil, "empty", ""); err != nil {
		t.Fatal(err)
	}

	res, err := c.Query(ctx, "select k, v, v is null from "+table+" order by k")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(res.Rows))
	}

	empty, null := res.Rows[0], res.Rows[1]
	if Text(empty[0]) != "empty" || Text(null[0]) != "null" {
		t.Fatalf("rows came back in an unexpected order: %q, %q", empty[0], null[0])
	}
	if Bool(empty[2]) {
		t.Error("the empty string was stored as NULL, so the parameter went as NULL")
	}
	if !Bool(null[2]) {
		t.Error("the nil parameter was stored as something other than NULL")
	}
	if empty[1] == nil {
		t.Error("an empty text column decoded to nil, which callers read as SQL NULL; " +
			"\"\" and NULL are different facts and the driver has just lost one of them")
	}
	if len(empty[1]) != 0 {
		t.Errorf("the empty string came back as %q", empty[1])
	}
	if null[1] != nil {
		t.Errorf("a NULL column decoded to %q rather than nil", null[1])
	}
}

// TestLiveByteaRoundTrip is the canvas blob path. Every byte value, a NUL-heavy
// blob and one larger than the read buffer, because a snapshot is arbitrary
// binary and a single wrong byte is a corrupted image.
func TestLiveByteaRoundTrip(t *testing.T) {
	c := liveConn(t)
	table := liveTable(t, c, "id int, b bytea")
	ctx := bg(t)

	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	big := make([]byte, 1<<20)
	for i := range big {
		big[i] = byte(i * 7)
	}

	blobs := map[string][]byte{
		"empty":              {},
		"one NUL":            {0x00},
		"every byte value":   all,
		"all zeroes":         bytes.Repeat([]byte{0}, 4096),
		"all high bytes":     bytes.Repeat([]byte{0xff}, 4096),
		"backslashes":        []byte(`\x\\\000`),
		"bigger than a page": big,
	}

	i := 0
	ids := map[int]string{}
	for name, blob := range blobs {
		if _, err := c.Query(ctx, "insert into "+table+" values ($1, $2)", i, blob); err != nil {
			t.Fatalf("inserting %s: %v", name, err)
		}
		ids[i] = name
		i++
	}

	res, err := c.Query(ctx, "select id, b from "+table+" order by id")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != len(blobs) {
		t.Fatalf("got %d rows, want %d", len(res.Rows), len(blobs))
	}
	for _, row := range res.Rows {
		id, err := Int(row[0])
		if err != nil {
			t.Fatal(err)
		}
		name := ids[id]
		got, err := Bytea(row[1])
		if err != nil {
			t.Fatalf("%s: decoding: %v", name, err)
		}
		want := blobs[name]
		if !bytes.Equal(got, want) {
			t.Errorf("%s: round trip changed %d bytes into %d", name, len(want), len(got))
		}
	}
}

// TestLiveNumericBoundaries checks every integer width at its extremes, because
// a driver that formats a parameter one digit short turns a valid write into a
// server-side range error - or worse, a silently different number.
func TestLiveNumericBoundaries(t *testing.T) {
	c := liveConn(t)
	ctx := bg(t)

	cases := []struct {
		name string
		typ  string
		in   any
		want string
	}{
		{"smallint min", "int2", int16(math.MinInt16), "-32768"},
		{"smallint max", "int2", int16(math.MaxInt16), "32767"},
		{"integer min", "int4", int32(math.MinInt32), "-2147483648"},
		{"integer max", "int4", int32(math.MaxInt32), "2147483647"},
		{"bigint min", "int8", int64(math.MinInt64), "-9223372036854775808"},
		{"bigint max", "int8", int64(math.MaxInt64), "9223372036854775807"},
		{"uint32 max as bigint", "int8", uint32(math.MaxUint32), "4294967295"},
		{"uint64 max as numeric", "numeric", uint64(math.MaxUint64), "18446744073709551615"},
		{"zero", "int4", 0, "0"},
		{"a byte", "int2", uint8(255), "255"},
		{"float8", "float8", 1.5, "1.5"},
		{"float8 tiny", "float8", 4.9e-324, "5e-324"},
		{"float8 huge", "float8", 1.7976931348623157e308, "1.7976931348623157e+308"},
		{"float4", "float4", float32(1.5), "1.5"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row, err := c.QueryRow(ctx, "select $1::"+tc.typ, tc.in)
			if err != nil {
				t.Fatalf("the server refused %v as %s: %v", tc.in, tc.typ, err)
			}
			if got := Text(row[0]); got != tc.want {
				t.Errorf("%v as %s came back as %q, want %q", tc.in, tc.typ, got, tc.want)
			}
		})
	}

	// Round-tripping through the decoders too, so a wrong sign or a lost digit
	// shows up as a number rather than a string.
	for _, want := range []int64{math.MinInt64, -1, 0, 1, math.MaxInt64} {
		row, err := c.QueryRow(ctx, "select $1::int8", want)
		if err != nil {
			t.Fatalf("selecting %d: %v", want, err)
		}
		got, err := Int64(row[0])
		if err != nil {
			t.Fatalf("decoding %q: %v", row[0], err)
		}
		if got != want {
			t.Errorf("%d round-tripped as %d", want, got)
		}
	}
}

// TestLiveTimestampRoundTrip covers the coupling between the DateStyle pinned in
// the startup packet and the layouts in values.go, against a server whose
// session timezone has been changed underneath us.
func TestLiveTimestampRoundTrip(t *testing.T) {
	c := liveConn(t)
	ctx := bg(t)

	times := []time.Time{
		time.Date(2026, 8, 15, 19, 2, 26, 265636000, time.UTC),
		time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 6, 1, 12, 0, 0, 500000000, time.FixedZone("IST", 5*3600+1800)),
		time.Date(1999, 12, 31, 23, 59, 59, 999999000, time.UTC),
	}

	// A session timezone the driver did not choose is exactly the situation the
	// DateStyle pin is there to survive.
	for _, zone := range []string{"UTC", "America/New_York", "Asia/Kathmandu"} {
		t.Run(zone, func(t *testing.T) {
			if err := c.Exec(ctx, "set timezone to '"+zone+"'"); err != nil {
				t.Fatal(err)
			}
			for _, want := range times {
				row, err := c.QueryRow(ctx, "select $1::timestamptz", want)
				if err != nil {
					t.Fatalf("selecting %v: %v", want, err)
				}
				got, err := Time(row[0])
				if err != nil {
					t.Fatalf("parsing %q in zone %s: %v", row[0], zone, err)
				}
				// PostgreSQL keeps microseconds, so compare at that resolution.
				if !got.Equal(want.Truncate(time.Microsecond)) {
					t.Errorf("in zone %s, %v round-tripped as %v (raw %q)",
						zone, want, got, row[0])
				}
			}
		})
	}
	if err := c.Exec(ctx, "set timezone to 'UTC'"); err != nil {
		t.Fatal(err)
	}
}

// TestLiveBooleanForms: PostgreSQL emits "t" and "f" in text format, but casts
// and expressions can produce other spellings, and NULL has to stay false.
func TestLiveBooleanRoundTrip(t *testing.T) {
	c := liveConn(t)
	ctx := bg(t)

	for _, tc := range []struct {
		expr string
		want bool
	}{
		{"true", true},
		{"false", false},
		{"1 = 1", true},
		{"1 = 2", false},
		{"'yes'::boolean", true},
		{"'no'::boolean", false},
		{"'1'::boolean", true},
		{"'0'::boolean", false},
		{"null::boolean", false},
	} {
		row, err := c.QueryRow(ctx, "select "+tc.expr)
		if err != nil {
			t.Fatalf("select %s: %v", tc.expr, err)
		}
		if got := Bool(row[0]); got != tc.want {
			t.Errorf("Bool(%s) = %v (raw %q), want %v", tc.expr, got, row[0], tc.want)
		}
	}

	// And a parameter in both directions.
	for _, want := range []bool{true, false} {
		row, err := c.QueryRow(ctx, "select $1::boolean", want)
		if err != nil {
			t.Fatal(err)
		}
		if got := Bool(row[0]); got != want {
			t.Errorf("%v round-tripped as %v", want, got)
		}
	}
}

// TestLiveResultShapes checks the answers a real server gives to the queries
// that return nothing much, since those are the ones where a decoding mistake
// looks like an empty database rather than a bug.
func TestLiveResultShapes(t *testing.T) {
	c := liveConn(t)
	table := liveTable(t, c, "id int")
	ctx := bg(t)

	t.Run("no rows", func(t *testing.T) {
		res, err := c.Query(ctx, "select id from "+table+" where id = $1", 12345)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Rows) != 0 {
			t.Errorf("got %d rows, want none", len(res.Rows))
		}
		if len(res.Columns) != 1 || res.Columns[0] != "id" {
			t.Errorf("columns = %v, want [id]; the description arrives even with no rows",
				res.Columns)
		}
	})

	t.Run("no columns", func(t *testing.T) {
		res, err := c.Query(ctx, "insert into "+table+" values (1)")
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Columns) != 0 {
			t.Errorf("columns = %v, want none", res.Columns)
		}
		if res.Tag != "INSERT 0 1" {
			t.Errorf("Tag = %q, want \"INSERT 0 1\"", res.Tag)
		}
		if res.Affected != 1 {
			t.Errorf("Affected = %d, want 1", res.Affected)
		}
	})

	t.Run("an empty query string", func(t *testing.T) {
		res, err := c.Query(ctx, "")
		if err != nil {
			t.Fatalf("an empty query should be harmless, got %v", err)
		}
		if len(res.Rows) != 0 || len(res.Columns) != 0 {
			t.Errorf("an empty query produced %v / %v", res.Columns, res.Rows)
		}
		if _, err := c.Query(ctx, "select 1"); err != nil {
			t.Errorf("the connection was unusable after an empty query: %v", err)
		}
	})

	t.Run("a query returning many rows", func(t *testing.T) {
		res, err := c.Query(ctx, "select i, repeat('x', 100) from generate_series(1, 5000) i")
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Rows) != 5000 {
			t.Fatalf("got %d rows, want 5000", len(res.Rows))
		}
		// Enough rows to cross the read buffer many times; a framing mistake
		// would show up as a wrong value somewhere in the middle.
		for i, row := range res.Rows {
			n, err := Int(row[0])
			if err != nil || n != i+1 {
				t.Fatalf("row %d holds %q (%v); the message framing slipped", i, row[0], err)
			}
			if len(row[1]) != 100 {
				t.Fatalf("row %d second column is %d bytes, want 100", i, len(row[1]))
			}
		}
	})

	t.Run("a single value larger than the read buffer", func(t *testing.T) {
		row, err := c.QueryRow(ctx, "select repeat('y', $1)::text", 2_000_000)
		if err != nil {
			t.Fatal(err)
		}
		if len(row[0]) != 2_000_000 {
			t.Errorf("got %d bytes, want 2000000; a message spanning many reads was "+
				"truncated", len(row[0]))
		}
	})
}

// TestLiveFailedQueryLeavesTheConnectionUsable against a real server, which is
// the one that decides what state the connection is really in after an error.
func TestLiveFailedQueryLeavesTheConnectionUsable(t *testing.T) {
	c := liveConn(t)
	ctx := bg(t)

	failures := []struct {
		name string
		sql  string
		args []any
		code string
	}{
		{"a table that does not exist", "select * from definitely_not_a_table", nil, "42P01"},
		{"a syntax error", "selct 1", nil, "42601"},
		{"a type error", "select $1::int", []any{"not a number"}, "22P02"},
		{"division by zero", "select 1/0", nil, "22012"},
		{"too few parameters", "select $1, $2", []any{1}, "08P01"},
		{"a column that does not exist", "select nope from (select 1) x", nil, "42703"},
	}

	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Query(ctx, tc.sql, tc.args...)
			if err == nil {
				t.Fatalf("%q was accepted", tc.sql)
			}
			if pgErr, ok := err.(*Error); !ok {
				t.Errorf("error is %T, not *pg.Error: %v", err, err)
			} else if pgErr.SQLState() != tc.code {
				t.Errorf("SQLSTATE = %q, want %q (error: %v)", pgErr.SQLState(), tc.code, err)
			}
			if c.Broken() {
				t.Fatal("a SQL error marked the connection broken; the pool would " +
					"redial on every failed query")
			}

			row, err := c.QueryRow(ctx, "select 42")
			if err != nil {
				t.Fatalf("the connection was unusable after a failed query: %v; the "+
					"exchange left unread bytes on the socket", err)
			}
			if Text(row[0]) != "42" {
				t.Fatalf("the next query returned %q, so it read the failed exchange's "+
					"bytes rather than its own", row[0])
			}
		})
	}
}

// TestLiveTooManyParameters is the other half of the mismatch: extra arguments
// the statement never mentions.
func TestLiveParameterCountMismatch(t *testing.T) {
	c := liveConn(t)
	ctx := bg(t)

	if _, err := c.Query(ctx, "select $1::int", 1, 2, 3); err == nil {
		t.Error("three arguments were accepted for a statement with one placeholder")
	}
	if c.Broken() {
		t.Fatal("a parameter mismatch broke the connection")
	}
	if _, err := c.Query(ctx, "select 1"); err != nil {
		t.Fatalf("the connection was unusable afterwards: %v", err)
	}
}

// TestLiveMultipleStatements: the simple protocol takes several statements in
// one string, the extended protocol does not. Both behaviours matter - Exec runs
// migrations, and Query must not become an injection vector by quietly allowing
// a second statement.
func TestLiveMultipleStatements(t *testing.T) {
	c := liveConn(t)
	table := liveTable(t, c, "id int")
	ctx := bg(t)

	if err := c.Exec(ctx, "insert into "+table+" values (1); insert into "+table+" values (2);"); err != nil {
		t.Fatalf("Exec with two statements: %v", err)
	}
	row, err := c.QueryRow(ctx, "select count(*) from "+table)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := Int64(row[0]); n != 2 {
		t.Errorf("Exec applied %d of 2 statements", n)
	}

	if _, err := c.Query(ctx, "select 1; select 2"); err == nil {
		t.Error("the extended protocol accepted two statements in one Query; a driver " +
			"that allows this turns a single injected semicolon into a second statement")
	}
	if _, err := c.Query(ctx, "select 1"); err != nil {
		t.Fatalf("the connection was unusable afterwards: %v", err)
	}
}

// TestLiveTransactionStatusIsTracked, because 'E' is the difference between "the
// next statement will run" and "the next statement will fail with 25P02 until
// somebody rolls back".
func TestLiveTransactionStatusIsTracked(t *testing.T) {
	c := liveConn(t)
	ctx := bg(t)

	if c.txStatus != 'I' {
		t.Errorf("a fresh connection reports transaction status %q, want 'I'", c.txStatus)
	}
	if err := c.Exec(ctx, "begin"); err != nil {
		t.Fatal(err)
	}
	if c.txStatus != 'T' {
		t.Errorf("inside a transaction the status is %q, want 'T'", c.txStatus)
	}
	if _, err := c.Query(ctx, "select * from definitely_not_a_table"); err == nil {
		t.Fatal("expected the query to fail")
	}
	if c.txStatus != 'E' {
		t.Errorf("after a failed statement inside a transaction the status is %q, "+
			"want 'E'", c.txStatus)
	}
	if err := c.Exec(ctx, "rollback"); err != nil {
		t.Fatal(err)
	}
	if c.txStatus != 'I' {
		t.Errorf("after rollback the status is %q, want 'I'", c.txStatus)
	}
}

// TestLiveServerKillsTheConnection is the real version of the fake server's
// mid-query hangup: PostgreSQL terminating a backend, which is what happens
// during a failover, a restart, or an administrator clearing a stuck query.
func TestLiveServerKillsTheConnection(t *testing.T) {
	victim := liveConn(t)
	killer := liveConn(t)
	ctx := bg(t)

	if victim.pid == 0 {
		t.Fatal("the server sent no BackendKeyData, so there is no pid to terminate")
	}

	done := make(chan error, 1)
	go func() {
		_, err := victim.Query(ctx, "select pg_sleep(30)")
		done <- err
	}()

	// Wait for the backend to actually be running the sleep before killing it,
	// rather than sleeping and hoping.
	waitFor(t, 20*time.Second, "the victim backend to start its query", func() bool {
		row, err := killer.QueryRow(ctx,
			"select count(*) from pg_stat_activity where pid = $1 and state = 'active'", victim.pid)
		if err != nil || row == nil {
			return false
		}
		n, _ := Int64(row[0])
		return n == 1
	})

	if _, err := killer.Query(ctx, "select pg_terminate_backend($1)", victim.pid); err != nil {
		t.Fatalf("terminating the victim backend: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the query reported success although the server terminated the " +
				"backend running it")
		}
	case <-time.After(25 * time.Second):
		t.Fatal("the query never returned after its backend was terminated")
	}

	// Either the server managed to send a FATAL first or the socket simply died;
	// either way the connection must not be reused.
	if !victim.Broken() && victim.txStatus != 0 {
		if _, err := victim.Query(ctx, "select 1"); err == nil {
			t.Error("a connection whose backend was terminated still answers queries")
		}
	}
	if _, err := killer.Query(ctx, "select 1"); err != nil {
		t.Errorf("the killer's own connection was affected: %v", err)
	}
}

// TestLiveErrorAfterSomeRowsHaveArrived: a statement timeout firing part-way
// through a large result is how a real server produces the "rows then error"
// sequence. The rows must not escape as a partial answer.
func TestLiveErrorMidResult(t *testing.T) {
	c := liveConn(t)
	ctx := bg(t)

	if err := c.Exec(ctx, "set statement_timeout to '150ms'"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.Exec(ctx, "set statement_timeout to 0")
	})

	res, err := c.Query(ctx, "select i, pg_sleep(0.01) from generate_series(1, 1000) i")
	if err == nil {
		t.Fatalf("the statement timeout did not fire; got %d rows", len(res.Rows))
	}
	if res != nil {
		t.Errorf("a partial result escaped alongside the error: %d rows", len(res.Rows))
	}
	if pgErr, ok := err.(*Error); !ok {
		t.Errorf("error is %T, want *pg.Error: %v", err, err)
	} else if pgErr.SQLState() != "57014" {
		t.Errorf("SQLSTATE = %q, want 57014 (query cancelled)", pgErr.SQLState())
	}
	if c.Broken() {
		t.Fatal("a statement timeout broke the connection")
	}
	if err := c.Exec(ctx, "set statement_timeout to 0"); err != nil {
		t.Fatalf("the connection was unusable after a cancelled query: %v", err)
	}
	if _, err := c.Query(ctx, "select 1"); err != nil {
		t.Fatalf("the connection was unusable after a cancelled query: %v", err)
	}
}

// TestLiveWrongPasswordIsRejected exercises SCRAM against a real server in the
// direction that matters: the server must be able to tell us no, and we must
// report it as 28P01 rather than as a connection failure.
func TestLiveWrongPasswordIsRejected(t *testing.T) {
	cfg := liveConfig(t)
	cfg.Password += "-definitely-wrong"

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Connect(ctx, cfg)
	if err == nil {
		_ = c.Close()
		t.Fatal("connected with the wrong password")
	}
	pgErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error is %T (%v), want *pg.Error so callers can tell bad credentials "+
			"from an unreachable host", err, err)
	}
	if pgErr.SQLState() != "28P01" {
		t.Errorf("SQLSTATE = %q, want 28P01", pgErr.SQLState())
	}
}

// TestLiveUnknownDatabaseIsRejected is the other startup failure operators hit,
// and it must not look the same as a network problem.
func TestLiveUnknownDatabaseIsRejected(t *testing.T) {
	cfg := liveConfig(t)
	cfg.Database = "pf_pg_does_not_exist"

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Connect(ctx, cfg)
	if err == nil {
		_ = c.Close()
		t.Fatal("connected to a database that does not exist")
	}
	if pgErr, ok := err.(*Error); !ok {
		t.Errorf("error is %T (%v), want *pg.Error", err, err)
	} else if pgErr.SQLState() != "3D000" {
		t.Errorf("SQLSTATE = %q, want 3D000", pgErr.SQLState())
	}
}

// TestLivePoolUnderConcurrency runs the pool against a real server, where each
// checkout is a real round trip and the timings are nothing like the fake's.
// Under -race this is the closest thing to production the tests get.
func TestLivePoolUnderConcurrency(t *testing.T) {
	cfg := liveConfig(t)
	// The other packages' database-backed tests run against the same database
	// under the same default application_name, so the connection count below has
	// to be able to pick this pool's connections out from theirs.
	cfg.ApplicationName = fmt.Sprintf("pgtest-pool-%d", time.Now().UnixNano())
	const (
		size    = 4
		workers = 16
		rounds  = 10
	)
	p := NewPool(cfg, size, quietLogger())
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := p.WaitReady(ctx, 3); err != nil {
		t.Fatalf("the test database is not reachable: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, workers*rounds)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < rounds; j++ {
				row, err := p.QueryRow(ctx, "select $1::int + $2::int", worker, j)
				if err != nil {
					errs <- err
					continue
				}
				got, err := Int(row[0])
				if err != nil {
					errs <- err
					continue
				}
				if got != worker+j {
					errs <- fmt.Errorf("got %d, want %d: a result was delivered to the "+
						"wrong caller", got, worker+j)
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("pooled query under concurrency: %v", err)
	}

	// The server's own view of how many connections we opened is the check that
	// cannot be fooled by the driver's bookkeeping.
	row, err := p.QueryRow(ctx,
		"select count(*) from pg_stat_activity where application_name = $1 and datname = current_database()",
		cfg.ApplicationName)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := Int64(row[0]); n > int64(size) {
		t.Errorf("the server sees %d connections from this pool of %d", n, size)
	}
}

// TestLivePoolSurvivesItsConnectionsBeingKilled: a failover kills every backend
// at once, and the pool has to notice and redial rather than serving the dead
// ones to the next callers.
func TestLivePoolSurvivesItsConnectionsBeingKilled(t *testing.T) {
	cfg := liveConfig(t)
	cfg.ApplicationName = fmt.Sprintf("pgtest-kill-%d", time.Now().UnixNano())

	p := NewPool(cfg, 3, quietLogger())
	defer p.Close()
	ctx := bg(t)

	// Fill the pool and put everything back so the connections are idle.
	held := make([]*Conn, 0, 3)
	for i := 0; i < 3; i++ {
		c, err := p.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquiring %d: %v", i, err)
		}
		held = append(held, c)
	}
	for _, c := range held {
		p.Release(c)
	}

	killer := liveConn(t)
	if _, err := killer.Query(ctx,
		`select pg_terminate_backend(pid) from pg_stat_activity
		  where application_name = $1 and pid <> pg_backend_pid()`, cfg.ApplicationName); err != nil {
		t.Fatalf("terminating the pool's backends: %v", err)
	}

	// Every checkout must now succeed on a fresh connection.
	for i := 0; i < 6; i++ {
		row, err := p.QueryRow(ctx, "select 1")
		if err != nil {
			t.Fatalf("query %d after the backends were killed: %v; the pool handed out a "+
				"connection the server had already terminated", i, err)
		}
		if Text(row[0]) != "1" {
			t.Fatalf("query %d returned %q", i, row[0])
		}
	}
}
