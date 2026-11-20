package vpn

import (
	"os"
	"path/filepath"
	"testing"
)

// A known pair from wg genkey / wg pubkey.
const (
	knownPrivate = "yAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk="
	knownPublic  = "HIgo9xNzJMWLKASShiTqIybxZ0U3wGLiUeJ1PKf8ykw="
)

func TestPublicKey(t *testing.T) {
	pub, err := PublicKey(knownPrivate + "\n")
	if err != nil || pub != knownPublic {
		t.Errorf("PublicKey: %q %v, want %q", pub, err, knownPublic)
	}
	if _, err := PublicKey("short"); err == nil {
		t.Error("PublicKey of garbage: no error")
	}
}

func TestGenerateKey(t *testing.T) {
	priv, pub, err := GenerateKey()
	if err != nil || !ValidKey(priv) || !ValidKey(pub) || priv == pub {
		t.Fatalf("GenerateKey: %q %q %v", priv, pub, err)
	}
	if again, _ := PublicKey(priv); again != pub {
		t.Errorf("the public key doesn't match the private one")
	}
	if p2, _, _ := GenerateKey(); p2 == priv {
		t.Error("two keys are the same")
	}
}

func TestValidKey(t *testing.T) {
	for k, want := range map[string]bool{knownPublic: true, "": false, "abc=": false, knownPublic[:43]: false, "!" + knownPublic[1:]: false} {
		if ValidKey(k) != want {
			t.Errorf("ValidKey(%q) = %v", k, !want)
		}
	}
}

func TestEnsureKey(t *testing.T) {
	root := t.TempDir()
	if _, ok, err := RouterPublicKey(root); ok || err != nil {
		t.Fatalf("a key before any was made: %v %v", ok, err)
	}
	pub, created, err := EnsureKey(root)
	if err != nil || !created || !ValidKey(pub) {
		t.Fatalf("EnsureKey: %q %v %v", pub, created, err)
	}
	st, err := os.Stat(filepath.Join(root, KeyFile))
	if err != nil || st.Mode().Perm()&0o007 != 0 {
		t.Errorf("the key file is readable by others: %v %v", st.Mode(), err)
	}
	again, created, err := EnsureKey(root)
	if err != nil || created || again != pub {
		t.Errorf("a second EnsureKey made a new key: %q %v %v", again, created, err)
	}
}
