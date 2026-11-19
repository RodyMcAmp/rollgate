// Command rollgate-release maintains releases.json for the release train
// (ADR 0012, docs/build-and-release.md). CI and maintainers run it; it is
// never installed on a router.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/RodyMcAmp/rollgate/compiler/release"
	"github.com/RodyMcAmp/rollgate/compiler/update"
)

const usage = `usage: rollgate-release <command> FILE [arguments]

  add FILE DATE        make DATE the newest (current) release in FILE, a releases.json
                       (a missing FILE is an empty list)
  withdraw FILE DATE   mark DATE withdrawn; rollgate update stops installing it
  verify FILE FPR      check FILE.sig the way a router does, accepting only the key
                       with fingerprint FPR, with gpg's keyring in $GNUPGHOME
                       (default ~/.gnupg); print the releases
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 3 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, file, arg := args[0], args[1], args[2]
	var err error
	switch cmd {
	case "add":
		err = change(file, func(r update.Releases) (update.Releases, error) { return release.Add(r, arg) })
	case "withdraw":
		err = change(file, func(r update.Releases) (update.Releases, error) { return release.Withdraw(r, arg) })
	case "verify":
		err = verify(file, arg, stdout)
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "rollgate-release %s: %v\n", cmd, err)
		return 1
	}
	return 0
}

// change rewrites file with f's result. The signature is the caller's to
// renew: a stale one would fail on every router, so it is removed.
func change(file string, f func(update.Releases) (update.Releases, error)) error {
	var r update.Releases
	data, err := os.ReadFile(file)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	default:
		if r, err = update.ParseReleases(data); err != nil {
			return err
		}
	}
	if r, err = f(r); err != nil {
		return err
	}
	if err := os.WriteFile(file, release.Marshal(r), 0o644); err != nil {
		return err
	}
	if err := os.Remove(file + ".sig"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func verify(file, fingerprint string, stdout io.Writer) error {
	home := os.Getenv("GNUPGHOME")
	if home == "" {
		dir, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		home = filepath.Join(dir, ".gnupg")
	}
	r, err := update.FetchReleases(os.ReadFile, update.GPGVerify(home), []string{fingerprint}, file)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s: good signature by %s\n", file, fingerprint)
	stdout.Write(release.Marshal(r))
	return nil
}
