package edit

import (
	"regexp"
	"strings"
)

// The scan finds where things are in router.toml, line by line: blank
// lines, comments, section headers and key lines. It doesn't decode values;
// the real parser does that when the result is checked.

type kind int

const (
	blankLine   kind = iota
	commentLine      // only a comment
	headerLine       // [table] or [[table]]
	keyLine          // key = value
	contLine         // a later line of a value spread over several lines
	otherLine        // anything else; the parser decides what it means
)

type line struct {
	text string // without the line ending
	kind kind

	table string // headerLine: the dotted name; "" if it isn't plain
	array bool   // headerLine: [[table]]

	key                  string // keyLine: a plain key; "" if dotted or quoted
	valueStart, valueEnd int    // keyLine: where the value is in text

	// complex marks what the writer doesn't edit: a quoted header name, a
	// dotted or quoted key, an inline table, or a value over several lines.
	complex bool
}

var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// byteOrderMark is U+FEFF, which some Windows editors put at the start of a file.
const byteOrderMark = string(rune(0xFEFF))

// valueState carries a value that continues on the next line.
type valueState struct {
	ml    string // inside a multi-line string: `"""` or `'''`
	depth int    // open [ of an array
}

func (v valueState) open() bool { return v.ml != "" || v.depth > 0 }

// scanLines classifies each line of a file.
func scanLines(texts []string) []line {
	lines := make([]line, len(texts))
	var st valueState
	owner := -1 // the key line a continuing value belongs to
	for i, t := range texts {
		l := line{text: t}
		if st.open() {
			l.kind = contLine
			scanValue(t, 0, &st)
			lines[owner].complex = true
			lines[i] = l
			continue
		}
		trimmed := strings.TrimSpace(t)
		switch {
		case trimmed == "":
			l.kind = blankLine
		case strings.HasPrefix(trimmed, "#"):
			l.kind = commentLine
		case strings.HasPrefix(trimmed, "["):
			scanHeader(&l, trimmed)
		default:
			scanKey(&l, &st)
			if st.open() {
				l.complex = true
				owner = i
			}
		}
		lines[i] = l
	}
	return lines
}

func scanHeader(l *line, trimmed string) {
	l.kind = headerLine
	open, closing := "[", "]"
	if strings.HasPrefix(trimmed, "[[") {
		open, closing, l.array = "[[", "]]", true
	}
	end := strings.Index(trimmed, closing)
	if end < 0 || strings.ContainsAny(trimmed[:end], `"'`) {
		l.complex = true
		return
	}
	rest := strings.TrimSpace(trimmed[end+len(closing):])
	if rest != "" && !strings.HasPrefix(rest, "#") {
		l.kind, l.complex = otherLine, true
		return
	}
	var segs []string
	for _, s := range strings.Split(trimmed[len(open):end], ".") {
		s = strings.TrimSpace(s)
		if !bareKey.MatchString(s) {
			l.complex = true
			return
		}
		segs = append(segs, s)
	}
	l.table = strings.Join(segs, ".")
}

func scanKey(l *line, st *valueState) {
	eq := strings.IndexByte(l.text, '=')
	if eq < 0 {
		l.kind, l.complex = otherLine, true
		return
	}
	l.kind = keyLine
	if k := strings.TrimSpace(l.text[:eq]); bareKey.MatchString(k) {
		l.key = k
	} else {
		l.complex = true // dotted or quoted
	}
	start := eq + 1
	for start < len(l.text) && (l.text[start] == ' ' || l.text[start] == '\t') {
		start++
	}
	if start < len(l.text) && l.text[start] == '{' {
		l.complex = true
	}
	l.valueStart = start
	l.valueEnd = scanValue(l.text, start, st)
}

// scanValue reads a value from s[i:], carrying st across lines, and returns
// where the value's text ends on this line: before trailing spaces and a
// comment.
func scanValue(s string, i int, st *valueState) int {
	last := i
	for i < len(s) {
		if st.ml != "" {
			basic := st.ml == `"""`
			for i < len(s) && st.ml != "" {
				switch {
				case basic && s[i] == '\\':
					i += 2
				case strings.HasPrefix(s[i:], st.ml):
					i += 3
					// Up to two more quotes belong to the string ("""a"""" ).
					for k := 0; k < 2 && i < len(s) && s[i] == st.ml[0]; k++ {
						i++
					}
					st.ml = ""
				default:
					i++
				}
			}
			last = min(i, len(s))
			continue
		}
		c := s[i]
		switch {
		case c == '#':
			return last
		case c == ' ' || c == '\t':
			i++
			continue
		case strings.HasPrefix(s[i:], `"""`), strings.HasPrefix(s[i:], "'''"):
			st.ml = s[i : i+3]
			i += 3
		case c == '"':
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' {
					i++
				}
				i++
			}
			i++
		case c == '\'':
			if j := strings.IndexByte(s[i+1:], '\''); j >= 0 {
				i += j + 2
			} else {
				i = len(s)
			}
		case c == '[':
			st.depth++
			i++
		case c == ']':
			st.depth--
			i++
		default:
			i++
		}
		last = min(i, len(s))
	}
	return last
}

