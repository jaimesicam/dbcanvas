package main

import (
	"errors"
	"path/filepath"
	"testing"
)

func stackNameStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("DBCANVAS_ENCRYPTION_KEY_PATH", filepath.Join(t.TempDir(), "key"))
	st, err := OpenStore(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.db.Close() })
	if _, err := st.CreateUser("alice"); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestStackNamesAreUnique(t *testing.T) {
	st := stackNameStore(t)
	design := []byte(`{"nodes":[],"edges":[]}`)
	lab, err := st.CreateStack("lab", "4h", nil, design)
	if err != nil {
		t.Fatal(err)
	}
	// The same name, in any case, is refused.
	for _, n := range []string{"lab", "LAB", "Lab"} {
		if _, err := st.CreateStack(n, "4h", nil, design); !errors.Is(err, ErrStackNameTaken) {
			t.Errorf("%q: %v, want ErrStackNameTaken", n, err)
		}
	}
	// Renaming onto a taken name is refused; keeping your own name is not a clash.
	other, _ := st.CreateStack("other", "4h", nil, design)
	if err := st.UpdateStack(other.ID, "Lab", design); !errors.Is(err, ErrStackNameTaken) {
		t.Errorf("rename onto lab: %v", err)
	}
	if err := st.UpdateStack(lab.ID, "lab", design); err != nil {
		t.Errorf("saving lab under its own name: %v", err)
	}
	if id := st.StackIDByName("LAB"); id != lab.ID {
		t.Errorf("StackIDByName = %d, want %d", id, lab.ID)
	}
	// The database enforces it too, so two racing creates cannot both win.
	if _, err := st.db.Exec("INSERT INTO stacks (name, ttl, status, created_at, design_json) VALUES ('LAB', '4h', 'draft', 'x', '')"); err == nil {
		t.Error("the unique index is missing")
	}
}

func TestFreeStackName(t *testing.T) {
	st := stackNameStore(t)
	d := []byte(`{}`)
	if n := st.FreeStackName("Untitled stack"); n != "Untitled stack" {
		t.Fatal(n)
	}
	st.CreateStack("Untitled stack", "4h", nil, d)
	st.CreateStack("untitled stack 2", "4h", nil, d)
	if n := st.FreeStackName("Untitled stack"); n != "Untitled stack 3" {
		t.Fatalf("got %q", n)
	}
}
