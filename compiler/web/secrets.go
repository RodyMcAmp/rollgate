// Package web holds the web UI's secrets (ADR 0014): the admin password's hash
// and the router's own HTTPS certificate, both in /etc/rollgate/secrets/.
package web

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Paths below the root ("/" on the router). The secrets directory is 0700.
const (
	PasswordPath = "etc/rollgate/secrets/web-password"
	CertPath     = "etc/rollgate/secrets/web.crt"
	KeyPath      = "etc/rollgate/secrets/web.key"
)

// MinPassword is the shortest password SetPassword accepts.
const MinPassword = 10

// iterations of PBKDF2-HMAC-SHA256: OWASP's recommendation (2023). About half
// a second on a 2011 CPU, once per login.
const iterations = 600_000

// HashPassword returns the stored form of password:
// "pbkdf2-sha256$<iterations>$<salt>$<hash>", base64 without padding.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return hash(password, salt, iterations)
}

func hash(password string, salt []byte, iter int) (string, error) {
	key, err := pbkdf2.Key(sha256.New, password, salt, iter, 32)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", iter, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// CheckPassword reports whether password matches stored, HashPassword's output.
func CheckPassword(stored, password string) (bool, error) {
	parts := strings.Split(strings.TrimSpace(stored), "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false, errors.New("the stored web password isn't in a known format")
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false, errors.New("the stored web password has a bad iteration count")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false, errors.New("the stored web password has a bad salt")
	}
	want, err := hash(password, salt, iter)
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(strings.TrimSpace(stored))) == 1, nil
}

// SetPassword stores password's hash below root (mode 0600), replacing any old one.
func SetPassword(root, password string) error {
	if len([]rune(password)) < MinPassword {
		return fmt.Errorf("the password must be at least %d characters", MinPassword)
	}
	h, err := HashPassword(password)
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(root, PasswordPath), []byte(h+"\n"), 0o600)
}

// PasswordSet reports whether a web password is stored below root.
func PasswordSet(root string) bool {
	_, err := os.Stat(filepath.Join(root, PasswordPath))
	return err == nil
}

// EnsureCert creates the web UI's self-signed certificate and key below root
// if they don't exist yet. names and ips are what the certificate is for: the
// router's host names and its management addresses. It returns the
// certificate's fingerprint, and whether it was created now.
func EnsureCert(root string, names []string, ips []netip.Addr, now time.Time) (string, bool, error) {
	certPath, keyPath := filepath.Join(root, CertPath), filepath.Join(root, KeyPath)
	if certPEM, err := os.ReadFile(certPath); err == nil {
		if _, err := os.Stat(keyPath); err == nil {
			fp, err := Fingerprint(certPEM)
			return fp, false, err
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", false, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return "", false, err
	}
	cn := "rollgate"
	if len(names) > 0 {
		cn = names[0]
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"rollgate router (self-signed)"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     names,
	}
	for _, ip := range ips {
		tmpl.IPAddresses = append(tmpl.IPAddresses, ip.AsSlice())
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", false, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", false, err
	}
	// The key first: a certificate without its key would be useless.
	if err := writeFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return "", false, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := writeFile(certPath, certPEM, 0o644); err != nil {
		return "", false, err
	}
	fp, err := Fingerprint(certPEM)
	return fp, true, err
}

// Fingerprint is a certificate's SHA-256 fingerprint as browsers show it:
// colon-separated uppercase hex.
func Fingerprint(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("not a PEM certificate")
	}
	sum := sha256.Sum256(block.Bytes)
	hexes := make([]string, len(sum))
	for i, b := range sum {
		hexes[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(hexes, ":"), nil
}

// Status is one line for rollgate status.
func Status(root string) string {
	if !PasswordSet(root) {
		return "Web password: not set (rollgate web-password sets it)."
	}
	certPEM, err := os.ReadFile(filepath.Join(root, CertPath))
	if err != nil {
		return "Web password: set. No certificate yet (rollgate web-password makes one)."
	}
	fp, err := Fingerprint(certPEM)
	if err != nil {
		return fmt.Sprintf("Web password: set. The certificate can't be read: %v.", err)
	}
	return "Web password: set. Certificate fingerprint (SHA-256): " + fp
}

// writeFile replaces path in one step, creating its directory (0700) if needed.
func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
