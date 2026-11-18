// Command rollgate validates, compiles and applies router.toml.
// See docs/architecture.md#config-flow. Only `validate` exists so far (M2).
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/RodyMcAmp/rollgate/compiler/config"
)

const usage = `usage: rollgate <command> [arguments]

commands:
  validate [file]   check a config (default ` + config.DefaultPath + `)
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run returns the exit code: 0 success, 1 invalid config, 2 bad usage.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "validate":
		if len(args) > 2 {
			fmt.Fprint(stderr, usage)
			return 2
		}
		path := config.DefaultPath
		if len(args) == 2 {
			path = args[1]
		}
		return validate(path, stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "rollgate: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func validate(path string, stdout, stderr io.Writer) int {
	_, err := config.Load(path)
	var problems config.Problems
	switch {
	case err == nil:
		fmt.Fprintf(stdout, "%s: valid\n", path)
		return 0
	case errors.As(err, &problems):
		for _, p := range problems {
			fmt.Fprintf(stderr, "%s: %s\n", path, p)
		}
		fmt.Fprintf(stderr, "%s: %d problem(s); see docs/config-spec.md#validation-rules\n", path, len(problems))
		return 1
	default:
		fmt.Fprintf(stderr, "%s: %v\n", path, err)
		return 1
	}
}
