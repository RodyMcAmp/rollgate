package release

import (
	"strings"
	"testing"

	"github.com/RodyMcAmp/rollgate/compiler/update"
)

func dates(r update.Releases) string {
	var s []string
	for _, rel := range r.Releases {
		s = append(s, rel.Date+" "+rel.Status)
	}
	return strings.Join(s, ", ")
}

func TestAdd(t *testing.T) {
	r, err := Add(update.Releases{}, "2026-11-09")
	if err != nil {
		t.Fatal(err)
	}
	if got := dates(r); got != "2026-11-09 current" {
		t.Errorf("first release: %s", got)
	}
	r, err = Add(r, "2026-11-16")
	if err != nil {
		t.Fatal(err)
	}
	if got := dates(r); got != "2026-11-09 previous, 2026-11-16 current" {
		t.Errorf("second release: %s", got)
	}
	for _, bad := range []string{"2026-11-16", "2026-11-10", "2026-02-30", "next week"} {
		if _, err := Add(r, bad); err == nil {
			t.Errorf("Add(%q) accepted", bad)
		}
	}
	// A withdrawn release stays withdrawn when a newer one is added.
	r, _ = Withdraw(r, "2026-11-16")
	r, err = Add(r, "2026-11-23")
	if err != nil {
		t.Fatal(err)
	}
	if got := dates(r); got != "2026-11-09 previous, 2026-11-16 withdrawn, 2026-11-23 current" {
		t.Errorf("after a withdrawn release: %s", got)
	}
}

func TestWithdraw(t *testing.T) {
	var r update.Releases
	for _, d := range []string{"2026-11-02", "2026-11-09", "2026-11-16"} {
		var err error
		if r, err = Add(r, d); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Withdraw(r, "2026-11-16")
	if err != nil {
		t.Fatal(err)
	}
	// The newest release that isn't withdrawn becomes current again.
	if got := dates(r); got != "2026-11-02 previous, 2026-11-09 current, 2026-11-16 withdrawn" {
		t.Errorf("withdrawing the current release: %s", got)
	}
	r, _ = Withdraw(r, "2026-11-02")
	if got := dates(r); got != "2026-11-02 withdrawn, 2026-11-09 current, 2026-11-16 withdrawn" {
		t.Errorf("withdrawing an older one: %s", got)
	}
	if _, err := Withdraw(r, "2026-11-23"); err == nil {
		t.Error("withdrawing an unlisted date accepted")
	}
}

func TestMarshal(t *testing.T) {
	r, _ := Add(update.Releases{}, "2026-11-09")
	r, _ = Add(r, "2026-11-16")
	want := `{"releases": [
  {"date": "2026-11-09", "status": "previous"},
  {"date": "2026-11-16", "status": "current"}
]}
`
	if got := string(Marshal(r)); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	// Routers parse what this writes.
	back, err := update.ParseReleases(Marshal(r))
	if err != nil || dates(back) != dates(r) {
		t.Errorf("round trip: %v, %s", err, dates(back))
	}
	if got := string(Marshal(update.Releases{})); got != "{\"releases\": []}\n" {
		t.Errorf("empty: %q", got)
	}
}
