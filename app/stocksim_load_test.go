package main

import "testing"

// The other half of stocksim_size_test.go: the two knobs that decide how hard a
// Stock Market Sim node works its target once the dataset is big. Same reason
// for pinning them — these mirror sim.ParseWorkingSet and store.ClampThreads
// inside the image, and drift between the two is invisible until a deploy
// behaves differently from what the canvas said it would.

func TestStockSimParseWorkingSet(t *testing.T) {
	ok := map[string]float64{
		"":       stockSimDefaultWorkingSetPct,
		"off":    0,
		"None":   0,
		"50%":    0.5,
		" 25 % ": 0.25,
		"100%":   1,
		"0.5":    0.5,
		"1":      1,
		// An absolute size is valid but resolves to no share here: how much of
		// the dataset "2G" is depends on how big the dataset turns out to be,
		// which only the running sim can measure.
		"2G":    0,
		"512Mi": 0,
	}
	for in, want := range ok {
		got, err := stockSimParseWorkingSet(in)
		if err != nil {
			t.Errorf("stockSimParseWorkingSet(%q): unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("stockSimParseWorkingSet(%q) = %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"half", "-10%", "150%", "-1", "5X", "%"} {
		if n, err := stockSimParseWorkingSet(bad); err == nil {
			t.Errorf("stockSimParseWorkingSet(%q) = %v, want an error", bad, n)
		}
	}
}

func TestStockSimWorkingSet(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		engine string
		want   string
	}{
		{"blank takes the default", "", "mysql", "50%"},
		{"a share is passed through", "80%", "postgres", "80%"},
		{"a size is passed through", "2.5G", "mongodb", "2.5G"},
		{"off is passed through", "off", "mysql", "off"},
		// Nothing to read back out of a capped stream, whatever was typed.
		{"valkey never has one", "50%", "valkey", "off"},
		{"valkey ignores the default too", "", "valkey", "off"},
		// A typo must not quietly deploy something the user did not ask for —
		// stockSimLoadIssues blocks it, and until it does the default stands.
		{"unparseable falls back to the default", "banana", "mysql", "50%"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stockSimWorkingSet(designNode{SSWorkingSet: c.raw}, c.engine)
			if got != c.want {
				t.Errorf("stockSimWorkingSet(%q, %q) = %q, want %q", c.raw, c.engine, got, c.want)
			}
		})
	}
}

func TestStockSimThreads(t *testing.T) {
	for in, want := range map[int]int{
		0:                      stockSimDefaultThreads,
		-4:                     stockSimDefaultThreads,
		1:                      1,
		16:                     16,
		stockSimMaxThreads:     stockSimMaxThreads,
		stockSimMaxThreads + 1: stockSimMaxThreads,
	} {
		if got := stockSimThreads(designNode{SSThreads: in}); got != want {
			t.Errorf("stockSimThreads(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestStockSimLoadIssues(t *testing.T) {
	// A working set that cannot be parsed is an error: the deployed value would
	// not be the one the user asked for.
	bad := stockSimLoadIssues(designNode{Label: "sim1", SSWorkingSet: "banana"}, "mysql")
	if len(bad) != 1 || bad[0].Level != "error" {
		t.Fatalf("unparseable working set: got %+v, want one error", bad)
	}
	// Switching it off is legal, and is exactly the configuration in which a
	// buffer pool appears not to matter — worth saying so.
	off := stockSimLoadIssues(designNode{Label: "sim1", SSWorkingSet: "off"}, "postgres")
	if len(off) != 1 || off[0].Level != "info" {
		t.Fatalf("working set off: got %+v, want one info", off)
	}
	// On an engine with no cold data to read, a working set is ignored rather
	// than wrong — same shape as the size target's own warning.
	valkey := stockSimLoadIssues(designNode{Label: "sim1", SSWorkingSet: "50%"}, "valkey")
	if len(valkey) != 1 || valkey[0].Level != "warning" {
		t.Fatalf("valkey with a working set: got %+v, want one warning", valkey)
	}
	// A thread count past the cap still deploys, capped.
	over := stockSimLoadIssues(designNode{Label: "sim1", SSThreads: 500}, "mysql")
	if len(over) != 1 || over[0].Level != "warning" {
		t.Fatalf("500 threads: got %+v, want one warning", over)
	}
	neg := stockSimLoadIssues(designNode{Label: "sim1", SSThreads: -2}, "mysql")
	if len(neg) != 1 || neg[0].Level != "error" {
		t.Fatalf("negative threads: got %+v, want one error", neg)
	}
	for _, quiet := range []string{"", "50%", "2.5G", "100%"} {
		n := designNode{Label: "sim1", SSWorkingSet: quiet, SSThreads: 8}
		if got := stockSimLoadIssues(n, "mysql"); len(got) != 0 {
			t.Errorf("working set %q on mysql: got %+v, want no issues", quiet, got)
		}
	}
}
