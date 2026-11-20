package edit

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite tests/edit/*/expected.toml from the writer")

// jsonEdit is one edit in tests/edit/<case>/edits.json.
type jsonEdit struct {
	Op     string   `json:"op"` // add, change, remove, set
	Table  string   `json:"table"`
	Index  int      `json:"index"`
	ID     []any    `json:"id"`     // [key, value]
	Fields [][]any  `json:"fields"` // [[key, value], ...]
	Remove []string `json:"remove"`
}

func toValue(v any) any {
	switch v := v.(type) {
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			panic(err)
		}
		return n
	case []any:
		out := make([]string, len(v))
		for i, x := range v {
			out[i] = x.(string)
		}
		return out
	}
	return v
}

func (j jsonEdit) edit() Edit {
	var fields []Field
	for _, f := range j.Fields {
		fields = append(fields, Field{Key: f[0].(string), Value: toValue(f[1])})
	}
	var idKey string
	var idValue any
	if len(j.ID) == 2 {
		idKey, idValue = j.ID[0].(string), toValue(j.ID[1])
	}
	switch j.Op {
	case "add":
		return Add(j.Table, fields...)
	case "change":
		return Change(j.Table, j.Index, idKey, idValue, fields, j.Remove...)
	case "remove":
		return Remove(j.Table, j.Index, idKey, idValue)
	case "set":
		return Set(j.Table, fields...)
	}
	panic("unknown op " + j.Op)
}

// TestGolden runs each tests/edit/<case>: router.toml with edits.json must
// become expected.toml exactly, or, if there is an expected-error.txt
// instead, fail with an error containing its text.
func TestGolden(t *testing.T) {
	dirs, err := filepath.Glob("../../tests/edit/*")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no cases in tests/edit: %v", err)
	}
	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			source, err := os.ReadFile(filepath.Join(dir, "router.toml"))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "edits.json"))
			if err != nil {
				t.Fatal(err)
			}
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			var js []jsonEdit
			if err := dec.Decode(&js); err != nil {
				t.Fatal(err)
			}
			var edits []Edit
			for _, j := range js {
				edits = append(edits, j.edit())
			}
			got, err := Apply(source, edits...)

			if want, rerr := os.ReadFile(filepath.Join(dir, "expected-error.txt")); rerr == nil {
				if err == nil || !strings.Contains(err.Error(), strings.TrimSpace(string(want))) {
					t.Fatalf("got error %v, want one containing %q", err, strings.TrimSpace(string(want)))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "expected.toml")
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("result differs from expected.toml:\n%s", lineDiff(string(want), string(got)))
			}
		})
	}
}

// lineDiff shows the first lines where two texts differ.
func lineDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < max(len(w), len(g)); i++ {
		var a, b string
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			b = g[i]
		}
		if a != b {
			return fmt.Sprintf("line %d:\n  want %q\n  got  %q", i+1, a, b)
		}
	}
	return "(only line endings differ)"
}

func TestRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		source string
		edit   Edit
		want   string // in the error
	}{
		"value over several lines": {
			"[management]\nallowed_zones = [\n  \"lan\",\n]\n",
			Set("management", Field{"allowed_zones", []string{"lan", "iot"}}),
			"can't change safely",
		},
		"inline table": {
			"[system]\nhostname = \"gw\"\nextra = { a = 1 }\n",
			Set("system", Field{"hostname", "home"}),
			"can't change safely",
		},
		"dotted key": {
			"[system]\nhostname = \"gw\"\nfoo.bar = 1\n",
			Set("system", Field{"hostname", "home"}),
			"can't change safely",
		},
		"quoted section name": {
			"[dhcp.\"my net\"]\nlease_time = \"1h\"\n[system]\nhostname = \"gw\"\n",
			Set("system", Field{"hostname", "home"}),
			"in quotes",
		},
		"section defined by dotted keys": {
			"system.hostname = \"gw\"\n",
			Set("system", Field{"hostname", "home"}),
			"doesn't fit",
		},
		"list written inline": {
			"[firewall]\nport_forward = [{ name = \"a\", protocol = \"tcp\", wan_port = 1, to = \"10.0.0.2:1\" }]\n",
			Add("firewall.port_forward", Field{"name", "b"}),
			"can't change safely",
		},
		"add to a single section": {
			"[system]\nhostname = \"gw\"\n",
			Add("system", Field{"hostname", "home"}),
			"not a list",
		},
		"stale entry": {
			"[[firewall.port_forward]]\nname = \"a\"\n",
			Remove("firewall.port_forward", 0, "name", "b"),
			"changed since",
		},
		"entry gone": {
			"[[firewall.port_forward]]\nname = \"a\"\n",
			Remove("firewall.port_forward", 1, "name", "a"),
			"changed since",
		},
		"unreadable file": {
			"[system\nhostname = \"gw\"\n",
			Set("system", Field{"hostname", "home"}),
			"has a mistake",
		},
		"value kind": {
			"[system]\n",
			Set("system", Field{"hostname", 1.5}),
			"can't write",
		},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := Apply([]byte(tc.source), tc.edit)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %q, %v; want an error containing %q", out, err, tc.want)
			}
			var cf *CheckFailed
			if errors.As(err, &cf) {
				t.Fatalf("a refusal must not be a failed check: %v", err)
			}
		})
	}
}

func TestOneChangePerList(t *testing.T) {
	src := "[[firewall.port_forward]]\nname = \"a\"\n\n[[firewall.port_forward]]\nname = \"b\"\n"
	_, err := Apply([]byte(src),
		Remove("firewall.port_forward", 0, "name", "a"),
		Change("firewall.port_forward", 1, "name", "b", []Field{{"name", "c"}}))
	if err == nil {
		t.Fatal("two index edits on one list were accepted")
	}
}
