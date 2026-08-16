package pg

import (
	"context"
	"strings"
	"testing"
	"time"
)

func bg(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestQueryReportsTheServerVanishingMidResult is the single most important
// behaviour in this file. A driver that hands back a nil error and an empty
// Result when the socket died mid-answer is worse than one that panics: the
// caller writes "0 rooms" into a cache, or treats an interrupted read as an
// authoritative "no such row", and the data loss is silent and permanent.
func TestQueryReportsTheServerVanishingMidResult(t *testing.T) {
	cases := []struct {
		name  string
		where string
		reply func(*fakeConn)
	}{
		{
			name:  "before any reply at all",
			where: "the server never answered the query",
			reply: func(f *fakeConn) { _ = f.Close() },
		},
		{
			name:  "after ParseComplete",
			where: "the server died between Parse and Bind",
			reply: func(f *fakeConn) {
				f.send(msgParseComplete, func(*writeBuf) {})
				_ = f.Close()
			},
		},
		{
			name:  "between RowDescription and the first DataRow",
			where: "the server described the columns and then died",
			reply: func(f *fakeConn) {
				f.send(msgParseComplete, func(*writeBuf) {})
				f.send(msgBindComplete, func(*writeBuf) {})
				f.rowDescription("id", "slug")
				_ = f.Close()
			},
		},
		{
			name:  "between two DataRows",
			where: "the server delivered two of an unknown number of rows and died",
			reply: func(f *fakeConn) {
				f.send(msgParseComplete, func(*writeBuf) {})
				f.send(msgBindComplete, func(*writeBuf) {})
				f.rowDescription("id", "slug")
				f.dataRow([]byte("1"), []byte("alpha"))
				f.dataRow([]byte("2"), []byte("beta"))
				_ = f.Close()
			},
		},
		{
			name:  "in the middle of a DataRow",
			where: "a DataRow was cut in half by the socket closing",
			reply: func(f *fakeConn) {
				f.send(msgParseComplete, func(*writeBuf) {})
				f.send(msgBindComplete, func(*writeBuf) {})
				f.rowDescription("id")
				full := msg(msgDataRow, func(w *writeBuf) {
					w.int16(1)
					w.int32(5)
					w.raw([]byte("hello"))
				})
				f.write(full[:len(full)-3])
				_ = f.Close()
			},
		},
		{
			name:  "after CommandComplete but before ReadyForQuery",
			where: "the result was complete but the server never went idle",
			reply: func(f *fakeConn) {
				f.send(msgParseComplete, func(*writeBuf) {})
				f.send(msgBindComplete, func(*writeBuf) {})
				f.rowDescription("id")
				f.dataRow([]byte("1"))
				f.commandComplete("SELECT 1")
				_ = f.Close()
			},
		},
		{
			name:  "inside ReadyForQuery",
			where: "ReadyForQuery lost its status byte",
			reply: func(f *fakeConn) {
				f.send(msgParseComplete, func(*writeBuf) {})
				f.send(msgBindComplete, func(*writeBuf) {})
				f.commandComplete("SELECT 0")
				f.write(header(msgReadyForQuery, 5))
				_ = f.Close()
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeServer(t, scripted(tc.reply))
			c := srv.dial(t)

			res, err := c.Query(bg(t), "select id, slug from rooms")
			if err == nil {
				t.Fatalf("Query returned no error although %s; it returned %d rows, which "+
					"the caller will believe is the whole answer", tc.where, len(res.Rows))
			}
			if res != nil {
				t.Errorf("Query returned a non-nil Result alongside the error %v; a partial "+
					"result must not be reachable", err)
			}
			if !c.Broken() {
				t.Error("Broken() is false after the socket died, so the pool will hand " +
					"this connection to the next caller instead of redialling")
			}
		})
	}
}

// TestExecReportsTheServerVanishing is the same contract for the simple query
// protocol, which is what migrations run through. A migration that silently
// "succeeds" against a dead socket leaves the schema half-applied.
func TestExecReportsTheServerVanishing(t *testing.T) {
	srv := newFakeServer(t, scripted(func(f *fakeConn) {
		f.commandComplete("CREATE TABLE")
		_ = f.Close()
	}))
	c := srv.dial(t)

	if err := c.Exec(bg(t), "create table t (id int)"); err == nil {
		t.Fatal("Exec reported success although the server died before ReadyForQuery")
	}
	if !c.Broken() {
		t.Error("Broken() is false after Exec hit a dead socket")
	}
}

// TestQueryRejectsMalformedServerMessages feeds the read loop the message
// bodies a real PostgreSQL never produces. The bar is that every one is either
// a returned error or a correctly decoded result - never a panic, and never a
// plausible-looking answer built from garbage.
func TestQueryRejectsMalformedServerMessages(t *testing.T) {
	cases := []struct {
		name      string
		why       string
		wantErr   bool
		wantInErr string
		reply     func(*fakeConn)
	}{
		{
			name:    "RowDescription claims more columns than it carries",
			why:     "the count says four columns, the body holds one",
			wantErr: true, wantInErr: "truncated",
			reply: func(f *fakeConn) {
				f.send(msgParseComplete, func(*writeBuf) {})
				f.send(msgBindComplete, func(*writeBuf) {})
				f.send(msgRowDescription, func(w *writeBuf) {
					w.int16(4)
					w.string("only_one")
					w.int32(0)
					w.int16(0)
					w.int32(25)
					w.int16(-1)
					w.int32(-1)
					w.int16(0)
				})
				f.commandComplete("SELECT 0")
				f.ready('I')
			},
		},
		{
			name:    "a column name with no NUL terminator",
			why:     "the last C string in RowDescription runs off the end of the body",
			wantErr: true, wantInErr: "truncated",
			reply: func(f *fakeConn) {
				f.send(msgParseComplete, func(*writeBuf) {})
				f.send(msgBindComplete, func(*writeBuf) {})
				f.send(msgRowDescription, func(w *writeBuf) {
					w.int16(1)
					w.raw([]byte("no_terminator"))
				})
				f.commandComplete("SELECT 0")
				f.ready('I')
			},
		},
		{
			name:    "a DataRow field longer than the message that carries it",
			why:     "a field claims 4096 bytes inside a body of 6",
			wantErr: true, wantInErr: "truncated",
			reply: func(f *fakeConn) {
				f.send(msgParseComplete, func(*writeBuf) {})
				f.send(msgBindComplete, func(*writeBuf) {})
				f.rowDescription("id")
				f.send(msgDataRow, func(w *writeBuf) {
					w.int16(1)
					w.int32(4096)
					w.raw([]byte("ab"))
				})
				f.commandComplete("SELECT 1")
				f.ready('I')
			},
		},
		{
			name:    "a DataRow with fewer fields than the RowDescription",
			why:     "the row description promised three columns and the row has one",
			wantErr: true, wantInErr: "field",
			reply: func(f *fakeConn) {
				f.send(msgParseComplete, func(*writeBuf) {})
				f.send(msgBindComplete, func(*writeBuf) {})
				f.rowDescription("a", "b", "c")
				f.dataRow([]byte("1"))
				f.commandComplete("SELECT 1")
				f.ready('I')
			},
		},
		{
			name:    "a DataRow with more fields than the RowDescription",
			why:     "the row description promised one column and the row has three",
			wantErr: true, wantInErr: "field",
			reply: func(f *fakeConn) {
				f.send(msgParseComplete, func(*writeBuf) {})
				f.send(msgBindComplete, func(*writeBuf) {})
				f.rowDescription("a")
				f.dataRow([]byte("1"), []byte("2"), []byte("3"))
				f.commandComplete("SELECT 1")
				f.ready('I')
			},
		},
		{
			name:    "a message type that means nothing here",
			why:     "the server sent an Authentication message mid-result",
			wantErr: true, wantInErr: "unexpected message",
			reply: func(f *fakeConn) {
				f.send(msgParseComplete, func(*writeBuf) {})
				f.send(msgAuth, func(w *writeBuf) { w.int32(authOK) })
				f.ready('I')
			},
		},
		{
			name:    "a message bigger than the driver will ever allocate",
			why:     "the server claimed a 512 MiB DataRow",
			wantErr: true, wantInErr: "exceeds limit",
			reply: func(f *fakeConn) {
				f.send(msgParseComplete, func(*writeBuf) {})
				f.send(msgBindComplete, func(*writeBuf) {})
				f.rowDescription("id")
				f.write(header(msgDataRow, 512<<20))
				_ = f.Close()
			},
		},
		{
			name:    "COPY, which this driver does not implement",
			why:     "the server started a COPY stream we cannot follow",
			wantErr: true, wantInErr: "COPY",
			reply: func(f *fakeConn) {
				f.send(msgParseComplete, func(*writeBuf) {})
				f.send(msgCopyOutResponse, func(w *writeBuf) { w.byte(0); w.int16(0) })
			},
		},
		{
			name:    "a bogus message length",
			why:     "the length prefix says two bytes, which is less than the prefix itself",
			wantErr: true, wantInErr: "bogus message length",
			reply: func(f *fakeConn) {
				f.write(header(msgDataRow, 2))
				_ = f.Close()
			},
		},
		{
			name:    "a ReadyForQuery with no status byte",
			why:     "the transaction status is missing",
			wantErr: false,
			reply: func(f *fakeConn) {
				f.send(msgParseComplete, func(*writeBuf) {})
				f.send(msgBindComplete, func(*writeBuf) {})
				f.commandComplete("SELECT 0")
				f.send(msgReadyForQuery, func(*writeBuf) {})
			},
		},
		{
			name:    "a RowDescription with a negative column count",
			why:     "the count is -1, which must not become a negative make() or a huge loop",
			wantErr: false,
			reply: func(f *fakeConn) {
				f.send(msgParseComplete, func(*writeBuf) {})
				f.send(msgBindComplete, func(*writeBuf) {})
				f.send(msgRowDescription, func(w *writeBuf) { w.int16(-1) })
				f.commandComplete("SELECT 0")
				f.ready('I')
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeServer(t, scripted(tc.reply))
			c := srv.dial(t)

			// A panic here is the finding, so let it be reported as one rather
			// than as an unexplained test binary crash.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("the driver panicked on server input where %s: %v", tc.why, r)
				}
			}()

			res, err := c.Query(bg(t), "select 1")
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("no error although %s; the caller receives %d rows built from "+
					"bytes the driver could not actually decode", tc.why, len(res.Rows))
			case tc.wantErr:
				if !strings.Contains(err.Error(), tc.wantInErr) {
					t.Errorf("error %q does not mention %q", err, tc.wantInErr)
				}
			case err != nil:
				t.Fatalf("unexpected error where %s: %v", tc.why, err)
			}
		})
	}
}

