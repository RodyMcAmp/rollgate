package config

import (
	"bytes"
	"encoding/json"
	"flag"
	"maps"
	"os"
	"slices"
	"testing"
)

var update = flag.Bool("update", false, "rewrite schema/router.schema.json")

const schemaFile = "../../schema/router.schema.json"

// TestSchemaCoversEveryKey: Schema() fails on an undocumented key, and every
// entry in keys must belong to a real key (no leftovers after a rename).
func TestSchemaCoversEveryKey(t *testing.T) {
	out, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]any
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var walk func(node map[string]any, path string)
	walk = func(node map[string]any, path string) {
		if path != "" {
			seen[path] = true
		}
		if props, ok := node["properties"].(map[string]any); ok {
			for name, p := range props {
				walk(p.(map[string]any), join(path, name))
			}
		}
		if items, ok := node["items"].(map[string]any); ok {
			walk(items, path+"[]")
		}
		if values, ok := node["additionalProperties"].(map[string]any); ok {
			walk(values, join(path, "*"))
		}
	}
	walk(s, "")
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		if !seen[k] {
			t.Errorf("keys has an entry for %q, which isn't a router.toml key", k)
		}
	}
}

// TestSchemaFile keeps the published schema in step with the code.
func TestSchemaFile(t *testing.T) {
	want, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.WriteFile(schemaFile, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(schemaFile)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("schema/router.schema.json is out of date (%v); run `make golden-update` and commit it", err)
	}
}