// block is a section: its header and the lines under it, up to the next
// header. The preamble (keys before the first header) is a block too, with
// header -1.
type block struct {
	start  int // first line: the header, or the comment lines directly above it
	header int // the header line; -1 for the preamble
	end    int // one past the last line, blank lines included

	table   string
	array   bool
	complex bool
	keys    map[string]int // key -> its line

	lastKey     int // the last key or value line; header if none
	lastContent int // the last line that isn't blank
}

type doc struct {
	lines  []line
	blocks []block
	eol    string // "\n" or "\r\n"
	bom    bool
	final  bool // the file ends with a line ending
}

func parseDoc(source []byte) *doc {
	s := string(source)
	d := &doc{eol: "\n"}
	if strings.HasPrefix(s, byteOrderMark) {
		d.bom, s = true, strings.TrimPrefix(s, byteOrderMark)
	}
	if i := strings.IndexByte(s, '\n'); i > 0 && s[i-1] == '\r' {
		d.eol = "\r\n"
	}
	d.final = strings.HasSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\n")
	var texts []string
	if s != "" || !d.final {
		texts = strings.Split(s, "\n")
	}
	for i, t := range texts {
		texts[i] = strings.TrimSuffix(t, "\r")
	}
	if len(texts) == 1 && texts[0] == "" {
		texts = nil
	}
	d.lines = scanLines(texts)

	var headers []int
	for i, l := range d.lines {
		if l.kind == headerLine {
			headers = append(headers, i)
		}
	}
	starts := make([]int, len(headers))
	prev := 0
	for n, h := range headers {
		start := h
		for start > prev && d.lines[start-1].kind == commentLine {
			start--
		}
		starts[n] = start
		prev = h + 1
	}
	preambleEnd := len(d.lines)
	if len(starts) > 0 {
		preambleEnd = starts[0]
	}
	d.blocks = append(d.blocks, d.newBlock(0, -1, preambleEnd))
	for n, h := range headers {
		end := len(d.lines)
		if n+1 < len(starts) {
			end = starts[n+1]
		}
		d.blocks = append(d.blocks, d.newBlock(starts[n], h, end))
	}
	return d
}

func (d *doc) newBlock(start, header, end int) block {
	b := block{start: start, header: header, end: end, keys: map[string]int{},
		lastKey: header, lastContent: header}
	from := start
	if header >= 0 {
		l := d.lines[header]
		b.table, b.array, b.complex = l.table, l.array, l.complex || l.table == ""
		from = header + 1
	}
	for i := from; i < end; i++ {
		l := d.lines[i]
		switch l.kind {
		case keyLine:
			if l.key != "" {
				b.keys[l.key] = i
			}
			b.lastKey = i
		case contLine:
			b.lastKey = i
		case otherLine:
			b.complex = true
		}
		if l.complex {
			b.complex = true
		}
		if l.kind != blankLine {
			b.lastContent = i
		}
	}
	return b
}

// entries returns the blocks of the list of sections named table, in file
// order, and whether a plain [table] (not a list) has that name.
func (d *doc) entries(table string) (list []*block, plain bool) {
	for i := range d.blocks {
		b := &d.blocks[i]
		if b.header >= 0 && b.table == table {
			if b.array {
				list = append(list, b)
			} else {
				plain = true
			}
		}
	}
	return list, plain
}

// section returns the plain [table] block, or nil; the preamble for "".
func (d *doc) section(table string) (*block, int) {
	if table == "" {
		return &d.blocks[0], 1
	}
	var found *block
	n := 0
	for i := range d.blocks {
		b := &d.blocks[i]
		if b.header >= 0 && b.table == table {
			n++
			if !b.array {
				found = b
			}
		}
	}
	return found, n
}

// hasComplexHeader reports whether some header's name couldn't be read, so
// it might name any table.
func (d *doc) hasComplexHeader() bool {
	for _, b := range d.blocks[1:] {
		if b.table == "" {
			return true
		}
	}
	return false
}
