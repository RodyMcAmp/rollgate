package apply

import (
	"bytes"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPrecheckReal runs the real checkers (L2, docs/testing.md). It needs
// root (nft -c talks to the kernel) and nft, dnsmasq, unbound and openssh
// installed, so it only runs when ROLLGATE_REAL_CHECKS=1, which
// make test-netns sets.
func TestPrecheckReal(t *testing.T) {
	if os.Getenv("ROLLGATE_REAL_CHECKS") != "1" {
		t.Skip("set ROLLGATE_REAL_CHECKS=1 to run the real checkers (make test-netns)")
	}
	c, files := compileExample(t)
	root := t.TempDir()
	writeSecret(t, root, "admin.pub", "ssh-ed25519 AAAA admin\n")
	// sshd -t needs a host key; on the router the main config finds the real ones.
	hostKey := filepath.Join(root, "hostkey")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", hostKey).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(root, "etc/ssh"), 0o755); err != nil {
		t.Fatal(err)
	}
	mainConfig := "HostKey " + hostKey + "\nPidFile " + filepath.Join(root, "sshd.pid") + "\n"
	if err := os.WriteFile(filepath.Join(root, "etc/ssh/sshd_config"), []byte(mainConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Precheck(Exec{}, root, c, files); err != nil {
		t.Fatalf("the real checkers reject the compiled example config:\n%v", err)
	}

	// Break each file; each checker must notice its own.
	broken := maps.Clone(files)
	for p, junk := range map[string]string{
		nftablesConf: "this is not nftables syntax\n",
		dnsmasqConf:  "no-such-dnsmasq-option\n",
		unboundConf:  "server:\n\tno-such-unbound-option: yes\n",
		sshdConf:     "NoSuchSshdKeyword yes\n",
	} {
		broken[p] = append(bytes.Clone(files[p]), junk...)
	}
	err := Precheck(Exec{}, root, c, broken)
	if err == nil {
		t.Fatal("the real checkers accepted four broken files")
	}
	for _, what := range []string{"firewall: nft", "DHCP and local names: dnsmasq", "DNS resolver: unbound-checkconf", "SSH: sshd"} {
		if !strings.Contains(err.Error(), what+" rejected") {
			t.Errorf("%s didn't catch its broken file:\n%v", what, err)
		}
	}
}
