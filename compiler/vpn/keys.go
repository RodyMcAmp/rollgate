// Package vpn holds the WireGuard VPN's keys (ADR 0018): the router's private
// key, a secret in /etc/rollgate/secrets, and key pairs for devices, made
// with the X25519 functions in Go's standard library.
package vpn

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// KeyFile is the router's private key, below the root. systemd-networkd
// reads it (PrivateKeyFile=), so it is root:systemd-network 0640, and its
// directory lets that group through (0710) without listing it. The other
// secrets there stay 0600 root.
const KeyFile = "etc/rollgate/secrets/vpn.key"

// networkGroup is the group systemd-networkd runs as.
const networkGroup = "systemd-network"

// ValidKey reports whether s is a WireGuard key: 32 bytes in standard base64.
func ValidKey(s string) bool {
	b, err := base64.StdEncoding.DecodeString(s)
	return err == nil && len(b) == 32
}

// GenerateKey makes a key pair, both halves base64 as WireGuard writes them.
func GenerateKey() (private, public string, err error) {
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		return "", "", err
	}
	// Clamp it as wg genkey does; X25519 would clamp it anyway.
	k[0] &= 248
	k[31] = k[31]&127 | 64
	priv := base64.StdEncoding.EncodeToString(k[:])
	pub, err := PublicKey(priv)
	return priv, pub, err
}

// PublicKey is the public half of a base64 private key.
func PublicKey(private string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(private))
	if err != nil || len(b) != 32 {
		return "", errors.New("not a WireGuard private key")
	}
	k, err := ecdh.X25519().NewPrivateKey(b)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(k.PublicKey().Bytes()), nil
}

// RouterPublicKey reads the router's key below root and returns its public
// half; ok is false if there is no key yet.
func RouterPublicKey(root string) (pub string, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(root, KeyFile))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	pub, err = PublicKey(string(b))
	return pub, err == nil, err
}

// EnsureKey makes the router's key below root if there is none, with the
// permissions systemd-networkd needs, and returns its public half and whether
// it made it now.
func EnsureKey(root string) (pub string, created bool, err error) {
	if pub, ok, err := RouterPublicKey(root); err != nil || ok {
		return pub, false, err
	}
	gid := -1
	if g, err := user.LookupGroup(networkGroup); err == nil {
		gid, _ = strconv.Atoi(g.Gid)
	}
	dir := filepath.Join(root, filepath.Dir(KeyFile))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", false, err
	}
	if gid >= 0 {
		if err := os.Chown(dir, 0, gid); err != nil {
			return "", false, fmt.Errorf("letting systemd-networkd through %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o710); err != nil {
			return "", false, err
		}
	}
	priv, pub, err := GenerateKey()
	if err != nil {
		return "", false, err
	}
	path := filepath.Join(root, KeyFile)
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(priv+"\n"), 0o600); err != nil {
		return "", false, err
	}
	if gid >= 0 {
		if err := os.Chown(tmp, 0, gid); err != nil {
			os.Remove(tmp)
			return "", false, err
		}
		if err := os.Chmod(tmp, 0o640); err != nil {
			os.Remove(tmp)
			return "", false, err
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", false, err
	}
	return pub, true, nil
}
