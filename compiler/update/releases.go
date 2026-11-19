// Package update implements rollgate update (ADR 0011): choosing a release
// date from the signed releases.json (ADR 0012), preparing the next root and
// arming its trial boot.
package update

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// DefaultReleasesURL is where the release train publishes releases.json and
// its detached signature, releases.json.sig (ADR 0012).
const DefaultReleasesURL = "https://rodymcamp.github.io/rollgate/releases.json"

// DefaultRepoURL is the base of the [rollgate] pacman repository: each release's
// packages are under repo/DATE/x86_64 (ADR 0012).
const DefaultRepoURL = "https://rodymcamp.github.io/rollgate"

// Release statuses in releases.json.
const (
	Current   = "current"
	Previous  = "previous"
	Withdrawn = "withdrawn"
)

// Release is one released date.
type Release struct {
	Date   string `json:"date"`
	Status string `json:"status"`
}

// Releases is the content of releases.json.
type Releases struct {
	Releases []Release `json:"releases"`
}

// ErrUpToDate means the router already runs the newest current release.
var ErrUpToDate = errors.New("already on the newest release")

// ParseReleases parses and checks releases.json: every date is a real
// YYYY-MM-DD date, listed once, with a known status.
func ParseReleases(data []byte) (Releases, error) {
	var r Releases
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Releases{}, fmt.Errorf("releases.json: %w", err)
	}
	seen := map[string]bool{}
	for _, rel := range r.Releases {
		if !validDate(rel.Date) {
			return Releases{}, fmt.Errorf("releases.json: bad date %q", rel.Date)
		}
		if seen[rel.Date] {
			return Releases{}, fmt.Errorf("releases.json: %s is listed twice", rel.Date)
		}
		seen[rel.Date] = true
		switch rel.Status {
		case Current, Previous, Withdrawn:
		default:
			return Releases{}, fmt.Errorf("releases.json: %s has unknown status %q", rel.Date, rel.Status)
		}
	}
	return r, nil
}

// Choose picks the date to update to (ADR 0011, step 1). With want empty, it
// is the newest current release, or ErrUpToDate if that is the running one.
// A date that is given must be listed and not withdrawn. Dates older than the
// running release are refused: going back is rollgate rollback.
func (r Releases) Choose(running, want string) (string, error) {
	if want == "" {
		var current []string
		for _, rel := range r.Releases {
			if rel.Status == Current {
				current = append(current, rel.Date)
			}
		}
		if len(current) == 0 {
			return "", errors.New("releases.json lists no current release")
		}
		sort.Strings(current) // YYYY-MM-DD sorts by date
		want = current[len(current)-1]
		if want == running {
			return "", ErrUpToDate
		}
	} else {
		if !validDate(want) {
			return "", fmt.Errorf("%q is not a date (YYYY-MM-DD)", want)
		}
		var found *Release
		for i := range r.Releases {
			if r.Releases[i].Date == want {
				found = &r.Releases[i]
			}
		}
		switch {
		case found == nil:
			return "", fmt.Errorf("%s is not a rollgate release", want)
		case found.Status == Withdrawn:
			return "", fmt.Errorf("release %s was withdrawn", want)
		}
	}
	if want < running {
		return "", fmt.Errorf("%s is older than the running release %s; to go back, use rollgate rollback", want, running)
	}
	return want, nil
}

func validDate(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil && len(s) == len("2006-01-02")
}

// MirrorlistPath is pacman's mirrorlist, relative to the root. It names the
// Arch Linux Archive date the router installs from (ADR 0002), so it is also
// where the running release is read from.
const MirrorlistPath = "etc/pacman.d/mirrorlist"

// RepoMirrorlistPath names the [rollgate] repository's server, relative to the root.
// The image writes it once the release key exists (ADR 0012).
const RepoMirrorlistPath = "etc/pacman.d/rollgate-mirrorlist"

var archiveServer = regexp.MustCompile(`^\s*Server\s*=\s*https://archive\.archlinux\.org/repos/(\d{4})/(\d{2})/(\d{2})/`)

// RunningRelease returns the release date the system below root installs
// from: the date in the mirrorlist's Server line.
func RunningRelease(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, MirrorlistPath))
	if err != nil {
		return "", err
	}
	date := ""
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		m := archiveServer.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		d := m[1] + "-" + m[2] + "-" + m[3]
		if date != "" && d != date {
			return "", fmt.Errorf("%s names two dates, %s and %s", MirrorlistPath, date, d)
		}
		date = d
	}
	if date == "" || !validDate(date) {
		return "", fmt.Errorf("%s has no Arch Linux Archive date; was it edited by hand?", MirrorlistPath)
	}
	return date, nil
}
