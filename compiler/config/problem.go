package config

import (
	"fmt"
	"strings"
)

// Error codes. The table in docs/config-spec.md#validation-rules is the reference;
// never reuse a code for a different meaning.
const (
	CodeOneWAN      = "E001"
	CodeOverlap     = "E002"
	CodeUnknownName = "E003"
	CodeManagement  = "E004"
	CodeDHCP        = "E005"
	CodePortForward = "E006"
	CodeUnknownKey  = "E007"
	CodeIPv6        = "E008"
	CodeInvalid     = "E009"
	CodeMissing     = "E010"
	CodeDuplicate   = "E011"
	CodeUnreadable  = "E012"
)

// Problem is one thing wrong with a config, written for someone who isn't a
// networking expert: what is wrong, where, and how to fix it.
type Problem struct {
	Code string // E001...
	Path string // where in router.toml, e.g. `interface "lan" ipv4`
	Msg  string
}

func (p Problem) String() string {
	if p.Path == "" {
		return p.Code + " " + p.Msg
	}
	return p.Code + " " + p.Path + ": " + p.Msg
}

// Problems is returned by Load when a config is invalid.
type Problems []Problem

func (ps Problems) Error() string {
	lines := make([]string, len(ps))
	for i, p := range ps {
		lines[i] = p.String()
	}
	return strings.Join(lines, "\n")
}

// problems collects Problems during validation.
type problems struct{ list Problems }

func (ps *problems) add(code, path, format string, args ...any) {
	ps.list = append(ps.list, Problem{Code: code, Path: path, Msg: fmt.Sprintf(format, args...)})
}
