package update

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	ourKey    = "0123456789ABCDEF0123456789ABCDEF01234567"
	ourSubkey = "89ABCDEF0123456789ABCDEF0123456789ABCDEF"
	archKey   = "FEDCBA9876543210FEDCBA9876543210FEDCBA98"
)

func validsig(fpr, primary string) string {
	return "[GNUPG:] VALIDSIG " + fpr + " 2026-11-16 1763251200 0 4 0 22 10 00 " + primary + "\n"
}

func TestCheckSignature(t *testing.T) {
	good := "[GNUPG:] NEWSIG\n[GNUPG:] GOODSIG 0123456789ABCDEF rollgate releases\n"
	for _, tc := range []struct {
		name, status, err string
	}{
		{"our key", good + validsig(ourKey, ourKey), ""},
		{"our subkey, primary key pinned", good + validsig(ourSubkey, ourKey), ""},
		{"lower-case pin", good + validsig(strings.ToLower(ourKey), ourKey), ""},
		{"an Arch packager's key", good + validsig(archKey, archKey), "not a rollgate release key"},
		{"bad signature", "[GNUPG:] BADSIG 0123456789ABCDEF rollgate releases\n", "BADSIG"},
		{"expired key", "[GNUPG:] EXPKEYSIG 0123456789ABCDEF rollgate releases\n" + validsig(ourKey, ourKey), "EXPKEYSIG"},
		{"revoked key", "[GNUPG:] REVKEYSIG 0123456789ABCDEF rollgate releases\n" + validsig(ourKey, ourKey), "REVKEYSIG"},
		{"unknown key", "[GNUPG:] ERRSIG 0123456789ABCDEF 22 10 00 1763251200 9 -\n[GNUPG:] NO_PUBKEY 0123456789ABCDEF\n", "ERRSIG"},
		{"valid but not good", validsig(ourKey, ourKey), "no good signature"},
		{"nothing", "", "no good signature"},
	} {
		err := CheckSignature([]byte(tc.status), []string{ourKey})
		switch {
		case tc.err == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("%s: err %v, want %q", tc.name, err, tc.err)
		}
	}
}

func TestFetchReleases(t *testing.T) {
	files := map[string]string{"/releases.json": sample, "/releases.json.sig": "SIG"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	url := srv.URL + "/releases.json"

	var verified []string
	verifyAs := func(status string) Verify {
		return func(sig, data string) ([]byte, error) {
			s, _ := os.ReadFile(sig)
			d, _ := os.ReadFile(data)
			verified = []string{string(s), string(d)}
			return []byte(status), nil
		}
	}
	good := "[GNUPG:] GOODSIG 0123456789ABCDEF rollgate releases\n" + validsig(ourKey, ourKey)

	r, err := FetchReleases(HTTPGet, verifyAs(good), []string{ourKey}, url)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Releases) != 4 {
		t.Errorf("got %+v", r)
	}
	if verified[0] != "SIG" || verified[1] != sample {
		t.Errorf("verified %q", verified)
	}

	if _, err := FetchReleases(HTTPGet, verifyAs("[GNUPG:] BADSIG x\n"), []string{ourKey}, url); err == nil ||
		!strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("bad signature: %v", err)
	}
	if _, err := FetchReleases(HTTPGet, verifyAs(good), nil, url); err == nil ||
		!strings.Contains(err.Error(), "no release signing key") {
		t.Errorf("no keys: %v", err)
	}
	failing := func(sig, data string) ([]byte, error) { return nil, errors.New("gpg missing") }
	if _, err := FetchReleases(HTTPGet, failing, []string{ourKey}, url); err == nil {
		t.Error("gpg failure: accepted")
	}
	delete(files, "/releases.json.sig")
	if _, err := FetchReleases(HTTPGet, verifyAs(good), []string{ourKey}, url); err == nil ||
		!strings.Contains(err.Error(), "404") {
		t.Errorf("missing signature: %v", err)
	}
	files["/releases.json"] = strings.Repeat("x", maxReleasesSize+1)
	if _, err := HTTPGet(url); err == nil {
		t.Error("oversized download: accepted")
	}
}

func TestReadKeys(t *testing.T) {
	root := t.TempDir()
	if keys, err := ReadKeys(root); err != nil || keys != nil {
		t.Errorf("no file: %v, %v", keys, err)
	}
	path := filepath.Join(root, KeysPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("# rollgate release keys\n\n" + ourKey + "\n")
	if keys, err := ReadKeys(root); err != nil || len(keys) != 1 || keys[0] != ourKey {
		t.Errorf("got %v, %v", keys, err)
	}
	write("0123456789ABCDEF\n") // a short key ID
	if _, err := ReadKeys(root); err == nil {
		t.Error("short key ID accepted")
	}
}

// TestGPGVerifyReal signs a file with a throwaway key and checks it with the
// real gpg, so the status-line parsing is tested against gpg itself.
func TestGPGVerifyReal(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not installed")
	}
	home, err := os.MkdirTemp("", "gpg") // short path: gpg-agent's socket path is limited
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	defer func() { _ = exec.Command("gpgconf", "--homedir", home, "--kill", "all").Run() }()
	gpg := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("gpg", append([]string{"--homedir", home, "--batch", "--pinentry-mode", "loopback", "--passphrase", ""}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("gpg %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	gpg("--quick-generate-key", "rollgate test <test@invalid>", "ed25519", "sign", "never")
	var fpr string
	for _, line := range strings.Split(gpg("--with-colons", "--list-secret-keys"), "\n") {
		if f := strings.Split(line, ":"); f[0] == "fpr" && fpr == "" {
			fpr = f[9]
		}
	}
	data := filepath.Join(home, "releases.json")
	if err := os.WriteFile(data, []byte(sample), 0o600); err != nil {
		t.Fatal(err)
	}
	gpg("--detach-sign", "--output", data+".sig", data)

	status, err := GPGVerify(home)(data+".sig", data)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckSignature(status, []string{fpr}); err != nil {
		t.Errorf("good signature refused: %v\n%s", err, status)
	}
	if err := CheckSignature(status, []string{archKey}); err == nil {
		t.Error("signature by an unpinned key accepted")
	}
	if err := os.WriteFile(data, []byte(sample+" "), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err = GPGVerify(home)(data+".sig", data)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckSignature(status, []string{fpr}); err == nil {
		t.Errorf("changed file accepted:\n%s", status)
	}
}