// TestQueryHandlesHostileValues checks the bytes inside a column rather than the
// framing around it. Text columns are handed to callers verbatim, so the driver
// must not assume they are valid UTF-8 or free of NULs - a bytea column read
// back as text contains arbitrary bytes by definition.
func TestQueryHandlesHostileValues(t *testing.T) {
	values := [][]byte{
		[]byte(""),                      // empty string, which is not NULL
		nil,                             // NULL
		{0xff, 0xfe, 0xfd},              // invalid UTF-8
		[]byte("before\x00after"),       // an embedded NUL
		[]byte("quote' backslash\\ %s"), // characters that matter to a formatter
		{0x00},                          // a lone NUL
	}
	srv := newFakeServer(t, scripted(func(f *fakeConn) {
		f.send(msgParseComplete, func(*writeBuf) {})
		f.send(msgBindComplete, func(*writeBuf) {})
		f.rowDescription("a", "b", "c", "d", "e", "f")
		f.dataRow(values...)
		f.commandComplete("SELECT 1")
		f.ready('I')
	}))
	c := srv.dial(t)

	res, err := c.Query(bg(t), "select stuff")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(res.Rows))
	}
	row := res.Rows[0]
	if len(row) != len(values) {
		t.Fatalf("got %d fields, want %d", len(row), len(values))
	}
	for i, want := range values {
		got := row[i]
		if want == nil {
			if got != nil {
				t.Errorf("field %d: NULL came back as %q; a caller cannot tell it from an "+
					"empty string any more", i, got)
			}
			continue
		}
		if got == nil {
			t.Errorf("field %d: %q came back as NULL", i, want)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("field %d: got %q, want %q", i, got, want)
		}
	}
	// An empty string and SQL NULL are different facts about the world, and the
	// only thing carrying the difference on the wire is a length of 0 versus -1.
	if res.Rows[0][0] == nil {
		t.Error("an empty string was decoded as NULL")
	}
}

