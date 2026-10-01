package seal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestSealer(t *testing.T) *Sealer {
	t.Helper()
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSealOpenRoundTrip(t *testing.T) {
	s := newTestSealer(t)
	v, err := s.Seal("users.password_hash:1", "$2a$10$secret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(v, Prefix) || strings.Contains(v, "secret") {
		t.Fatalf("sealed value %q is not opaque", v)
	}
	pt, err := s.Open("users.password_hash:1", v)
	if err != nil || pt != "$2a$10$secret" {
		t.Fatalf("Open = %q, %v", pt, err)
	}
	// Two seals of the same value differ: the nonce is random.
	if w, _ := s.Seal("users.password_hash:1", "$2a$10$secret"); w == v {
		t.Fatal("sealing twice gave the same ciphertext")
	}
	if pt, err := s.Open("x", ""); pt != "" || err != nil {
		t.Fatalf("empty opens to %q, %v", pt, err)
	}
}

func TestOpenRefusesWrongPlace(t *testing.T) {
	s := newTestSealer(t)
	v, _ := s.Seal("stacks.design_json:1", "{}")
	if _, err := s.Open("stacks.design_json:2", v); !errors.Is(err, ErrBadSeal) {
		t.Fatalf("a value moved to another row opened: %v", err)
	}
}

func TestOpenRefusesWrongKey(t *testing.T) {
	a, b := newTestSealer(t), newTestSealer(t)
	v, _ := a.Seal("aad", "x")
	if _, err := b.Open("aad", v); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("other key: %v", err)
	}
}

func TestOpenRefusesTamperAndPlaintext(t *testing.T) {
	s := newTestSealer(t)
	v, _ := s.Seal("aad", "hello")
	last := v[len(v)-1]
	flip := byte('A')
	if last == 'A' {
		flip = 'B'
	}
	if _, err := s.Open("aad", v[:len(v)-1]+string(flip)); !errors.Is(err, ErrBadSeal) {
		t.Fatalf("tampered: %v", err)
	}
	if _, err := s.Open("aad", `{"plain":true}`); !errors.Is(err, ErrBadSeal) {
		t.Fatalf("plaintext: %v", err)
	}
}

func TestLoadOrCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", KeyFile)
	s, created, err := LoadOrCreate(path)
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 || fi.Size() != KeySize {
		t.Fatalf("key file mode %v size %d", fi.Mode().Perm(), fi.Size())
	}
	if di, _ := os.Stat(filepath.Dir(path)); di.Mode().Perm() != 0o700 {
		t.Fatalf("key dir mode %v", di.Mode().Perm())
	}
	again, created, err := LoadOrCreate(path)
	if err != nil || created || again.KeyID() != s.KeyID() {
		t.Fatalf("reload: %v %v %s vs %s", created, err, again.KeyID(), s.KeyID())
	}
	os.WriteFile(path, []byte("short"), 0o600)
	if _, _, err := LoadOrCreate(path); err == nil {
		t.Fatal("a key of the wrong size loaded")
	}
}

func TestKeyPath(t *testing.T) {
	t.Setenv(KeyPathEnv, "")
	if got := KeyPath("/data/dbcanvas.db"); got != "/data/"+KeyFile {
		t.Fatalf("default: %s", got)
	}
	t.Setenv(KeyPathEnv, "/keys/k")
	if got := KeyPath("/data/dbcanvas.db"); got != "/keys/k" {
		t.Fatalf("env: %s", got)
	}
}
