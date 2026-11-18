// Command rollgate validates, compiles and applies router.toml.
// See docs/architecture.md#config-flow. M2 delivers validate and compile;
// diff and apply are planned.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/RodyMcAmp/rollgate/compiler/compile"
	"github.com/RodyMcAmp/rollgate/compiler/config"
)

const usage = `usage: rollgate <command> [arguments]

commands:
  validate [file]          check a config (default ` + config.DefaultPath + `)
  compile -o DIR [file]    validate, then write the generated system files below
                           DIR (a new or empty directory); changes nothing else
  diff [-root DIR] [file]  show what installing the compiled files would change
                           below DIR (default /); exit 0 no changes, 1 changes,
                           2 invalid config or error; changes nothing
  schema                   print the JSON Schema for router.toml
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run returns the exit code: 0 success, 1 invalid config or failure, 2 bad usage.
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
		if _, _, ok := load(path, stderr); !ok {
			return 1
		}
		fmt.Fprintf(stdout, "%s: valid\n", path)
		return 0
	case "compile":
		return compileCmd(args[1:], stdout, stderr)
	case "diff":
		return diffCmd(args[1:], stdout, stderr)
	case "schema":
		s, err := config.Schema()
		if err != nil || len(args) > 1 {
			fmt.Fprintf(stderr, "rollgate schema: %v\n%s", err, usage)
			return 2
		}
		stdout.Write(s)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "rollgate: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// diffCmd follows diff(1)'s exit codes: 0 no changes, 1 changes, 2 trouble.
func diffCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", "/", "directory the files are installed below")
	if err := fs.Parse(args); err != nil || fs.NArg() > 1 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	path := config.DefaultPath
	if fs.NArg() == 1 {
		path = fs.Arg(0)
	}
	c, source, ok := load(path, stderr)
	if !ok {
		return 2
	}
	files, err := compile.Compile(c, source, filepath.Base(path))
	var changes []compile.Change
	if err == nil {
		changes, err = files.Compare(*root)
	}
	if err != nil {
		fmt.Fprintf(stderr, "rollgate diff: %v\n", err)
		return 2
	}
	for _, ch := range changes {
		fmt.Fprintf(stdout, "%s: %s\n%s\n", ch.Kind, filepath.Join(*root, filepath.FromSlash(ch.Path)), ch.Diff)
	}
	if len(changes) == 0 {
		fmt.Fprintln(stdout, "no changes")
		return 0
	}
	fmt.Fprintf(stdout, "%d file(s) would change\n", len(changes))
	return 1
}

func compileCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("compile", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("o", "", "output directory (new or empty)")
	if err := fs.Parse(args); err != nil || *out == "" || fs.NArg() > 1 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	path := config.DefaultPath
	if fs.NArg() == 1 {
		path = fs.Arg(0)
	}
	c, source, ok := load(path, stderr)
	if !ok {
		return 1
	}
	files, err := compile.Compile(c, source, filepath.Base(path))
	if err == nil {
		err = files.WriteTree(*out)
	}
	if err != nil {
		fmt.Fprintf(stderr, "rollgate compile: %v\n", err)
		return 1
	}
	for _, p := range files.Paths() {
		fmt.Fprintln(stdout, filepath.Join(*out, filepath.FromSlash(p)))
	}
	return 0
}

// load reads and validates a config, printing every problem to stderr.
func load(path string, stderr io.Writer) (*config.Config, []byte, bool) {
	source, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %s %v\n", path, config.CodeUnreadable, err)
		return nil, nil, false
	}
	c, err := config.Parse(source)
	var problems config.Problems
	switch {
	case err == nil:
		return c, source, true
	case errors.As(err, &problems):
		for _, p := range problems {
			fmt.Fprintf(stderr, "%s: %s\n", path, p)
		}
		fmt.Fprintf(stderr, "%s: %d problem(s); see docs/config-spec.md#validation-rules\n", path, len(problems))
	default:
		fmt.Fprintf(stderr, "%s: %v\n", path, err)
	}
	return nil, nil, false
}