// TestQueryResultShapes covers the answers that are legal but easy to get wrong
// because they carry no rows to notice.
func TestQueryResultShapes(t *testing.T) {
	cases := []struct {
		name     string
		reply    func(*fakeConn)
		wantCols []string
		wantRows int
		wantTag  string
		wantAff  int64
	}{
		{
			name: "a select matching nothing",
			reply: func(f *fakeConn) {
				f.send(msgParseComplete, func(*writeBuf) {})
				f.send(msgBindComplete, func(*writeBuf) {})
				f.rowDescription("id", "slug")
				f.commandComplete("SELECT 0")
				f.ready('I')
			},
			wantCols: []string{"id", "slug"}, wantRows: 0, wantTag: "SELECT 0",
		},
		{
			name: "a statement with no result columns at all",
			reply: func(f *fakeConn) {
				f.send(msgParseComplete, func(*writeBuf) {})
				f.send(msgBindComplete, func(*writeBuf) {})
				f.send(msgNoData, func(*writeBuf) {})
				f.commandComplete("UPDATE 7")
				f.ready('I')
			},
			wantCols: nil, wantRows: 0, wantTag: "UPDATE 7", wantAff: 7,
		},
		{
			name: "an empty query string",
			reply: func(f *fakeConn) {
				f.send(msgParseComplete, func(*writeBuf) {})
				f.send(msgBindComplete, func(*writeBuf) {})
				f.send(msgEmptyQuery, func(*writeBuf) {})
				f.ready('I')
			},
			wantCols: nil, wantRows: 0, wantTag: "",
		},
		{
			name: "notices and parameter changes arriving mid-result",
			reply: func(f *fakeConn) {
				f.send(msgParseComplete, func(*writeBuf) {})
				f.send(msgBindComplete, func(*writeBuf) {})
				f.rowDescription("id")
				f.send(msgNoticeResponse, func(w *writeBuf) {
					w.byte('S')
					w.string("NOTICE")
					w.byte('M')
					w.string("table will be created")
					w.byte(0)
				})
				f.dataRow([]byte("1"))
				f.send(msgParameterStatus, func(w *writeBuf) {
					w.string("TimeZone")
					w.string("UTC")
				})
				f.send(msgNotification, func(w *writeBuf) {
					w.int32(4242)
					w.string("chan")
					w.string("payload")
				})
				f.dataRow([]byte("2"))
				f.commandComplete("SELECT 2")
				f.ready('I')
			},
			wantCols: []string{"id"}, wantRows: 2, wantTag: "SELECT 2", wantAff: 2,
		},
		{
			name: "a portal that suspended before finishing",
			reply: func(f *fakeConn) {
				f.send(msgParseComplete, func(*writeBuf) {})
				f.send(msgBindComplete, func(*writeBuf) {})
				f.rowDescription("id")
				f.dataRow([]byte("1"))
				f.send(msgPortalSuspended, func(*writeBuf) {})
				f.ready('I')
			},
			wantCols: []string{"id"}, wantRows: 1, wantTag: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeServer(t, scripted(tc.reply))
			c := srv.dial(t)

			res, err := c.Query(bg(t), "select 1")
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if len(res.Columns) != len(tc.wantCols) {
				t.Errorf("got %d columns %v, want %d %v",
					len(res.Columns), res.Columns, len(tc.wantCols), tc.wantCols)
			}
			for i, want := range tc.wantCols {
				if i < len(res.Columns) && res.Columns[i] != want {
					t.Errorf("column %d = %q, want %q", i, res.Columns[i], want)
				}
			}
			if len(res.Rows) != tc.wantRows {
				t.Errorf("got %d rows, want %d", len(res.Rows), tc.wantRows)
			}
			if res.Tag != tc.wantTag {
				t.Errorf("Tag = %q, want %q", res.Tag, tc.wantTag)
			}
			if res.Affected != tc.wantAff {
				t.Errorf("Affected = %d, want %d", res.Affected, tc.wantAff)
			}
		})
	}
}

