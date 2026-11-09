package main

import (
	"errors"
	"path/filepath"
	"testing"
)

func stackNameStore(t *testing.T) (*Store, User, User) {
	t.Helper()
	t.Setenv("DBCANVAS_ENCRYPTION_KEY_PATH", filepath.Join(t.TempDir(), "key"))
	st, err := OpenStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.db.Close() })
	a, err := st.CreateUser("alice", "x", RoleUser, StatusApproved)
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.CreateUser("bob", "x", RoleUser, StatusApproved)
	if err != nil {
		t.Fatal(err)
	}
	return st, a, b
}

func TestStackNamesAreUniquePerOwner(t *testing.T) {
	st, alice, bob := stackNameStore(t)
	design := []byte(`{"nodes":[],"edges":[]}`)
	lab, err := st.CreateStack("lab", alice.ID, "4h", nil, design)
	if err != nil {
		t.Fatal(err)
	}
	// The same name, in any case, for the same owner is refused…
	for _, n := range []string{"lab", "LAB", "Lab"} {
		if _, err := st.CreateStack(n, alice.ID, "4h", nil, design); !errors.Is(err, ErrStackNameTaken) {
			t.Errorf("%q: %v, want ErrStackNameTaken", n, err)
		}
	}
	// …and allowed for somebody else.
	bobLab, err := st.CreateStack("lab", bob.ID, "4h", nil, design)
	if err != nil {
		t.Fatalf("bob's lab: %v", err)
	}
	// Renaming onto a taken name is refused; keeping your own name is not a clash.
	other, _ := st.CreateStack("other", alice.ID, "4h", nil, design)
	if err := st.UpdateStack(other.ID, "Lab", design); !errors.Is(err, ErrStackNameTaken) {
		t.Errorf("rename onto lab: %v", err)
	}
	if err := st.UpdateStack(lab.ID, "lab", design); err != nil {
		t.Errorf("saving lab under its own name: %v", err)
	}
	// A transfer that would give bob two "lab" stacks is refused…
	if err := st.SetStackOwner(lab.ID, bob.ID); !errors.Is(err, ErrStackNameTaken) {
		t.Errorf("transfer: %v", err)
	}
	// …but an account deletion's bulk handover renames instead of stopping.
	name, err := st.TransferStackKeepingNames(lab.ID, bob.ID, "alice")
	if err != nil || name != "lab (from alice)" {
		t.Fatalf("handover: %q %v", name, err)
	}
	third, _ := st.CreateStack("lab", alice.ID, "4h", nil, design)
	if name, _ := st.TransferStackKeepingNames(third.ID, bob.ID, "alice"); name != "lab (from alice) 2" {
		t.Errorf("second handover: %q", name)
	}
	if id := st.StackIDByName(bob.ID, "LAB"); id != bobLab.ID {
		t.Errorf("StackIDByName = %d, want %d", id, bobLab.ID)
	}
	// The database enforces it too, so two racing creates cannot both win.
	if _, err := st.db.Exec("INSERT INTO stacks (name, owner_id, ttl, status, created_at, design_json) VALUES ('LAB', ?, '4h', 'draft', 'x', '')", bob.ID); err == nil {
		t.Error("the unique index is missing")
	}
}

func TestFreeStackName(t *testing.T) {
	st, alice, _ := stackNameStore(t)
	d := []byte(`{}`)
	if n := st.FreeStackName(alice.ID, "Untitled stack"); n != "Untitled stack" {
		t.Fatal(n)
	}
	st.CreateStack("Untitled stack", alice.ID, "4h", nil, d)
	st.CreateStack("untitled stack 2", alice.ID, "4h", nil, d)
	if n := st.FreeStackName(alice.ID, "Untitled stack"); n != "Untitled stack 3" {
		t.Fatalf("got %q", n)
	}
}
