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
	u, err := st.CreateUser("alice")
	if err != nil {
		t.Fatal(err)
	}
	design := `{"nodes":[{"id":"n1","rootPassword":"hunter2"}]}`
	stack, err := st.CreateStack("s", ttlLongest, nil, []byte(design))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertDeployment(Deployment{StackID: stack.ID, NodeID: "n1", State: "running",
		Secrets: json.RawMessage(`{"vncPassword":"s3cret"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSession("tok-abc", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	sess, err := st.CreateShareSession(stack.ID, "linkhash", false, false, time.Now().Add(time.Hour))
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
	if raw := rawCol(t, st, "SELECT token FROM sessions"); raw != seal.HashSecret("tok-abc") {
		t.Errorf("session token stored as %q, want its hash", raw)
	}

	// …and every read path gives the plaintext back.
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
	if err := st.DeleteSession("tok-abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SessionUser("tok-abc"); err == nil {
		t.Error("deleted session still signs in")
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
	u, _ := st.CreateUser("alice")
	st.SetUserProfile(u.ID, Profile{FirstName: "Alice", LastName: "A", Email: "alice@example.com"})
	stack, _ := st.CreateStack("s", ttlLongest, nil, []byte(`{"pw":"x"}`))
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
	if got, err := st.GetUser(u.ID); err != nil || got.Email != "alice@example.com" {
		t.Fatalf("after rotation: %q, %v", got.Email, err)
	}
	if s, err := st.GetStack(stack.ID); err != nil || string(s.Design) != `{"pw":"x"}` {
		t.Fatalf("after rotation: %s, %v", s.Design, err)
	}
}
