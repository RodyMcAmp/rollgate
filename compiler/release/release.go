// Package release maintains releases.json for the release train (ADR 0012,
// docs/build-and-release.md). Routers read the file with the update package;
// this package writes it. It runs in CI, never on a router.
package release

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/RodyMcAmp/rollgate/compiler/update"
)

// Add makes date the newest release. It must be newer than every listed
// date: a release is never inserted into the past.
func Add(r update.Releases, date string) (update.Releases, error) {
	for _, rel := range r.Releases {
		if rel.Date >= date {
			return update.Releases{}, fmt.Errorf("%s is not newer than %s, which is already listed", date, rel.Date)
		}
	}
	out := update.Releases{Releases: append(append([]update.Release(nil), r.Releases...), update.Release{Date: date})}
	return normalize(out)
}

// Withdraw marks date as withdrawn: rollgate update no longer installs it.
// If it was the current release, the newest one not withdrawn becomes current.
func Withdraw(r update.Releases, date string) (update.Releases, error) {
	out := update.Releases{Releases: append([]update.Release(nil), r.Releases...)}
	found := false
	for i := range out.Releases {
		if out.Releases[i].Date == date {
			out.Releases[i].Status = update.Withdrawn
			found = true
		}
	}
	if !found {
		return update.Releases{}, fmt.Errorf("%s is not listed", date)
	}
	return normalize(out)
}

// normalize sorts by date and gives every release that isn't withdrawn its
// status: the newest is current, the others previous. So there is at most
// one current release, and rollgate update always picks the newest good one.
func normalize(r update.Releases) (update.Releases, error) {
	sort.Slice(r.Releases, func(i, j int) bool { return r.Releases[i].Date < r.Releases[j].Date })
	current := true
	for i := len(r.Releases) - 1; i >= 0; i-- {
		switch {
		case r.Releases[i].Status == update.Withdrawn:
		case current:
			r.Releases[i].Status, current = update.Current, false
		default:
			r.Releases[i].Status = update.Previous
		}
	}
	// What routers will parse must parse.
	if _, err := update.ParseReleases(Marshal(r)); err != nil {
		return update.Releases{}, err
	}
	return r, nil
}

// Marshal writes releases.json in the documented layout: one release per
// line, oldest first.
func Marshal(r update.Releases) []byte {
	var b bytes.Buffer
	b.WriteString(`{"releases": [`)
	for i, rel := range r.Releases {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "\n  {\"date\": %q, \"status\": %q}", rel.Date, rel.Status)
	}
	if len(r.Releases) > 0 {
		b.WriteByte('\n')
	}
	b.WriteString("]}\n")
	return b.Bytes()
}
