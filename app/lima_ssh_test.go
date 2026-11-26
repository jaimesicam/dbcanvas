package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// TestLimaSSHChecksHostKey: dbcanvas's own ssh connection to a VM goes through only when the
// server's host key is the one Lima pinned for that VM.
func TestLimaSSHChecksHostKey(t *testing.T) {
	newSigner := func() ssh.Signer {
		_, k, _ := ed25519.GenerateKey(rand.Reader)
		s, err := ssh.NewSignerFromKey(k)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	vmKey, otherKey := newSigner(), newSigner()

	// An ssh server on 127.0.0.1 presenting vmKey and accepting any client key.
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }}
	cfg.AddHostKey(vmKey)
	// A second host key of another type, which the client must not settle on: the VM has
	// several, and Lima pins one.
	_, ecKey, _ := ecdsaKeyForTest()
	cfg.AddHostKey(ecKey)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				if sc, chans, reqs, err := ssh.NewServerConn(c, cfg); err == nil {
					go ssh.DiscardRequests(reqs)
					go func() {
						for ch := range chans {
							ch.Reject(ssh.Prohibited, "test")
						}
					}()
					sc.Wait()
				}
			}()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	// A Lima folder with one instance: its ssh config, the client key and the pinned host key.
	home := t.TempDir()
	id := "dbc-0123456789ab"
	dir := filepath.Join(home, id)
	os.MkdirAll(dir, 0o700)
	_, clientKey, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(clientKey, "")
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(home, "user")
	os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600)
	os.WriteFile(filepath.Join(dir, "ssh.config"), []byte(fmt.Sprintf(
		"IdentityFile %q\nHost lima-%s\n  Hostname 127.0.0.1\n  Port %d\n  User someone\n", keyPath, id, port)), 0o600)
	pin := func(k ssh.PublicKey) {
		line := knownhosts.Line([]string{"lima-" + id, knownhosts.Normalize(fmt.Sprintf("127.0.0.1:%d", port))}, k)
		os.WriteFile(filepath.Join(dir, "ssh.known_hosts"), []byte(line+"\n"), 0o600)
	}
	l := &Lima{home: home}

	pin(vmKey.PublicKey())
	c, err := l.sshClient(id)
	if err != nil {
		t.Fatalf("the VM's own host key was refused: %v", err)
	}
	c.Close()

	pin(otherKey.PublicKey())
	if c, err := l.sshClient(id); err == nil {
		c.Close()
		t.Fatal("connected to a server whose host key is not the one Lima pinned")
	} else if !strings.Contains(err.Error(), "key mismatch") {
		t.Fatalf("refused, but not for the host key: %v", err)
	}

	os.Remove(filepath.Join(dir, "ssh.known_hosts"))
	if c, err := l.sshClient(id); err == nil {
		c.Close()
		t.Fatal("connected with no pinned host key at all")
	}
}

func ecdsaKeyForTest() (*ecdsa.PrivateKey, ssh.Signer, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	s, err := ssh.NewSignerFromKey(k)
	return k, s, err
}
