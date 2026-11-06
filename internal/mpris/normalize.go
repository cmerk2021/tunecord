package mpris

import (
	"math"
	"strconv"
	"strings"

	"github.com/godbus/dbus/v5"
)

// maxMicroseconds guards against overflow when converting floats.
const maxMicroseconds = float64(math.MaxInt64 / 2)

// ToMicroseconds normalizes a D-Bus time value (Position, mpris:length,
// Seeked argument) into int64 microseconds. Players are inconsistent: values
// arrive as int64, uint64, int32, uint32, other integer widths, float64, or
// even strings, possibly wrapped in (nested) variants. The bool is false if
// the value is missing or cannot be interpreted.
func ToMicroseconds(v any) (int64, bool) {
	v = unwrap(v)
	switch x := v.(type) {
	case nil:
		return 0, false
	case int64:
		return x, true
	case int32:
		return int64(x), true
	case int16:
		return int64(x), true
	case int8:
		return int64(x), true
	case int:
		return int64(x), true
	case uint64:
		if x > math.MaxInt64 {
			return 0, false
		}
		return int64(x), true
	case uint32:
		return int64(x), true
	case uint16:
		return int64(x), true
	case uint8:
		return int64(x), true
	case uint:
		if uint64(x) > math.MaxInt64 {
			return 0, false
		}
		return int64(x), true
	case float64:
		return floatToMicro(x)
	case float32:
		return floatToMicro(float64(x))
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0, false
		}
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n, true
		}
		if n, err := strconv.ParseUint(s, 10, 64); err == nil {
			if n > math.MaxInt64 {
				return 0, false
			}
			return int64(n), true
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return floatToMicro(f)
		}
		return 0, false
	case []byte:
		return ToMicroseconds(string(x))
	}
	return 0, false
}

func floatToMicro(f float64) (int64, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) > maxMicroseconds {
		return 0, false
	}
	return int64(math.Round(f)), true
}

// ToFloat normalizes a numeric D-Bus value (e.g. Rate) to float64.
func ToFloat(v any) (float64, bool) {
	v = unwrap(v)
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return 0, false
		}
		return x, true
	case float32:
		return ToFloat(float64(x))
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return 0, false
		}
		return ToFloat(f)
	}
	if n, ok := ToMicroseconds(v); ok {
		return float64(n), true
	}
	return 0, false
}

// ToString extracts a string (also from object paths and variants).
func ToString(v any) (string, bool) {
	v = unwrap(v)
	switch x := v.(type) {
	case string:
		return x, true
	case dbus.ObjectPath:
		return string(x), true
	case []byte:
		return string(x), true
	}
	return "", false
}

// ToStringSlice extracts a list of strings; a single string becomes a
// one-element slice. Empty entries are dropped.
func ToStringSlice(v any) []string {
	v = unwrap(v)
	var out []string
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	switch x := v.(type) {
	case string:
		add(x)
	case []string:
		for _, s := range x {
			add(s)
		}
	case []any:
		for _, e := range x {
			if s, ok := ToString(e); ok {
				add(s)
			}
		}
	case []dbus.Variant:
		for _, e := range x {
			if s, ok := ToString(e); ok {
				add(s)
			}
		}
	}
	return out
}

func unwrap(v any) any {
	for i := 0; i < 8; i++ {
		switch x := v.(type) {
		case dbus.Variant:
			v = x.Value()
		case *dbus.Variant:
			if x == nil {
				return nil
			}
			v = x.Value()
		default:
			return v
		}
	}
	return v
}

// Unwrap removes any (nested) variant wrappers.
func Unwrap(v any) any { return unwrap(v) }
