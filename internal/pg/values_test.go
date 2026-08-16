package pg

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"
)

// TestEncodeParamEveryType walks the type switch. These bytes go straight into
// a Bind message, so an encoding that is merely plausible - "+Inf" where the
// server wants "Infinity" - fails at the server with an error that names the
// column rather than the driver.
func TestEncodeParamEveryType(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
		null bool
	}{
		{name: "nil", in: nil, null: true},
		{name: "a nil []byte", in: []byte(nil), null: true},

		{name: "empty string", in: "", want: ""},
		{name: "plain string", in: "hello", want: "hello"},
		{name: "a string with a single quote", in: "it's", want: "it's"},
		{name: "a string with a backslash", in: `back\slash`, want: `back\slash`},
		{name: "a string with a NUL", in: "a\x00b", want: "a\x00b"},
		{name: "a string with invalid UTF-8", in: "\xff\xfe", want: "\xff\xfe"},
		{name: "a multibyte string", in: "canvas ✏️", want: "canvas ✏️"},

		// An empty but non-nil []byte is an empty bytea, which is a different
		// value from SQL NULL. Collapsing the two loses a real distinction.
		{name: "empty bytea", in: []byte{}, want: `\x`},
		{name: "bytea with a NUL", in: []byte{0x00}, want: `\x00`},
		{name: "bytea with high bytes", in: []byte{0xde, 0xad, 0xbe, 0xef}, want: `\xdeadbeef`},

		{name: "true", in: true, want: "t"},
		{name: "false", in: false, want: "f"},

		{name: "int zero", in: 0, want: "0"},
		{name: "int negative", in: -7, want: "-7"},
		{name: "int8 min", in: int8(math.MinInt8), want: "-128"},
		{name: "int8 max", in: int8(math.MaxInt8), want: "127"},
		{name: "int16 min", in: int16(math.MinInt16), want: "-32768"},
		{name: "int16 max", in: int16(math.MaxInt16), want: "32767"},
		{name: "int32 min", in: int32(math.MinInt32), want: "-2147483648"},
		{name: "int32 max", in: int32(math.MaxInt32), want: "2147483647"},
		{name: "int64 min", in: int64(math.MinInt64), want: "-9223372036854775808"},
		{name: "int64 max", in: int64(math.MaxInt64), want: "9223372036854775807"},
		{name: "uint8 max", in: uint8(math.MaxUint8), want: "255"},
		{name: "uint16 max", in: uint16(math.MaxUint16), want: "65535"},
		{name: "uint32 max", in: uint32(math.MaxUint32), want: "4294967295"},
		{name: "uint64 max", in: uint64(math.MaxUint64), want: "18446744073709551615"},

		{name: "float64 zero", in: 0.0, want: "0"},
		{name: "float64 fraction", in: 0.5, want: "0.5"},
		{name: "float64 negative", in: -1.25, want: "-1.25"},
		{name: "float32", in: float32(1.5), want: "1.5"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := encodeParam(tc.in)
			if err != nil {
				t.Fatalf("encodeParam(%#v): %v", tc.in, err)
			}
			if tc.null {
				if got != nil {
					t.Errorf("encodeParam(%#v) = %q, want nil so the parameter is sent as "+
						"SQL NULL", tc.in, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("encodeParam(%#v) returned nil, which the Bind message sends as "+
					"SQL NULL rather than %q", tc.in, tc.want)
			}
			if string(got) != tc.want {
				t.Errorf("encodeParam(%#v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestEncodeParamRejectsWhatItCannotRepresent, because silently sending
// something else would put the wrong value in the database.
func TestEncodeParamRejectsUnsupportedTypes(t *testing.T) {
	for _, v := range []any{
		struct{ A int }{1},
		[]string{"a"},
		map[string]int{"a": 1},
		make(chan int),
		func() {},
		[]int{1, 2},
		int64ptr(5),
	} {
		if got, err := encodeParam(v); err == nil {
			t.Errorf("encodeParam(%T) returned %q with no error; an unsupported type must "+
				"not be guessed at", v, got)
		}
	}
}

func int64ptr(v int64) *int64 { return &v }

// TestEncodeParamTimeIsMicrosecondsInUTC pins the two lossy things about time
// encoding: it is normalised to UTC, and PostgreSQL stores microseconds, so the
// nanosecond part of a Go time does not survive the trip. Both are fine, but a
// caller comparing what it wrote to what it read needs to know.
func TestEncodeParamTime(t *testing.T) {
	cases := []struct {
		name string
		in   time.Time
		want string
	}{
		{
			name: "UTC with microseconds",
			in:   time.Date(2026, 8, 15, 19, 2, 26, 500000000, time.UTC),
			want: "2026-08-15 19:02:26.5+00:00",
		},
		{
			name: "a whole second loses the fraction",
			in:   time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			want: "2026-01-02 03:04:05+00:00",
		},
		{
			name: "an offset is normalised to UTC",
			in:   time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("IST", 5*3600+1800)),
			want: "2026-01-01 21:34:05+00:00",
		},
		{
			name: "nanoseconds are truncated to microseconds",
			in:   time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC),
			want: "2026-01-02 03:04:05.123456+00:00",
		},
		{
			name: "the zero time is still a valid literal",
			in:   time.Time{},
			want: "0001-01-01 00:00:00+00:00",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := encodeParam(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("encodeParam(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestByteaRoundTripsEveryByte is the one that matters for the canvas blob: a
// snapshot is arbitrary binary, and a single mangled byte is a corrupted image
// that nobody notices until somebody looks at it.
func TestByteaRoundTripsEveryByte(t *testing.T) {
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	for _, blob := range [][]byte{
		all,
		{},
		{0x00},
		{0xff},
		bytes.Repeat([]byte{0x00}, 1024),
		[]byte(`\x this is not hex`),
		[]byte("\\\\\\"),
	} {
		enc, err := encodeParam(blob)
		if err != nil {
			t.Fatalf("encoding %x: %v", blob, err)
		}
		back, err := Bytea(enc)
		if err != nil {
			t.Fatalf("decoding %q: %v", enc, err)
		}
		if !bytes.Equal(back, blob) {
			t.Errorf("round trip changed the bytes\n got %x\nwant %x", back, blob)
		}
	}
}

func TestByteaDecoding(t *testing.T) {
	cases := []struct {
		name    string
		in      []byte
		want    []byte
		wantErr bool
	}{
		{name: "NULL", in: nil, want: nil},
		{name: "the empty hex value", in: []byte(`\x`), want: []byte{}},
		{name: "lowercase hex", in: []byte(`\x48656c6c6f`), want: []byte("Hello")},
		{name: "an uppercase X marker", in: []byte(`\X48`), want: []byte("H")},
		{name: "uppercase hex digits", in: []byte(`\xDEADBEEF`), want: []byte{0xde, 0xad, 0xbe, 0xef}},
		{name: "an odd number of hex digits", in: []byte(`\x123`), wantErr: true},
		{name: "a non-hex digit", in: []byte(`\xzz`), wantErr: true},

		{name: "escape format, plain text", in: []byte("hello"), want: []byte("hello")},
		{name: "escape format, empty", in: []byte(""), want: []byte{}},
		{name: "escape format, doubled backslash", in: []byte(`a\\b`), want: []byte(`a\b`)},
		{name: "escape format, octal", in: []byte(`\001\377`), want: []byte{0x01, 0xff}},
		{name: "escape format, octal at the end", in: []byte(`ab\000`), want: []byte{'a', 'b', 0x00}},
		{name: "escape format, a lone backslash", in: []byte(`\`), wantErr: true},
		{name: "escape format, a truncated octal escape", in: []byte(`\01`), wantErr: true},
		{name: "escape format, a non-octal escape", in: []byte(`\abc`), wantErr: true},
		{name: "escape format, an octal digit out of range", in: []byte(`\400`), wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Bytea(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("Bytea(%q) returned %x with no error; undecodable bytes must "+
						"not be passed off as data", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Bytea(%q): %v", tc.in, err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Errorf("Bytea(%q) = %x, want %x", tc.in, got, tc.want)
			}
		})
	}
}

func TestScalarDecoders(t *testing.T) {
	t.Run("Text", func(t *testing.T) {
		for _, tc := range []struct {
			in   []byte
			want string
		}{
			{nil, ""},
			{[]byte{}, ""},
			{[]byte("hello"), "hello"},
			{[]byte("a\x00b"), "a\x00b"},
			{[]byte{0xff}, "\xff"},
		} {
			if got := Text(tc.in); got != tc.want {
				t.Errorf("Text(%q) = %q, want %q", tc.in, got, tc.want)
			}
		}
	})

	t.Run("Int64", func(t *testing.T) {
		cases := []struct {
			in      []byte
			want    int64
			wantErr bool
		}{
			{in: nil, want: 0},
			{in: []byte("0"), want: 0},
			{in: []byte("-1"), want: -1},
			{in: []byte("9223372036854775807"), want: math.MaxInt64},
			{in: []byte("-9223372036854775808"), want: math.MinInt64},
			{in: []byte("  42  "), want: 42},
			{in: []byte("9223372036854775808"), wantErr: true},
			{in: []byte("1.5"), wantErr: true},
			{in: []byte(""), wantErr: true},
			{in: []byte("abc"), wantErr: true},
		}
		for _, tc := range cases {
			got, err := Int64(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("Int64(%q) = %d with no error; a value that does not fit must "+
						"not be silently truncated", tc.in, got)
				}
				continue
			}
			if err != nil {
				t.Errorf("Int64(%q): %v", tc.in, err)
				continue
			}
			if got != tc.want {
				t.Errorf("Int64(%q) = %d, want %d", tc.in, got, tc.want)
			}
		}
	})

	t.Run("Float64", func(t *testing.T) {
		// PostgreSQL really does emit these three spellings for float8, so a
		// decoder that only handles digits loses them.
		for _, tc := range []struct {
			in   string
			want float64
		}{
			{"0", 0},
			{"-0.5", -0.5},
			{"1e10", 1e10},
			{"Infinity", math.Inf(1)},
			{"-Infinity", math.Inf(-1)},
		} {
			got, err := Float64([]byte(tc.in))
			if err != nil {
				t.Errorf("Float64(%q): %v", tc.in, err)
				continue
			}
			if got != tc.want {
				t.Errorf("Float64(%q) = %v, want %v", tc.in, got, tc.want)
			}
		}
		if got, err := Float64([]byte("NaN")); err != nil || !math.IsNaN(got) {
			t.Errorf("Float64(\"NaN\") = %v, %v; want NaN", got, err)
		}
		if v, err := Float64(nil); err != nil || v != 0 {
			t.Errorf("Float64(NULL) = %v, %v; want 0", v, err)
		}
		if _, err := Float64([]byte("banana")); err == nil {
			t.Error("Float64 accepted \"banana\"")
		}
	})

	t.Run("Bool", func(t *testing.T) {
		for _, tc := range []struct {
			in   []byte
			want bool
		}{
			// What PostgreSQL actually emits in text format.
			{[]byte("t"), true},
			{[]byte("f"), false},
			// Spellings a hand-written query or a cast can produce.
			{[]byte("true"), true},
			{[]byte("false"), false},
			{[]byte("T"), true},
			{[]byte("1"), true},
			{[]byte("0"), false},
			// NULL and empty are false, which is what the doc comment promises.
			{nil, false},
			{[]byte{}, false},
		} {
			if got := Bool(tc.in); got != tc.want {
				t.Errorf("Bool(%q) = %v, want %v", tc.in, got, tc.want)
			}
		}
	})

	t.Run("Int", func(t *testing.T) {
		if v, err := Int([]byte("2147483647")); err != nil || v != 2147483647 {
			t.Errorf("Int = %d, %v", v, err)
		}
		if _, err := Int([]byte("nope")); err == nil {
			t.Error("Int accepted a non-numeric value")
		}
	})
}

// TestTimeParsing covers every shape PostgreSQL emits under the DateStyle the
// startup packet pins. The offsets are the interesting part: PostgreSQL prints
// whole hours as "+00", half hours as "+05:45", and - for timestamps before the
// zone was standardised - seconds, as "-04:56:02".
func TestTimeDecoding(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    time.Time
		wantErr bool
	}{
		{
			name: "UTC with microseconds",
			in:   "2026-08-15 19:02:26.265636+00",
			want: time.Date(2026, 8, 15, 19, 2, 26, 265636000, time.UTC),
		},
		{
			name: "UTC with no fraction",
			in:   "2026-01-02 03:04:05+00",
			want: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		},
		{
			name: "a whole-hour offset",
			in:   "2026-01-01 16:34:05.123456-05",
			want: time.Date(2026, 1, 1, 16, 34, 5, 123456000, time.FixedZone("", -5*3600)),
		},
		{
			name: "a three-quarter-hour offset",
			in:   "2026-01-02 03:04:05+05:45",
			want: time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("", 5*3600+45*60)),
		},
		{
			// PostgreSQL prints local mean time for dates before the zone was
			// standardised. Without a layout that has seconds in the offset this
			// is unparseable, and a historical timestamp becomes an error the
			// caller usually discards with "_".
			name: "an offset with seconds in it",
			in:   "1880-06-01 12:00:00-04:56:02",
			want: time.Date(1880, 6, 1, 12, 0, 0, 0, time.FixedZone("", -(4*3600+56*60+2))),
		},
		{
			name: "no offset at all, which a plain timestamp column gives",
			in:   "2026-01-02 03:04:05",
			want: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		},
		{
			name: "a date on its own",
			in:   "2026-01-02",
			want: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		},
		{name: "infinity, which this driver does not represent", in: "infinity", wantErr: true},
		{name: "garbage", in: "not a timestamp", wantErr: true},
		{name: "empty", in: "", wantErr: true},
		{name: "a bare time", in: "03:04:05", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Time([]byte(tc.in))
			if tc.wantErr {
				if err == nil {
					t.Errorf("Time(%q) = %v with no error; a value that cannot be parsed "+
						"must not come back as a plausible-looking instant", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Time(%q): %v", tc.in, err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("Time(%q) = %v, want %v (a %v difference)",
					tc.in, got, tc.want, got.Sub(tc.want))
			}
		})
	}

	t.Run("NULL is the zero time", func(t *testing.T) {
		got, err := Time(nil)
		if err != nil {
			t.Fatal(err)
		}
		if !got.IsZero() {
			t.Errorf("Time(NULL) = %v, want the zero time", got)
		}
	})
}

// TestAffectedFromTagEdges extends the existing table with the shapes that come
// off a real server and the ones a hostile one could invent, since the tag is a
// string the server chooses.
func TestAffectedFromTagEdges(t *testing.T) {
	cases := map[string]int64{
		"INSERT 0 1":                  1,
		"INSERT 0 0":                  0,
		"UPDATE 0":                    0,
		"SELECT 1":                    1,
		"MERGE 3":                     3,
		"COPY 5":                      5,
		"CREATE TABLE":                0,
		"COMMIT":                      0,
		"SET":                         0,
		"":                            0,
		"UPDATE":                      0,
		"UPDATE not-a-number":         0,
		"UPDATE 9223372036854775807":  math.MaxInt64,
		"UPDATE 99999999999999999999": 0,
		"UPDATE -3":                   -3,
		"   ":                         0,
	}
	for tag, want := range cases {
		if got := affectedFromTag(tag); got != want {
			t.Errorf("affectedFromTag(%q) = %d, want %d", tag, got, want)
		}
	}
}

// TestParseDSNEdges is the table the existing DSN tests do not cover: the
// malformed, the ambiguous and the merely unusual. A DSN comes from the
// environment, so every one of these is something an operator can type.
func TestParseDSNEdges(t *testing.T) {
	cases := []struct {
		name    string
		dsn     string
		wantErr bool
		check   func(*testing.T, *Config)
	}{
		{
			name: "an IPv6 host keeps its brackets off",
			dsn:  "postgres://u@[::1]:5432/db",
			check: func(t *testing.T, c *Config) {
				if c.Host != "::1" {
					t.Errorf("host = %q, want ::1 without brackets, because JoinHostPort "+
						"adds them back", c.Host)
				}
				if c.Port != "5432" {
					t.Errorf("port = %q", c.Port)
				}
			},
		},
		{
			name: "an IPv6 host with no port",
			dsn:  "postgres://u@[fe80::1]/db",
			check: func(t *testing.T, c *Config) {
				if c.Host != "fe80::1" || c.Port != "5432" {
					t.Errorf("host:port = %s:%s", c.Host, c.Port)
				}
			},
		},
		{
			name:    "a port that is not a number",
			dsn:     "postgres://u@host:notaport/db",
			wantErr: true,
		},
		{
			name: "a password full of characters that need escaping",
			dsn:  "postgres://u:p%40ss%3Aword%2F%3F%23%25@host/db",
			check: func(t *testing.T, c *Config) {
				if want := "p@ss:word/?#%"; c.Password != want {
					t.Errorf("password = %q, want %q", c.Password, want)
				}
			},
		},
		{
			name: "an empty password is not the same as no password",
			dsn:  "postgres://u:@host/db",
			check: func(t *testing.T, c *Config) {
				if c.Password != "" {
					t.Errorf("password = %q, want empty", c.Password)
				}
				if c.User != "u" {
					t.Errorf("user = %q", c.User)
				}
			},
		},
		{
			name: "postgresql:// is the same scheme",
			dsn:  "postgresql://u@host/db",
			check: func(t *testing.T, c *Config) {
				if c.Host != "host" || c.Database != "db" {
					t.Errorf("got %+v", c)
				}
			},
		},
		{
			name: "no host falls back to loopback",
			dsn:  "postgres://u@/db",
			check: func(t *testing.T, c *Config) {
				if c.Host != "127.0.0.1" {
					t.Errorf("host = %q, want the loopback default", c.Host)
				}
			},
		},
		{
			name:    "no user is refused, because the server requires one",
			dsn:     "postgres://host/db",
			wantErr: true,
		},
		{
			name: "an unknown query parameter becomes a runtime parameter",
			dsn:  "postgres://u@host/db?search_path=app&statement_timeout=5000",
			check: func(t *testing.T, c *Config) {
				if c.RuntimeParams["search_path"] != "app" {
					t.Errorf("search_path = %q", c.RuntimeParams["search_path"])
				}
				if c.RuntimeParams["statement_timeout"] != "5000" {
					t.Errorf("statement_timeout = %q", c.RuntimeParams["statement_timeout"])
				}
			},
		},
		{
			name: "connect_timeout is seconds",
			dsn:  "postgres://u@host/db?connect_timeout=3",
			check: func(t *testing.T, c *Config) {
				if c.ConnectTimeout != 3*time.Second {
					t.Errorf("ConnectTimeout = %v, want 3s", c.ConnectTimeout)
				}
			},
		},
		{
			name: "a nonsensical connect_timeout keeps the default",
			dsn:  "postgres://u@host/db?connect_timeout=abc",
			check: func(t *testing.T, c *Config) {
				if c.ConnectTimeout != 10*time.Second {
					t.Errorf("ConnectTimeout = %v, want the 10s default", c.ConnectTimeout)
				}
			},
		},
		{
			name: "a zero connect_timeout keeps the default rather than dialling forever",
			dsn:  "postgres://u@host/db?connect_timeout=0",
			check: func(t *testing.T, c *Config) {
				if c.ConnectTimeout != 10*time.Second {
					t.Errorf("ConnectTimeout = %v, want the 10s default", c.ConnectTimeout)
				}
			},
		},
		{
			name: "keyword form with no database falls back to the user",
			dsn:  "host=h user=bob",
			check: func(t *testing.T, c *Config) {
				if c.Database != "bob" {
					t.Errorf("database = %q, want the user name", c.Database)
				}
			},
		},
		{
			name:    "keyword form with a field that has no value",
			dsn:     "host=h user",
			wantErr: true,
		},
		{
			name: "keyword form with quoted values containing spaces",
			dsn:  "host=h user=bob password='a b c' application_name='my app'",
			check: func(t *testing.T, c *Config) {
				if c.Password != "a b c" {
					t.Errorf("password = %q", c.Password)
				}
				if c.ApplicationName != "my app" {
					t.Errorf("application_name = %q", c.ApplicationName)
				}
			},
		},
		{
			name: "keyword form is case-insensitive in its keys",
			dsn:  "HOST=h USER=bob DBNAME=app",
			check: func(t *testing.T, c *Config) {
				if c.Host != "h" || c.User != "bob" || c.Database != "app" {
					t.Errorf("got %+v", c)
				}
			},
		},
		{
			name: "tabs separate keyword fields too",
			dsn:  "host=h\tuser=bob\tdbname=app",
			check: func(t *testing.T, c *Config) {
				if c.User != "bob" || c.Database != "app" {
					t.Errorf("got %+v", c)
				}
			},
		},
		{name: "empty", dsn: "", wantErr: true},
		{name: "whitespace only", dsn: "   \t ", wantErr: true},
		{name: "a URL with no user and no host", dsn: "postgres://", wantErr: true},
		{name: "a scheme we do not speak is read as keyword form", dsn: "mysql://u@h/db", wantErr: true},
		{
			name:    "a URL that is not a URL",
			dsn:     "postgres://u@ho st:5432/db",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseDSN(tc.dsn)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseDSN(%q) succeeded with %+v; a DSN this broken must be "+
						"reported at startup, not turned into a connection attempt against "+
						"the wrong host", tc.dsn, c)
				}
				if !strings.Contains(err.Error(), "pg:") {
					t.Errorf("error %q is missing the package prefix", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseDSN(%q): %v", tc.dsn, err)
			}
			tc.check(t, c)
		})
	}
}

// TestRedactedNeverLeaksTheSecret, since the redacted form is what goes into
// startup logs and error reports.
func TestRedactedNeverLeaksTheSecret(t *testing.T) {
	for _, pw := range []string{"hunter2", "", "p@ss:word/?#%", "sslmode=disable"} {
		c := &Config{User: "u", Password: pw, Host: "h", Port: "5432", Database: "db", SSLMode: "require"}
		got := c.Redacted()
		if pw != "" && strings.Contains(got, pw) {
			t.Errorf("Redacted() = %q, which contains the password %q", got, pw)
		}
		for _, want := range []string{"u", "h", "5432", "db", "require"} {
			if !strings.Contains(got, want) {
				t.Errorf("Redacted() = %q, which does not identify %q; an operator cannot "+
					"tell which database failed", got, want)
			}
		}
	}
}

// TestConfigFromEnv covers the other way a config is built, which is what runs
// in production.
func TestConfigFromEnv(t *testing.T) {
	for _, k := range []string{
		"DATABASE_URL", "POSTGRES_URL", "PG_DSN",
		"PGHOST", "PGPORT", "PGUSER", "PGPASSWORD", "PGDATABASE", "PGSSLMODE", "PGAPPNAME",
	} {
		t.Setenv(k, "")
	}

	if _, err := ConfigFromEnv(); err == nil {
		t.Error("ConfigFromEnv succeeded with nothing set; it would dial some default host")
	}

	t.Setenv("PGHOST", "db.example")
	t.Setenv("PGUSER", "bob")
	c, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != "db.example" || c.User != "bob" || c.Database != "bob" || c.Port != "5432" {
		t.Errorf("PG* variables gave %+v", c)
	}

	// A URL wins, because a managed provider sets one and the PG* variables are
	// often left over from something else.
	t.Setenv("DATABASE_URL", "postgres://alice:pw@other:6000/shop?sslmode=require")
	c, err = ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != "other" || c.User != "alice" || c.Port != "6000" || c.SSLMode != "require" {
		t.Errorf("DATABASE_URL did not take precedence: %+v", c)
	}
}
