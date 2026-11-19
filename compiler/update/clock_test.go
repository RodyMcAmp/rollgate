package update

import (
	"errors"
	"strings"
	"testing"
)

type timedatectl struct {
	out string
	err error
}

func (t timedatectl) Run(name string, args ...string) ([]byte, error) {
	if name != "timedatectl" || strings.Join(args, " ") != "show --property=NTP --property=NTPSynchronized" {
		return nil, errors.New("unexpected command: " + name + " " + strings.Join(args, " "))
	}
	return []byte(t.out), t.err
}

func TestClock(t *testing.T) {
	for _, tc := range []struct {
		out    string
		err    error
		synced bool
		detail string
	}{
		{"NTP=yes\nNTPSynchronized=yes\n", nil, true, "synchronized"},
		{"NTP=yes\nNTPSynchronized=no\n", nil, false, "not synchronized yet"},
		{"NTP=no\nNTPSynchronized=no\n", nil, false, "time sync is off"},
		{"", errors.New("exit status 1"), false, "timedatectl failed"},
	} {
		synced, detail := Clock(timedatectl{tc.out, tc.err})
		if synced != tc.synced || !strings.Contains(detail, tc.detail) {
			t.Errorf("%q: got %v %q, want %v and %q", tc.out, synced, detail, tc.synced, tc.detail)
		}
		hint := ClockHint(timedatectl{tc.out, tc.err})
		if tc.synced != (hint == "") {
			t.Errorf("%q: hint %q", tc.out, hint)
		}
	}
}
