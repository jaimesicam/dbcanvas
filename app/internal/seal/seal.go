// Package seal is DBCanvas's encryption at rest for the SQLite database.
//
// Every column that holds a credential (the canvas design with its passwords, a
// deployment's generated secrets, the bcrypt hash of an account's password, a
// shared-session guest's name and email, the session transcript) is sealed with
// AES-256-GCM under a 32-byte key kept in its own file, outside the database. The
// file holds the raw key bytes; it is generated on first launch, and its path is
// DBCANVAS_ENCRYPTION_KEY_PATH, by default beside the database. A copy of the
// database without the key file is a copy of ciphertext — and so is a backup of the
// database alone: back the key up too, separately.
//
// It is a package of its own, not part of the server's, because the server is not the
// only program that writes a sealed column: dbcanvas_reset_password (cmd/) seals the
// new password hash it writes.
//
// A sealed value is TEXT:
//
//	"v1:" + base64url( 0x01 | keyID[4] | nonce[12] | ciphertext+tag )
//
// keyID is the first 4 bytes of SHA-256(key), so a wrong key is told apart from
// corrupt data. The AAD binds each value to where it belongs
// ("users.password_hash:42"): a ciphertext copied into another row or column does
// not open.
package seal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	// Prefix starts every sealed value, and nothing a plaintext column held before
	// sealing existed (JSON, a bcrypt hash, a name) — which is how a legacy row is
	// recognised and sealed on startup.
	Prefix  = "v1:"
	version = 0x01
	KeySize = 32
	// KeyFile is the key file's name when it sits beside the database.
	KeyFile = "dbcanvas-encryption.key"
	// KeyPathEnv overrides where the key file is.
	KeyPathEnv = "DBCANVAS_ENCRYPTION_KEY_PATH"
)

var (
	ErrWrongKey = errors.New("value was sealed with a different encryption key")
	ErrBadSeal  = errors.New("sealed value is malformed or was tampered with")
)

// Sealer seals and opens values under one key.
type Sealer struct {
	aead cipher.AEAD
	id   [4]byte
}

// New makes a Sealer from raw key bytes.
func New(key []byte) (*Sealer, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("encryption key must be exactly %d raw bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	s := &Sealer{aead: aead}
	sum := sha256.Sum256(key)
	copy(s.id[:], sum[:4])
	return s, nil
}

// KeyID is the key's public fingerprint (8 hex characters), safe to show and log.
func (s *Sealer) KeyID() string { return hex.EncodeToString(s.id[:]) }

// KeyPath is DBCANVAS_ENCRYPTION_KEY_PATH, else the key file beside the database.
func KeyPath(dbPath string) string {
	if v := os.Getenv(KeyPathEnv); v != "" {
		return v
	}
	return filepath.Join(filepath.Dir(dbPath), KeyFile)
}

// GenerateKey returns a new random key.
func GenerateKey() ([]byte, error) {
	k := make([]byte, KeySize)
	_, err := rand.Read(k)
	return k, err
}

// WriteKeyFile writes a key with 0600 permissions, creating its directory 0700.
func WriteKeyFile(path string, key []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(key); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Load reads the key file. A missing file is fs.ErrNotExist.
func Load(path string) (*Sealer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := New(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// LoadOrCreate reads the key file, or generates it when there is none. created
// reports that a new key was written.
func LoadOrCreate(path string) (s *Sealer, created bool, err error) {
	s, err = Load(path)
	if errors.Is(err, fs.ErrNotExist) {
		k, err := GenerateKey()
		if err != nil {
			return nil, false, err
		}
		if err := WriteKeyFile(path, k); err != nil {
			return nil, false, fmt.Errorf("write encryption key %s: %w", path, err)
		}
		s, err := New(k)
		return s, true, err
	}
	if err != nil {
		return nil, false, fmt.Errorf("read encryption key %s: %w", path, err)
	}
	return s, false, nil
}

// Seal encrypts plaintext for the given location (the AAD).
func (s *Sealer) Seal(aad, plaintext string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := make([]byte, 0, 1+4+len(nonce)+len(plaintext)+s.aead.Overhead())
	out = append(out, version)
	out = append(out, s.id[:]...)
	out = append(out, nonce...)
	out = s.aead.Seal(out, nonce, []byte(plaintext), []byte(aad))
	return Prefix + base64.RawURLEncoding.EncodeToString(out), nil
}

// Open decrypts a sealed value. The empty string opens to the empty string, so a
// column that is empty by convention needs no special case.
func (s *Sealer) Open(aad, sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	if !strings.HasPrefix(sealed, Prefix) {
		return "", ErrBadSeal
	}
	raw, err := base64.RawURLEncoding.DecodeString(sealed[len(Prefix):])
	ns := s.aead.NonceSize()
	if err != nil || len(raw) < 1+4+ns+s.aead.Overhead() || raw[0] != version {
		return "", ErrBadSeal
	}
	if string(raw[1:5]) != string(s.id[:]) {
		return "", ErrWrongKey
	}
	pt, err := s.aead.Open(nil, raw[5:5+ns], raw[5+ns:], []byte(aad))
	if err != nil {
		return "", ErrBadSeal
	}
	return string(pt), nil
}

// IsSealed reports whether v is in the sealed format (rather than a legacy plaintext).
func IsSealed(v string) bool { return strings.HasPrefix(v, Prefix) }

// HashSecret is how a bearer secret is stored: SHA-256, hex. A session cookie or an
// API token is high-entropy random, so a fast hash is right — and unlike sealing, a
// lookup by hash needs no key.
func HashSecret(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}
