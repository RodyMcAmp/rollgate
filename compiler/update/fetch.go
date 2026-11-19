package update

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// KeysPath lists the fingerprints of the keys allowed to sign releases.json,
// one per line, relative to the root. The image ships it (ADR 0012).
const KeysPath = "usr/lib/rollgate/release-keys"

// PacmanGnupg is pacman's keyring. releases.json is checked against it, so a
// router has one keyring for packages and releases (ADR 0012).
const PacmanGnupg = "etc/pacman.d/gnupg"

// maxReleasesSize bounds what is downloaded: releases.json is a short list.
const maxReleasesSize = 1 << 20

// Get downloads a URL. HTTPGet is the real one.
type Get func(url string) ([]byte, error)

// HTTPGet downloads url with a time limit, refusing anything but 200 OK and
// anything over 1 MiB.
func HTTPGet(url string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxReleasesSize+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", url, err)
	}
	if len(data) > maxReleasesSize {
		return nil, fmt.Errorf("%s: larger than %d bytes", url, maxReleasesSize)
	}
	return data, nil
}

// Verify checks a detached signature and returns gpg's machine-readable
// status lines (gpg --status-fd). GPGVerify is the real one.
type Verify func(sig, data string) ([]byte, error)

// GPGVerify returns a Verify that checks signatures with gpg against the
// keyring in homedir.
func GPGVerify(homedir string) Verify {
	return func(sig, data string) ([]byte, error) {
		cmd := exec.Command("gpg", "--homedir", homedir, "--batch", "--no-auto-key-retrieve",
			"--status-fd", "1", "--verify", sig, data)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if err != nil && stdout.Len() == 0 {
			return nil, fmt.Errorf("gpg: %v: %s", err, strings.TrimSpace(stderr.String()))
		}
		// A bad signature exits non-zero but still reports why; the status
		// lines decide.
		return stdout.Bytes(), nil
	}
}

// FetchReleases downloads releases.json and releases.json.sig from url,
// checks the signature, and parses the signed bytes. Nothing is trusted
// before the signature checks out.
func FetchReleases(get Get, verify Verify, keys []string, url string) (Releases, error) {
	if len(keys) == 0 {
		return Releases{}, fmt.Errorf("no release signing key is installed (/%s); updates need one (ADR 0012)", KeysPath)
	}
	data, err := get(url)
	if err != nil {
		return Releases{}, err
	}
	sig, err := get(url + ".sig")
	if err != nil {
		return Releases{}, err
	}
	dir, err := os.MkdirTemp("", "rollgate-releases-")
	if err != nil {
		return Releases{}, err
	}
	defer os.RemoveAll(dir)
	dataPath, sigPath := filepath.Join(dir, "releases.json"), filepath.Join(dir, "releases.json.sig")
	if err := os.WriteFile(dataPath, data, 0o600); err != nil {
		return Releases{}, err
	}
	if err := os.WriteFile(sigPath, sig, 0o600); err != nil {
		return Releases{}, err
	}
	status, err := verify(sigPath, dataPath)
	if err != nil {
		return Releases{}, fmt.Errorf("checking the signature of %s: %w", url, err)
	}
	if err := CheckSignature(status, keys); err != nil {
		return Releases{}, fmt.Errorf("%s: %w; nothing was changed", url, err)
	}
	return ParseReleases(data)
}

// CheckSignature reads gpg's status lines and accepts only a good signature,
// by a key that hasn't expired or been revoked, whose key or primary key
// fingerprint is one of keys. pacman's keyring also trusts every Arch
// packager, so a good signature alone isn't enough: it must be ours.
func CheckSignature(status []byte, keys []string) error {
	allowed := map[string]bool{}
	for _, k := range keys {
		allowed[strings.ToUpper(k)] = true
	}
	good, valid, signer := false, false, ""
	sc := bufio.NewScanner(bytes.NewReader(status))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || f[0] != "[GNUPG:]" {
			continue
		}
		switch f[1] {
		case "GOODSIG":
			good = true
		case "VALIDSIG":
			// VALIDSIG <fpr> ... <primary-key-fpr> (gpg doc/DETAILS)
			if len(f) < 3 {
				continue
			}
			signer = f[2]
			if allowed[strings.ToUpper(f[2])] || allowed[strings.ToUpper(f[len(f)-1])] {
				valid = true
			}
		case "BADSIG", "ERRSIG", "EXPSIG", "EXPKEYSIG", "REVKEYSIG", "NO_PUBKEY":
			return fmt.Errorf("signature not accepted (%s)", f[1])
		}
	}
	switch {
	case !good:
		return errors.New("no good signature")
	case !valid:
		return fmt.Errorf("signed by %s, which is not a rollgate release key", signer)
	}
	return nil
}

// ReadKeys reads the allowed signing-key fingerprints below root. A missing
// file means no keys. Blank lines and # comments are skipped.
func ReadKeys(root string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, KeysPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !isFingerprint(line) {
			return nil, fmt.Errorf("/%s: %q is not a full key fingerprint", KeysPath, line)
		}
		keys = append(keys, line)
	}
	return keys, nil
}

// isFingerprint accepts a v4 (40 hex digits) or v5/v6 (64) fingerprint.
// Short key IDs can be forged, so they are refused.
func isFingerprint(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}
