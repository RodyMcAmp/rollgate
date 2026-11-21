package install

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"text/template"

	"github.com/RodyMcAmp/rollgate/compiler/config"
	"github.com/RodyMcAmp/rollgate/compiler/templates"
)

var tmpl = template.Must(template.New("").Option("missingkey=error").ParseFS(templates.FS, "install-router.toml.tmpl"))

// RouterTOML writes the router.toml for c, dated date (YYYY-MM-DD), and checks
// it with the same code as rollgate validate.
func RouterTOML(c Choice, date string) ([]byte, error) {
	start, end := DHCPRange(c.LANAddr)
	var b bytes.Buffer
	err := tmpl.ExecuteTemplate(&b, "install-router.toml.tmpl", struct {
		Choice
		Date       string
		Start, End netip.Addr
	}{c, date, start, end})
	if err != nil {
		return nil, err
	}
	b.WriteString("\n")
	if _, err := Validate(b.Bytes()); err != nil {
		return nil, fmt.Errorf("the router.toml the installer wrote isn't valid (a bug): %w", err)
	}
	return b.Bytes(), nil
}

// Validate checks a router.toml as rollgate validate does, with every problem
// in the error.
func Validate(data []byte) (*config.Config, error) {
	c, err := config.Parse(data)
	var problems config.Problems
	if errors.As(err, &problems) {
		var lines []string
		for _, p := range problems {
			lines = append(lines, p.String())
		}
		return nil, errors.New(strings.Join(lines, "\n"))
	}
	return c, err
}

// AuthorizedKeys checks ssh-key.pub and returns it as an authorized_keys
// file. It accepts what a Windows editor saves (a byte-order mark, CRLF line
// ends) and blank or # lines.
func AuthorizedKeys(data []byte) ([]byte, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	var out bytes.Buffer
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Contains(line, "PRIVATE KEY") {
			return nil, errors.New("ssh-key.pub holds a private key: copy the .pub file instead, and keep the private key to yourself")
		}
		f := strings.Fields(line)
		if len(f) < 2 || !isKeyType(f[0]) {
			return nil, fmt.Errorf("ssh-key.pub: %q doesn't look like an SSH public key (it should start with ssh-ed25519, ssh-rsa or ecdsa-sha2-)", short(line))
		}
		out.WriteString(line + "\n")
	}
	if out.Len() == 0 {
		return nil, errors.New("ssh-key.pub is empty")
	}
	return out.Bytes(), nil
}

func isKeyType(s string) bool {
	switch s {
	case "ssh-ed25519", "ssh-rsa", "sk-ssh-ed25519@openssh.com", "sk-ecdsa-sha2-nistp256@openssh.com":
		return true
	}
	return strings.HasPrefix(s, "ecdsa-sha2-")
}

func short(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}