// TestFailedQueryLeavesTheConnectionUsable is the nastiest bug shape this
// package could have: a query that errors leaves unread bytes on the socket, so
// the *next* query reads someone else's answer. That failure is intermittent,
// looks like data corruption rather than a driver bug, and only shows up under
// load once an error has occurred.
func TestFailedQueryLeavesTheConnectionUsable(t *testing.T) {
	failures := map[string]func(*fakeConn){
		"an error before any row": func(f *fakeConn) {
			f.send(msgParseComplete, func(*writeBuf) {})
			f.errorResponse("ERROR", "42P01", `relation "nope" does not exist`)
			f.ready('I')
		},
		"an error after some rows have arrived": func(f *fakeConn) {
			f.send(msgParseComplete, func(*writeBuf) {})
			f.send(msgBindComplete, func(*writeBuf) {})
			f.rowDescription("id")
			f.dataRow([]byte("1"))
			f.dataRow([]byte("2"))
			f.errorResponse("ERROR", "57014", "canceling statement due to statement timeout")
			f.ready('I')
		},
		"two errors in one exchange": func(f *fakeConn) {
			f.send(msgParseComplete, func(*writeBuf) {})
			f.errorResponse("ERROR", "23505", "duplicate key value violates unique constraint")
			f.errorResponse("ERROR", "25P02", "current transaction is aborted")
			f.ready('I')
		},
		"an error leaving a failed transaction": func(f *fakeConn) {
			f.send(msgParseComplete, func(*writeBuf) {})
			f.errorResponse("ERROR", "23503", "insert violates foreign key constraint")
			f.ready('E')
		},
	}

	for name, failure := range failures {
		t.Run(name, func(t *testing.T) {
			srv := newFakeServer(t, scripted(failure, func(f *fakeConn) {
				f.send(msgParseComplete, func(*writeBuf) {})
				f.send(msgBindComplete, func(*writeBuf) {})
				f.rowDescription("answer")
				f.dataRow([]byte("42"))
				f.commandComplete("SELECT 1")
				f.ready('I')
			}))
			c := srv.dial(t)

			res, err := c.Query(bg(t), "select * from nope")
			if err == nil {
				t.Fatal("the server sent an ErrorResponse and Query returned no error")
			}
			if res != nil {
				t.Errorf("Query returned rows alongside the error %v", err)
			}
			if c.Broken() {
				t.Fatal("a server-side SQL error marked the connection broken; the pool " +
					"would throw away a perfectly good connection on every failed query")
			}

			// The whole point: the next query must get its own answer.
			res, err = c.Query(bg(t), "select 42")
			if err != nil {
				t.Fatalf("the query after a failed one errored: %v; the connection was "+
					"left with unread bytes on it", err)
			}
			if len(res.Rows) != 1 || len(res.Rows[0]) != 1 || string(res.Rows[0][0]) != "42" {
				t.Fatalf("the query after a failed one returned %v; it read the previous "+
					"exchange's bytes instead of its own", res.Rows)
			}
		})
	}
}

