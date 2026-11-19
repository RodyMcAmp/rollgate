package web

import (
	"crypto/x509"
	"encoding/pem"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPassword(t *testing.T) {
	root := t.TempDir()
	if PasswordSet(root) {
		t.Fatal("a password is set in an empty root")
	}
	if err := SetPassword(root, "too short"); err == nil || !strings.Contains(err.Error(), "at least 10") {
		t.Errorf("short password: %v", err)
	}
	if err := SetPassword(root, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, PasswordPath)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("password file mode %v, want 0600", info.Mode().Perm())
	}
	stored, _ := os.ReadFile(path)
	if strings.Contains(string(stored), "horse") || !strings.HasPrefix(string(stored), "pbkdf2-sha256$600000$") {
		t.Errorf("stored: %q", stored)
	}
	for pw, want := range map[string]bool{"correct horse battery": true, "correct horse batter": false, "": false} {
		ok, err := CheckPassword(string(stored), pw)
		if err != nil || ok != want {
			t.Errorf("CheckPassword(%q): %v %v, want %v", pw, ok, err, want)
		}
	}
	for _, bad := range []string{"", "plain", "md5$1$a$b", "pbkdf2-sha256$x$a$b", "pbkdf2-sha256$1000$!!$b"} {
		if _, err := CheckPassword(bad, "x"); err == nil {
			t.Errorf("CheckPassword accepted stored value %q", bad)
		}
	}
	// Two hashes of one password differ: each has its own salt.
	a, _ := HashPassword("same password here")
	b, _ := HashPassword("same password here")
	if a == b {
		t.Error("two hashes of the same password are equal")
	}
}

func TestCert(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	ips := []netip.Addr{netip.MustParseAddr("192.168.50.2")}
	fp, created, err := EnsureCert(root, []string{"gateway", "gateway.home.arpa"}, ips, now)
	if err != nil || !created {
		t.Fatalf("EnsureCert: %v %v", created, err)
	}
	if len(fp) != 95 || strings.ToUpper(fp) != fp || strings.Count(fp, ":") != 31 {
		t.Errorf("fingerprint %q", fp)
	}
	certPEM, _ := os.ReadFile(filepath.Join(root, CertPath))
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != "gateway" || len(cert.IPAddresses) != 1 || cert.IPAddresses[0].String() != "192.168.50.2" {
		t.Errorf("certificate: CN %q, IPs %v", cert.Subject.CommonName, cert.IPAddresses)
	}
	if err := cert.VerifyHostname("gateway.home.arpa"); err != nil {
		t.Error(err)
	}
	if !cert.NotAfter.After(now.AddDate(9, 0, 0)) || cert.NotBefore.After(now) {
		t.Errorf("valid %v to %v", cert.NotBefore, cert.NotAfter)
	}
	if info, _ := os.Stat(filepath.Join(root, KeyPath)); info.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v, want 0600", info.Mode().Perm())
	}

	// A second call keeps the certificate: its fingerprint is what the user checked.
	fp2, created, err := EnsureCert(root, []string{"other"}, nil, now.Add(time.Hour))
	if err != nil || created || fp2 != fp {
		t.Errorf("second EnsureCert: %q %v %v, want %q kept", fp2, created, err, fp)
	}
}

func TestStatus(t *testing.T) {
	root := t.TempDir()
	if s := Status(root); !strings.Contains(s, "not set") {
		t.Errorf("no password: %q", s)
	}
	if err := SetPassword(root, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if s := Status(root); !strings.Contains(s, "No certificate yet") {
		t.Errorf("no certificate: %q", s)
	}
	fp, _, err := EnsureCert(root, nil, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s := Status(root); !strings.HasSuffix(s, fp) {
		t.Errorf("status %q doesn't end with the fingerprint %q", s, fp)
	}
}
