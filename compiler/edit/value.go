package edit

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// encode writes a value the way examples/router.toml does. Only the kinds
// router.toml has: strings, whole numbers, true or false, lists of strings.
func encode(v any) (string, error) {
	switch v := v.(type) {
	case string:
		return quote(v), nil
	case int:
		return strconv.Itoa(v), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case bool:
		return strconv.FormatBool(v), nil
	case []string:
		parts := make([]string, len(v))
		for i, s := range v {
			parts[i] = quote(s)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	}
	return "", fmt.Errorf("edit: can't write a %T value", v)
}

// quote is a TOML basic string.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\r':
			b.WriteString(`\r`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// decode reads TOML into plain values, as the parser understands it.
func decode(source []byte) (map[string]any, error) {
	var tree map[string]any
	if _, err := toml.Decode(string(source), &tree); err != nil {
		return nil, err
	}
	return normalize(tree).(map[string]any), nil
}

// normalize gives decoded and expected values one shape: lists as []any,
// whole numbers as int64, and no empty tables (an empty table and a missing
// one mean the same to the compiler).
func normalize(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			x = normalize(x)
			if m, ok := x.(map[string]any); ok && len(m) == 0 {
				continue
			}
			out[k] = x
		}
		return out
	case []map[string]any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = normalize(x)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = normalize(x)
		}
		return out
	case []string:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = x
		}
		return out
	case int:
		return int64(v)
	}
	return v
}

// lookup walks to the table at path, creating missing tables if create is set.
func lookup(tree map[string]any, path []string, create bool) (map[string]any, error) {
	t := tree
	for i, seg := range path {
		switch next := t[seg].(type) {
		case map[string]any:
			t = next
		case nil:
			if !create {
				return nil, nil
			}
			m := map[string]any{}
			t[seg] = m
			t = m
		default:
			return nil, refuse("%s isn't a section in router.toml", strings.Join(path[:i+1], "."))
		}
	}
	return t, nil
}

func fieldsMap(fields []Field) map[string]any {
	m := make(map[string]any, len(fields))
	for _, f := range fields {
		m[f.Key] = normalize(f.Value)
	}
	return m
}

// describe is a value as a person would read it in an error.
func describe(v any) string {
	if s, ok := v.(string); ok {
		return strconv.Quote(s)
	}
	return fmt.Sprint(v)
}