// TestQueryErrorCarriesSQLState checks that a mid-result error is decoded rather
// than flattened to a string, because the store distinguishes 23505 from
// everything else to turn a duplicate slug into a user-facing message.
func TestQueryErrorCarriesSQLState(t *testing.T) {
	srv := newFakeServer(t, scripted(func(f *fakeConn) {
		f.send(msgParseComplete, func(*writeBuf) {})
		f.errorResponse("ERROR", "23505", "duplicate key value violates unique constraint")
		f.ready('I')
	}))
	c := srv.dial(t)

	_, err := c.Query(bg(t), "insert into rooms ...")
	if err == nil {
		t.Fatal("no error")
	}
	pgErr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error is %T, not *pg.Error; callers cannot reach SQLSTATE", err)
	}
	if pgErr.SQLState() != "23505" {
		t.Errorf("SQLSTATE = %q, want 23505", pgErr.SQLState())
	}
}

// TestBindSendsParametersAsTheServerExpects decodes the Bind message the driver
// actually put on the wire. Encoding is tested in isolation elsewhere; this is
// about the framing around it, and in particular that a nil argument becomes the
// -1 length that means NULL rather than a zero-length string.
func TestBindSendsParametersAsTheServerExpects(t *testing.T) {
	got := make(chan [][]byte, 1)
	srv := newFakeServer(t, func(f *fakeConn) {
		f.acceptStartup()
		if _, _, err := f.readMsg(); err != nil {
			return
		}
		_, body, err := f.readMsg()
		if err != nil {
			f.t.Errorf("fake server: reading Bind: %v", err)
			return
		}
		r := &readBuf{b: body}
		r.string() // portal
		r.string() // statement
		for i, n := 0, int(r.int16()); i < n; i++ {
			r.int16() // parameter format codes
		}
		params := make([][]byte, 0, 4)
		for i, n := 0, int(r.int16()); i < n; i++ {
			size := int(r.int32())
			if size < 0 {
				params = append(params, nil)
				continue
			}
			params = append(params, append([]byte(nil), r.next(size)...))
		}
		if r.err != nil {
			f.t.Errorf("fake server: decoding Bind: %v", r.err)
		}
		got <- params
		if !f.drainUntil('S') {
			return
		}
		emptyResult(f)
	})
	c := srv.dial(t)

	if _, err := c.Query(bg(t), "select $1, $2, $3, $4", "hello", nil, 42, []byte{0x00, 0xff}); err != nil {
		t.Fatalf("Query: %v", err)
	}

	params := <-got
	want := [][]byte{[]byte("hello"), nil, []byte("42"), []byte(`\x00ff`)}
	if len(params) != len(want) {
		t.Fatalf("Bind carried %d parameters, want %d", len(params), len(want))
	}
	for i := range want {
		if want[i] == nil {
			if params[i] != nil {
				t.Errorf("parameter %d: nil was sent as %q rather than the -1 length that "+
					"means SQL NULL, so the column gets an empty string instead", i, params[i])
			}
			continue
		}
		if string(params[i]) != string(want[i]) {
			t.Errorf("parameter %d = %q, want %q", i, params[i], want[i])
		}
	}
}

