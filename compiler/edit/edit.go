// Package edit changes router.toml's text the way a person would, so that a
// form in the web UI can change one setting without losing the user's
// comments, order and hand edits (ADR 0016).
//
// It knows a fixed list of edits: add, change or remove an entry in a list
// of sections ([[firewall.port_forward]], [[dhcp.lan.static]], [[vlan]]),
// and set keys in a section ([system]). Every result is checked by parsing
// again: it must mean the old config plus exactly the edits, and every line
// the edits didn't touch must be the same.
package edit

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// Field is a key and its value. Values are strings, ints, bools or []string.
type Field struct {
	Key   string
	Value any
}

type op int

const (
	opAdd op = iota
	opChange
	opRemove
	opSet
	opInsert
)

// Edit is one change to router.toml. Make one with Add, Insert, Change,
// Remove or Set.
type Edit struct {
	op      op
	table   string
	index   int
	idKey   string
	idValue any
	set     []Field
	remove  []string
}

// Add adds an entry to the list of sections table, such as
// "firewall.port_forward", after its last entry.
func Add(table string, fields ...Field) Edit {
	return Edit{op: opAdd, table: table, set: fields}
}

// Insert adds an entry to the list table before entry before (from 0, in
// file order), above the comment lines that belong to that entry; before
// equal to the number of entries adds it at the end. For lists whose order
// matters, such as "firewall.policy" (ADR 0017).
func Insert(table string, before int, fields ...Field) Edit {
	return Edit{op: opInsert, table: table, index: before, set: fields}
}

// Change changes entry index (from 0, in file order) of the list table. The
// entry's idKey must have the value idValue (a port forward's "name"), or the
// file changed since it was read and the edit is refused. set gives keys
// new values, adding the ones that aren't there; remove deletes keys.
func Change(table string, index int, idKey string, idValue any, set []Field, remove ...string) Edit {
	return Edit{op: opChange, table: table, index: index, idKey: idKey, idValue: idValue, set: set, remove: remove}
}

// Remove removes entry index of the list table, with the comment above it.
func Remove(table string, index int, idKey string, idValue any) Edit {
	return Edit{op: opRemove, table: table, index: index, idKey: idKey, idValue: idValue}
}

// Set sets keys in the section table ("system"), adding the section if
// there is none; "" is the keys before the first section.
func Set(table string, fields ...Field) Edit {
	return Edit{op: opSet, table: table, set: fields}
}

// AdvancedHint is what a page shows with a Refusal.
const AdvancedHint = "This part of router.toml is written in a way this page can't change safely. Use Advanced to edit it."

// Refusal means the edit can't be made safely on this file: the part it
// would change is written in a way the writer doesn't edit. Nothing changed.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return r.Reason }

func refuse(format string, args ...any) *Refusal {
	return &Refusal{Reason: fmt.Sprintf(format, args...)}
}

// CheckFailed means the writer's own result didn't pass its check: a bug in
// the writer. Nothing changed.
type CheckFailed struct{ What string }

func (c *CheckFailed) Error() string {
	return "edit: the result didn't pass its check (" + c.What + "), so nothing was changed"
}

// ErrStale means router.toml changed since the page read it.
var ErrStale = errors.New("router.toml changed since this page read it; reload the page")

// patch replaces old lines [start, end) with lines; start == end inserts.
type patch struct {
	start, end int
	lines      []string
	seq        int
	section    bool   // inserts a new section (rather than lines of the section above)
	table      string // section: its name
}

