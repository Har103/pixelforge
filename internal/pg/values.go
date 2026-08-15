package pg

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// encodeParam renders a Go value as a text-format bind parameter. A nil return
// with a nil error means SQL NULL.
//
// Text format costs a little bandwidth versus binary but removes every
// dependency on type OIDs, which differ across extensions and major versions.
// For this workload - small rows and one bytea blob - that is the right trade.
func encodeParam(v any) ([]byte, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		return []byte(t), nil
	case []byte:
		if t == nil {
			return nil, nil
		}
		// bytea literal: \x followed by lowercase hex.
		out := make([]byte, 2+hex.EncodedLen(len(t)))
		out[0], out[1] = '\\', 'x'
		hex.Encode(out[2:], t)
		return out, nil
	case bool:
		if t {
			return []byte("t"), nil
		}
		return []byte("f"), nil
	case int:
		return strconv.AppendInt(nil, int64(t), 10), nil
	case int8:
		return strconv.AppendInt(nil, int64(t), 10), nil
	case int16:
		return strconv.AppendInt(nil, int64(t), 10), nil
	case int32:
		return strconv.AppendInt(nil, int64(t), 10), nil
	case int64:
		return strconv.AppendInt(nil, t, 10), nil
	case uint:
		return strconv.AppendUint(nil, uint64(t), 10), nil
	case uint8:
		return strconv.AppendUint(nil, uint64(t), 10), nil
	case uint16:
		return strconv.AppendUint(nil, uint64(t), 10), nil
	case uint32:
		return strconv.AppendUint(nil, uint64(t), 10), nil
	case uint64:
		return strconv.AppendUint(nil, t, 10), nil
	case float32:
		return strconv.AppendFloat(nil, float64(t), 'g', -1, 32), nil
	case float64:
		return strconv.AppendFloat(nil, t, 'g', -1, 64), nil
	case time.Time:
		return []byte(t.UTC().Format("2006-01-02 15:04:05.999999-07:00")), nil
	default:
		return nil, fmt.Errorf("unsupported parameter type %T", v)
	}
}

// Text returns a column value as a string. NULL becomes "".
func Text(v []byte) string { return string(v) }

// Int64 parses an integer column. NULL becomes 0.
func Int64(v []byte) (int64, error) {
	if v == nil {
		return 0, nil
	}
	return strconv.ParseInt(strings.TrimSpace(string(v)), 10, 64)
}

// Int parses an integer column, clamped to int. NULL becomes 0.
func Int(v []byte) (int, error) {
	n, err := Int64(v)
	return int(n), err
}

// Float64 parses a floating point column. NULL becomes 0.
func Float64(v []byte) (float64, error) {
	if v == nil {
		return 0, nil
	}
	return strconv.ParseFloat(strings.TrimSpace(string(v)), 64)
}

// Bool parses a boolean column. NULL becomes false.
func Bool(v []byte) bool { return len(v) > 0 && (v[0] == 't' || v[0] == 'T' || v[0] == '1') }

// Bytea decodes a bytea column in either the modern hex format (\x48656c6c6f)
// or the legacy escape format that older servers may still emit.
func Bytea(v []byte) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	if len(v) >= 2 && v[0] == '\\' && (v[1] == 'x' || v[1] == 'X') {
		out := make([]byte, hex.DecodedLen(len(v)-2))
		n, err := hex.Decode(out, v[2:])
		if err != nil {
			return nil, fmt.Errorf("pg: decoding bytea hex: %w", err)
		}
		return out[:n], nil
	}
	return decodeByteaEscape(v)
}

// decodeByteaEscape handles the pre-9.0 output format: printable bytes appear
// literally, a backslash is doubled, and anything else is \NNN in octal.
func decodeByteaEscape(v []byte) ([]byte, error) {
	out := make([]byte, 0, len(v))
	for i := 0; i < len(v); {
		if v[i] != '\\' {
			out = append(out, v[i])
			i++
			continue
		}
		if i+1 < len(v) && v[i+1] == '\\' {
			out = append(out, '\\')
			i += 2
			continue
		}
		if i+3 < len(v) {
			n, err := strconv.ParseUint(string(v[i+1:i+4]), 8, 8)
			if err != nil {
				return nil, fmt.Errorf("pg: decoding bytea escape: %w", err)
			}
			out = append(out, byte(n))
			i += 4
			continue
		}
		return nil, fmt.Errorf("pg: truncated bytea escape sequence")
	}
	return out, nil
}

// Time parses a timestamptz column emitted with DateStyle "ISO, MDY", which the
// startup packet pins. NULL becomes the zero Time.
func Time(v []byte) (time.Time, error) {
	if v == nil {
		return time.Time{}, nil
	}
	s := string(v)
	layouts := []string{
		"2006-01-02 15:04:05.999999-07",
		"2006-01-02 15:04:05.999999-07:00",
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05-07",
		"2006-01-02",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("pg: cannot parse timestamp %q", s)
}
