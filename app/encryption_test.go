package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dbcanvas/internal/seal"
)

// rawCol reads a column as stored, past the store's sealing.
func rawCol(t *testing.T, s *Store, q string, args ...any) string {
	t.Helper()
	var v sql.NullString
	if err := s.db.QueryRow(q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v.String
}

func TestCredentialsAreSealedAtRest(t *testing.T) {
	app := newTestApp(t)
	st := app.store
	u, err := st.CreateUser("alice", "$2a$10$hash", RoleAdmin, StatusApproved)
	if err != nil {
		t.Fatal(err)
	}
	design := `{"nodes":[{"id":"n1","rootPassword":"hunter2"}]}`
	stack, err := st.CreateStack("s", u.ID, ttlInfinity, nil, []byte(design))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDeployment(Deployment{StackID: stack.ID, NodeID: "n1", State: "running",
		Secrets: json.RawMessage(`{"vncPassword":"s3cret"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession("tok-abc", u.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateShareSession(u.ID, stack.ID, "linkhash", false, false, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateShareGuest(sess.ID, "Bob", "bob@example.net", "cookiehash", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddShareMessage(ShareMessage{SessionID: sess.ID, AuthorKind: "guest", Author: "Bob", Kind: "chat", Body: "the password is hunter2"}); err != nil {
		t.Fatal(err)
	}

	for _, raw := range []string{
		rawCol(t, st, "SELECT password_hash FROM users WHERE id = ?", u.ID),
		rawCol(t, st, "SELECT design_json FROM stacks WHERE id = ?", stack.ID),
		rawCol(t, st, "SELECT secrets_json FROM deployments WHERE stack_id = ?", stack.ID),
		rawCol(t, st, "SELECT name FROM share_guests WHERE session_id = ?", sess.ID),
		rawCol(t, st, "SELECT email FROM share_guests WHERE session_id = ?", sess.ID),
		rawCol(t, st, "SELECT body FROM share_messages WHERE session_id = ?", sess.ID),
	} {
		if !seal.IsSealed(raw) || strings.Contains(raw, "hunter2") || strings.Contains(raw, "Bob") {
			t.Errorf("stored in the clear: %q", raw)
		}
	}
	if raw := rawCol(t, st, "SELECT token FROM sessions WHERE user_id = ?", u.ID); raw != seal.HashSecret("tok-abc") {
		t.Errorf("session token stored as %q, want its hash", raw)
	}

	// …and every read path gives the plaintext back.
	if _, h, err := st.CredByUsername("alice"); err != nil || h != "$2a$10$hash" {
		t.Errorf("CredByUsername = %q, %v", h, err)
	}
	if got, err := st.GetStack(stack.ID); err != nil || string(got.Design) != design {
		t.Errorf("GetStack design = %s, %v", got.Design, err)
	}
	if d, err := st.GetDeployment(stack.ID, "n1"); err != nil || string(d.Secrets) != `{"vncPassword":"s3cret"}` {
		t.Errorf("GetDeployment secrets = %s, %v", d.Secrets, err)
	}
	if ds, err := st.ListDeployments(stack.ID); err != nil || len(ds) != 1 || !strings.Contains(string(ds[0].Secrets), "s3cret") {
		t.Errorf("ListDeployments = %+v, %v", ds, err)
	}
	if su, err := st.SessionUser("tok-abc"); err != nil || su.ID != u.ID {
		t.Errorf("SessionUser = %v, %v", su, err)
	}
	if g, err := st.ShareGuestByCookie("cookiehash"); err != nil || g.Name != "Bob" || g.Email != "bob@example.net" {
		t.Errorf("guest = %+v, %v", g, err)
	}
	if ms, err := st.ListShareMessages(sess.ID, 0); err != nil || len(ms) != 1 || ms[0].Body != "the password is hunter2" || ms[0].Author != "Bob" {
		t.Errorf("messages = %+v, %v", ms, err)
	}
	if err := st.SetUserPassword(u.ID, "$2a$10$new"); err != nil {
		t.Fatal(err)
	}
	if _, h, _ := st.CredByUsername("alice"); h != "$2a$10$new" {
		t.Errorf("after SetUserPassword: %q", h)
	}
	if err := st.DeleteSession("tok-abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SessionUser("tok-abc"); err == nil {
		t.Error("deleted session still signs in")
	}
}

// A database written by a version that did not encrypt is sealed on first open, and
// its sign-ins survive the switch to hashed tokens.
func TestPlaintextRowsAreSealedOnOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// Turn it back into what an older version left behind.
	for _, q := range []string{
		`INSERT INTO users (id, username, password_hash, role, status, created_at) VALUES (7, 'old', '$2a$10$plain', 'admin', 'approved', '2026-01-01T00:00:00Z')`,
		`INSERT INTO stacks (id, name, owner_id, ttl, status, created_at, design_json) VALUES (3, 's', 7, 'infinity', 'draft', '2026-01-01T00:00:00Z', '{"nodes":[]}')`,
		`INSERT INTO deployments (stack_id, node_id, state, secrets_json) VALUES (3, 'n1', 'running', '{"p":"x"}')`,
		`INSERT INTO deployments (stack_id, node_id, state, secrets_json) VALUES (3, 'n2', 'running', NULL)`,
		`INSERT INTO sessions (token, user_id, expires_at) VALUES ('plaintok', 7, '2099-01-01T00:00:00Z')`,
		`DELETE FROM app_settings WHERE key = 'sessions_hashed'`,
	} {
		if _, err := st.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	st.Close()

	for i := 0; i < 2; i++ { // the second open must change nothing
		st, err = OpenStore(path)
		if err != nil {
			t.Fatal(err)
		}
		if raw := rawCol(t, st, "SELECT password_hash FROM users WHERE id = 7"); !seal.IsSealed(raw) {
			t.Fatalf("open %d: hash left as %q", i, raw)
		}
		if _, h, err := st.CredByUsername("old"); err != nil || h != "$2a$10$plain" {
			t.Fatalf("open %d: hash = %q, %v", i, h, err)
		}
		if s, err := st.GetStack(3); err != nil || string(s.Design) != `{"nodes":[]}` {
			t.Fatalf("open %d: design = %s, %v", i, s.Design, err)
		}
		if d, err := st.GetDeployment(3, "n1"); err != nil || string(d.Secrets) != `{"p":"x"}` {
			t.Fatalf("open %d: secrets = %s, %v", i, d.Secrets, err)
		}
		if d, err := st.GetDeployment(3, "n2"); err != nil || d.Secrets != nil {
			t.Fatalf("open %d: NULL secrets = %s, %v", i, d.Secrets, err)
		}
		if u, err := st.SessionUser("plaintok"); err != nil || u.ID != 7 {
			t.Fatalf("open %d: existing session lost: %v", i, err)
		}
		st.Close()
	}
}

func TestOpenRefusesAnotherKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	kp := seal.KeyPath(path)

	// Missing: refused, and no stray new key is left behind.
	orig, _ := os.ReadFile(kp)
	os.Remove(kp)
	if _, err := OpenStore(path); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing key: %v", err)
	}
	if _, err := os.Stat(kp); !os.IsNotExist(err) {
		t.Fatal("a generated key was left where the original belongs")
	}
	// Different: refused.
	other, _ := seal.GenerateKey()
	seal.WriteKeyFile(kp, other)
	if _, err := OpenStore(path); err == nil || !strings.Contains(err.Error(), "restore the original") {
		t.Fatalf("other key: %v", err)
	}
	seal.WriteKeyFile(kp, orig)
	st, err = OpenStore(path)
	if err != nil {
		t.Fatalf("original key: %v", err)
	}
	st.Close()
}

func TestRotateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "r.db")
	st, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := st.CreateUser("alice", "$2a$10$hash", RoleAdmin, StatusApproved)
	stack, _ := st.CreateStack("s", u.ID, ttlInfinity, nil, []byte(`{"pw":"x"}`))
	old := st.seal.KeyID()
	st.Close()

	if err := rotateKey(path); err != nil {
		t.Fatal(err)
	}
	kp := seal.KeyPath(path)
	if _, err := os.Stat(kp + ".old"); err != nil {
		t.Fatal("old key not kept")
	}
	st, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if st.seal.KeyID() == old {
		t.Fatal("key did not change")
	}
	if _, h, err := st.CredByUsername("alice"); err != nil || h != "$2a$10$hash" {
		t.Fatalf("after rotation: %q, %v", h, err)
	}
	if s, err := st.GetStack(stack.ID); err != nil || string(s.Design) != `{"pw":"x"}` {
		t.Fatalf("after rotation: %s, %v", s.Design, err)
	}
}