// TestQueryRejectsUnencodableParameterBeforeTouchingTheSocket matters because a
// half-sent Parse/Bind sequence would desynchronise the connection: the server
// would still be waiting for a Sync that never comes.
func TestQueryRejectsUnencodableParameterBeforeTouchingTheSocket(t *testing.T) {
	srv := newFakeServer(t, scripted(emptyResult))
	c := srv.dial(t)

	_, err := c.Query(bg(t), "select $1", make(chan int))
	if err == nil {
		t.Fatal("a parameter of an unsupported type was accepted")
	}
	if !strings.Contains(err.Error(), "$1") {
		t.Errorf("error %q does not say which parameter was at fault", err)
	}
	if c.Broken() {
		t.Fatal("rejecting a parameter broke the connection")
	}
	if _, err := c.Query(bg(t), "select 1"); err != nil {
		t.Fatalf("the connection was unusable after a rejected parameter: %v; nothing "+
			"should have reached the socket", err)
	}
}

// TestQueryRowOnAnEmptyResult pins the nil-means-no-row contract that every
// caller in the store depends on to return ErrNotFound.
func TestQueryRowDistinguishesNoRowFromAnEmptyRow(t *testing.T) {
	srv := newFakeServer(t, scripted(
		func(f *fakeConn) {
			f.send(msgParseComplete, func(*writeBuf) {})
			f.send(msgBindComplete, func(*writeBuf) {})
			f.rowDescription("id")
			f.commandComplete("SELECT 0")
			f.ready('I')
		},
		func(f *fakeConn) {
			f.send(msgParseComplete, func(*writeBuf) {})
			f.send(msgBindComplete, func(*writeBuf) {})
			f.rowDescription("id")
			f.dataRow(nil)
			f.commandComplete("SELECT 1")
			f.ready('I')
		},
	))
	c := srv.dial(t)

	row, err := c.QueryRow(bg(t), "select id from rooms where false")
	if err != nil {
		t.Fatalf("QueryRow: %v", err)
	}
	if row != nil {
		t.Errorf("a result with no rows gave QueryRow %v, want nil so callers can "+
			"report ErrNotFound", row)
	}

	row, err = c.QueryRow(bg(t), "select null")
	if err != nil {
		t.Fatalf("QueryRow: %v", err)
	}
	if row == nil {
		t.Fatal("a row holding a single NULL was reported as no row at all, which turns " +
			"a real record into ErrNotFound")
	}
	if len(row) != 1 || row[0] != nil {
		t.Errorf("got %v, want one NULL field", row)
	}
}