// Apply makes the edits on source and returns the new text, after checking it.
func Apply(source []byte, edits ...Edit) ([]byte, error) {
	old, err := decode(source)
	if err != nil {
		return nil, fmt.Errorf("router.toml has a mistake, so this page can't change it (fix it in Advanced): %w", err)
	}
	d := parseDoc(source)
	if d.hasComplexHeader() {
		return nil, refuse("router.toml has a section name in quotes. %s", AdvancedHint)
	}
	want := normalize(old).(map[string]any) // a copy
	indexed, added, set, inserted := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
	var ps []patch
	for n, e := range edits {
		if err := e.check(); err != nil {
			return nil, err
		}
		switch e.op {
		case opChange, opRemove:
			if indexed[e.table] || (e.op == opRemove && added[e.table]) {
				return nil, fmt.Errorf("edit: one change or removal per list at a time (%s)", e.table)
			}
			indexed[e.table] = true
		case opAdd:
			if inserted[e.table] {
				return nil, fmt.Errorf("edit: an insert and an add on one list at a time (%s)", e.table)
			}
			added[e.table] = true
		case opInsert:
			if indexed[e.table] || added[e.table] {
				return nil, fmt.Errorf("edit: one insert, change or removal per list at a time (%s)", e.table)
			}
			indexed[e.table], inserted[e.table] = true, true
		case opSet:
			if set[e.table] {
				return nil, fmt.Errorf("edit: one Set per section at a time; put all its keys in one (%q)", e.table)
			}
			set[e.table] = true
		}
		p, err := e.patches(d, old)
		if err != nil {
			return nil, err
		}
		for i := range p {
			p[i].seq = n*1000 + i
		}
		ps = append(ps, p...)
		if err := e.expect(want); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(ps, func(i, j int) bool {
		a, b := ps[i], ps[j]
		if a.start != b.start {
			return a.start < b.start
		}
		if (a.start == a.end) != (b.start == b.end) {
			return a.start == a.end // inserts before a replacement at the same line
		}
		if a.section != b.section {
			// Keys added to the section above go before a new section
			// inserted at the same place, or they'd land in the new one.
			return !a.section
		}
		if a.section && a.table != b.table {
			// A new [dhcp.guest] goes before a new [[dhcp.guest.static]].
			if strings.HasPrefix(b.table, a.table+".") {
				return true
			}
			if strings.HasPrefix(a.table, b.table+".") {
				return false
			}
		}
		return a.seq < b.seq
	})
	for i := 1; i < len(ps); i++ {
		if ps[i].start < ps[i-1].end {
			return nil, fmt.Errorf("edit: two edits change the same lines of router.toml")
		}
	}
	out := d.render(ps)
	got, err := decode(out)
	if err != nil {
		return nil, refuse("the change doesn't fit how router.toml is written (%v). %s", err, AdvancedHint)
	}
	if !reflect.DeepEqual(got, normalize(want)) {
		return nil, &CheckFailed{What: "the new router.toml doesn't mean the old one plus exactly this change"}
	}
	if err := untouched(d, ps, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (e Edit) check() error {
	segs := strings.Split(e.table, ".")
	if e.table == "" && e.op == opSet {
		segs = nil
	}
	for _, s := range segs {
		if !bareKey.MatchString(s) {
			return fmt.Errorf("edit: %q isn't a section name router.toml uses", e.table)
		}
	}
	keys := map[string]bool{}
	for _, f := range e.set {
		if !bareKey.MatchString(f.Key) || keys[f.Key] {
			return fmt.Errorf("edit: bad or repeated key %q", f.Key)
		}
		keys[f.Key] = true
		if _, err := encode(f.Value); err != nil {
			return err
		}
	}
	for _, k := range e.remove {
		if keys[k] {
			return fmt.Errorf("edit: key %q is both set and removed", k)
		}
	}
	if (e.op == opChange || e.op == opRemove) && (e.index < 0 || !bareKey.MatchString(e.idKey)) {
		return fmt.Errorf("edit: an entry is named by its position and a key")
	}
	if (e.op == opAdd || e.op == opInsert) && len(e.set) == 0 {
		return fmt.Errorf("edit: a new entry needs at least one key")
	}
	if e.op == opInsert && e.index < 0 {
		return fmt.Errorf("edit: an insert goes before an entry, counted from 0")
	}
	return nil
}

func fieldLines(fields []Field) []string {
	lines := make([]string, len(fields))
	for i, f := range fields {
		v, _ := encode(f.Value) // check() made sure it encodes
		lines[i] = f.Key + " = " + v
	}
	return lines
}

func (e Edit) patches(d *doc, old map[string]any) ([]patch, error) {
	switch e.op {
	case opAdd:
		list, plain := d.entries(e.table)
		if plain {
			return nil, refuse("%s is a single section in router.toml, not a list. %s", e.table, AdvancedHint)
		}
		var after *block
		if len(list) > 0 {
			after = list[len(list)-1]
		} else if i := strings.LastIndexByte(e.table, '.'); i > 0 {
			after, _ = d.section(e.table[:i])
		}
		if after == nil {
			after = d.lastWithPrefix(e.table)
		}
		return []patch{d.newSection(after, e.table, true, e.set)}, nil

	case opInsert:
		list, plain := d.entries(e.table)
		switch {
		case plain:
			return nil, refuse("%s is a single section in router.toml, not a list. %s", e.table, AdvancedHint)
		case e.index > len(list):
			return nil, ErrStale
		case e.index == len(list):
			add := e
			add.op = opAdd
			return add.patches(d, old)
		}
		b := list[e.index]
		lines := append(append([]string{"[[" + e.table + "]]"}, fieldLines(e.set)...), "")
		return []patch{{start: b.start, end: b.start, lines: lines, section: true, table: e.table}}, nil

	case opChange, opRemove:
		list, _ := d.entries(e.table)
		if e.index >= len(list) {
			return nil, ErrStale
		}
		if err := checkID(old, e); err != nil {
			return nil, err
		}
		b := list[e.index]
		if b.complex {
			return nil, refuse("that %s entry is written in a way this page can't change safely. Use Advanced to edit it.", e.table)
		}
		if e.op == opChange {
			return d.keyPatches(b, e.set, e.remove), nil
		}
		start, end := b.start, b.end
		if end == len(d.lines) { // the last section: no blank lines left at the end
			for start > 0 && d.lines[start-1].kind == blankLine {
				start--
			}
		}
		return []patch{{start: start, end: end}}, nil

	default: // opSet
		b, n := d.section(e.table)
		if n > 1 || (n == 1 && b == nil) {
			return nil, refuse("%s is a list of sections in router.toml, or written twice. %s", e.table, AdvancedHint)
		}
		if b != nil {
			if b.complex {
				return nil, refuse("the [%s] section is written in a way this page can't change safely. Use Advanced to edit it.", e.table)
			}
			return d.keyPatches(b, e.set, nil), nil
		}
		return []patch{d.newSection(d.lastWithPrefix(e.table), e.table, false, e.set)}, nil
	}
}

// keyPatches rewrites, adds and removes key lines in block b. A rewritten
// line keeps everything but its value, so its comment stays.
func (d *doc) keyPatches(b *block, set []Field, remove []string) []patch {
	var ps []patch
	var add []Field
	for _, f := range set {
		li, ok := b.keys[f.Key]
		if !ok {
			add = append(add, f)
			continue
		}
		l := d.lines[li]
		v, _ := encode(f.Value)
		ps = append(ps, patch{start: li, end: li + 1, lines: []string{l.text[:l.valueStart] + v + l.text[l.valueEnd:]}})
	}
	for _, k := range remove {
		if li, ok := b.keys[k]; ok {
			ps = append(ps, patch{start: li, end: li + 1})
		}
	}
	if len(add) > 0 {
		pos := b.lastKey + 1
		ps = append(ps, patch{start: pos, end: pos, lines: fieldLines(add)})
	}
	return ps
}

// newSection inserts a header ([table], or [[table]] for a list entry) and
// its keys after block after's last line that isn't blank, or at the end of
// the file if after is nil.
func (d *doc) newSection(after *block, table string, entry bool, fields []Field) patch {
	header := "[" + table + "]"
	if entry {
		header = "[[" + table + "]]"
	}
	pos := 0
	if after != nil {
		pos = after.lastContent + 1
	} else {
		for i := len(d.lines) - 1; i >= 0; i-- {
			if d.lines[i].kind != blankLine {
				pos = i + 1
				break
			}
		}
	}
	lines := append([]string{"", header}, fieldLines(fields)...)
	if pos == 0 {
		lines = lines[1:] // no blank line at the top of the file
	}
	return patch{start: pos, end: pos, lines: lines, section: true, table: table}
}

// lastWithPrefix returns the last section in the same family as table
// (for "dhcp.iot", the last [dhcp...] section), or nil.
func (d *doc) lastWithPrefix(table string) *block {
	first, _, _ := strings.Cut(table, ".")
	var last *block
	for i := range d.blocks[1:] {
		b := &d.blocks[i+1]
		if b.table == first || strings.HasPrefix(b.table, first+".") {
			last = b
		}
	}
	return last
}

func splitTable(table string) (parent []string, last string) {
	segs := strings.Split(table, ".")
	return segs[:len(segs)-1], segs[len(segs)-1]
}

// list returns the entries of the list table in tree, and its parent table.
func list(tree map[string]any, table string, create bool) ([]any, map[string]any, error) {
	path, last := splitTable(table)
	parent, err := lookup(tree, path, create)
	if err != nil || parent == nil {
		return nil, parent, err
	}
	switch v := parent[last].(type) {
	case nil:
		return nil, parent, nil
	case []any:
		return v, parent, nil
	}
	return nil, nil, refuse("%s isn't a list of sections in router.toml. %s", table, AdvancedHint)
}

func checkID(old map[string]any, e Edit) error {
	entries, _, err := list(old, e.table, false)
	if err != nil {
		return err
	}
	if e.index >= len(entries) {
		return ErrStale
	}
	entry, _ := entries[e.index].(map[string]any)
	if !reflect.DeepEqual(entry[e.idKey], normalize(e.idValue)) {
		return fmt.Errorf("%w (entry %d is %s %s, not %s)", ErrStale, e.index, e.idKey, describe(entry[e.idKey]), describe(e.idValue))
	}
	return nil
}

// expect makes the edit on the decoded config: what the new file must mean.
func (e Edit) expect(want map[string]any) error {
	switch e.op {
	case opAdd:
		entries, parent, err := list(want, e.table, true)
		if err != nil {
			return err
		}
		_, last := splitTable(e.table)
		parent[last] = append(entries, fieldsMap(e.set))
	case opInsert:
		entries, parent, err := list(want, e.table, true)
		if err != nil {
			return err
		}
		if e.index > len(entries) {
			return ErrStale
		}
		_, last := splitTable(e.table)
		parent[last] = append(entries[:e.index:e.index], append([]any{fieldsMap(e.set)}, entries[e.index:]...)...)
	case opChange:
		entries, _, err := list(want, e.table, false)
		if err != nil {
			return err
		}
		entry := entries[e.index].(map[string]any)
		for k, v := range fieldsMap(e.set) {
			entry[k] = v
		}
		for _, k := range e.remove {
			delete(entry, k)
		}
	case opRemove:
		entries, parent, err := list(want, e.table, false)
		if err != nil {
			return err
		}
		_, last := splitTable(e.table)
		entries = append(entries[:e.index:e.index], entries[e.index+1:]...)
		if len(entries) == 0 {
			delete(parent, last)
		} else {
			parent[last] = entries
		}
	case opSet:
		var path []string
		if e.table != "" {
			path = strings.Split(e.table, ".")
		}
		t, err := lookup(want, path, true)
		if err != nil {
			return err
		}
		for k, v := range fieldsMap(e.set) {
			t[k] = v
		}
	}
	return nil
}

func (d *doc) render(ps []patch) []byte {
	var out []string
	i := 0
	atEnd := false
	for _, p := range ps {
		for ; i < p.start; i++ {
			out = append(out, d.lines[i].text)
		}
		out = append(out, p.lines...)
		i = max(i, p.end)
		atEnd = atEnd || p.start == len(d.lines)
	}
	for ; i < len(d.lines); i++ {
		out = append(out, d.lines[i].text)
	}
	s := strings.Join(out, d.eol)
	if len(out) > 0 && (d.final || atEnd) {
		s += d.eol
	}
	if d.bom {
		s = byteOrderMark + s
	}
	return []byte(s)
}

// untouched checks that every line outside the patches is still there, in
// order, and that the new file has no lines but those and the patches'.
func untouched(d *doc, ps []patch, out []byte) error {
	patched := make([]bool, len(d.lines))
	added := 0
	for _, p := range ps {
		for k := p.start; k < p.end; k++ {
			patched[k] = true
		}
		added += len(p.lines)
	}
	var keep []string
	for k, l := range d.lines {
		if !patched[k] {
			keep = append(keep, l.text)
		}
	}
	got := parseDoc(out).lines
	if len(got) != len(keep)+added {
		return &CheckFailed{What: fmt.Sprintf("the new file has %d lines, want %d", len(got), len(keep)+added)}
	}
	j := 0
	for _, l := range got {
		if j < len(keep) && l.text == keep[j] {
			j++
		}
	}
	if j != len(keep) {
		return &CheckFailed{What: fmt.Sprintf("a line the edit didn't touch changed: %q", keep[j])}
	}
	return nil
}
