package update

import (
	"strings"

	"github.com/RodyMcAmp/rollgate/compiler/apply"
)

// Clock says whether systemd-timesyncd has set the clock (ADR 0013): synced,
// and a few words for rollgate status and update's error message. It asks
// timedatectl; if that fails, the clock counts as not synchronized.
func Clock(run apply.Runner) (synced bool, detail string) {
	out, err := run.Run("timedatectl", "show", "--property=NTP", "--property=NTPSynchronized")
	if err != nil {
		return false, "unknown (timedatectl failed)"
	}
	props := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	switch {
	case props["NTPSynchronized"] == "yes":
		return true, "synchronized"
	case props["NTP"] == "yes":
		return false, "not synchronized yet (systemd-timesyncd is trying)"
	case props["NTP"] == "no":
		return false, "not synchronized: time sync is off (timedatectl set-ntp true turns it on)"
	}
	return false, "unknown"
}

// ClockHint is added to a failed update's error when the clock isn't
// synchronized: signatures and HTTPS certificates are only valid at the right time.
func ClockHint(run apply.Runner) string {
	if synced, detail := Clock(run); !synced {
		return "\nThe clock is " + detail + ". Checking signatures and downloading over HTTPS need the right time, so that may be why; timedatectl shows the clock."
	}
	return ""
}
