package apply

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/RodyMcAmp/rollgate/compiler/compile"
	"github.com/RodyMcAmp/rollgate/compiler/config"
)

// Runner runs a program and returns its combined output. Exec runs real
// programs; tests use a fake that records the commands.
type Runner interface {
	Run(name string, args ...string) ([]byte, error)
}

// Exec runs programs on this system.
type Exec struct{}

// Run runs name with args and returns its combined output.
func (Exec) Run(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// SecretsDir holds the files router.toml refers to by name, relative to the
// root (AGENTS.md hard rule 8).
const SecretsDir = "etc/rollgate/secrets"

// Paths of the compiled files that have a checker or a reload action.
const (
	nftablesConf = "etc/nftables.conf"
	sysctlConf   = "etc/sysctl.d/90-rollgate.conf"
	dnsmasqConf  = "etc/dnsmasq.conf"
	unboundConf  = "etc/unbound/unbound.conf"
	sshdConf     = "etc/ssh/sshd_config.d/10-rollgate.conf"
	networkDir   = "etc/systemd/network/"
)

// Precheck checks compiled files before any of them is installed, with each
// program's own checker (ADR 0010): nft -c, dnsmasq --test,
// unbound-checkconf and sshd -t. sshd reads the new file the way it will be
// read once installed: before root/etc/ssh/sshd_config. Precheck also checks
// that every SSH key file the config names exists below root and isn't empty,
// so sshd never switches to a key that isn't there.
//
// It returns every problem found, joined, or nil. It changes nothing on the
// system: the files are checked from a temporary directory.
func Precheck(r Runner, root string, c *config.Config, files compile.Files) error {
	stage, err := os.MkdirTemp("", "rollgate-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := files.WriteTree(stage); err != nil {
		return err
	}
	at := func(p string) string { return filepath.Join(stage, filepath.FromSlash(p)) }

	var problems []error
	check := func(what, name string, args ...string) {
		if out, err := r.Run(name, args...); err != nil {
			problems = append(problems, fmt.Errorf("%s: %s rejected the new config (%v)%s", what, name, err, indent(out)))
		}
	}
	if files[nftablesConf] != nil {
		check("firewall", "nft", "-c", "-f", at(nftablesConf))
	}
	if files[dnsmasqConf] != nil {
		check("DHCP and local names", "dnsmasq", "--test", "--conf-file="+at(dnsmasqConf))
	}
	if files[unboundConf] != nil {
		check("DNS resolver", "unbound-checkconf", at(unboundConf))
	}
	if files[sshdConf] != nil {
		main := filepath.Join(stage, "sshd_config")
		content := fmt.Sprintf("Include %s\nInclude %s\n", at(sshdConf), filepath.Join(root, "etc/ssh/sshd_config"))
		if err := os.WriteFile(main, []byte(content), 0o644); err != nil {
			return err
		}
		check("SSH", "sshd", "-t", "-f", main)
	}

	for _, name := range c.Management.SSHAuthorizedKeys {
		path := filepath.Join(root, filepath.FromSlash(SecretsDir), name)
		fi, err := os.Stat(path)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
			problems = append(problems, fmt.Errorf("SSH: management.ssh_authorized_keys names %q, but /%s/%s is missing or empty; "+
				"applying would leave SSH without that key, so put the public key there first", name, SecretsDir, name))
		}
	}
	return errors.Join(problems...)
}

// indent formats a program's output to follow an error message.
func indent(out []byte) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	return "\n    " + strings.ReplaceAll(s, "\n", "\n    ")
}