// TestExecIgnoresRowsButNotErrors: Exec is used for migrations, which sometimes
// return rows from a DO block or a select. Those rows are discarded, but an
// error in the same exchange still has to come back.
func TestExecIgnoresRowsButNotErrors(t *testing.T) {
	srv := newFakeServer(t, scripted(
		func(f *fakeConn) {
			f.rowDescription("a")
			f.dataRow([]byte("1"))
			f.commandComplete("SELECT 1")
			f.ready('I')
		},
		func(f *fakeConn) {
			f.rowDescription("a")
			f.dataRow([]byte("1"))
			f.errorResponse("ERROR", "42601", "syntax error at or near \"selct\"")
			f.ready('I')
		},
	))
	c := srv.dial(t)

	if err := c.Exec(bg(t), "select 1"); err != nil {
		t.Fatalf("Exec over a row-returning statement: %v", err)
	}
	if err := c.Exec(bg(t), "selct 1"); err == nil {
		t.Fatal("Exec swallowed an ErrorResponse; a migration would be reported as applied")
	}
}

// TestQueryHonoursContextCancellation checks that a caller who gives up is not
// left waiting on a server that never answers. Without it a slow query pins a
// request goroutine and a pool slot until the 30 second fallback deadline.
func TestQueryHonoursContextCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := newFakeServer(t, scripted(func(f *fakeConn) {
		<-release
		emptyResult(f)
	}))
	t.Cleanup(func() { close(release) })
	c := srv.dial(t)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.Query(ctx, "select pg_sleep(60)")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Query returned success although its context expired first")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Query ignored its context deadline and was still blocked 10s later")
	}
}
