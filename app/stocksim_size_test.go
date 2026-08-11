package main

import "testing"

// These lock down the halves of stocksim.go that mirror logic inside the sim
// image — the pair that decides how large a Stock Market Sim node grows its
// dataset. Drift between the two is invisible until a deploy behaves
// differently from what the canvas said it would.

func TestStockSimParseSize(t *testing.T) {
	ok := map[string]int64{
		"":       0,
		"0":      0,
		"1024":   1024,
		"5G":     5 << 30,
		"5g":     5 << 30,
		"5GB":    5 << 30,
		"5Gi":    5 << 30,
		"5GiB":   5 << 30,
		"512Mi":  512 << 20,
		"2T":     2 << 40,
		" 1.5G ": 1_610_612_736,
	}
	for in, want := range ok {
		got, err := stockSimParseSize(in)
		if err != nil {
			t.Errorf("stockSimParseSize(%q): unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("stockSimParseSize(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"five", "5X", "-1G", "G", "GiB"} {
		if n, err := stockSimParseSize(bad); err == nil {
			t.Errorf("stockSimParseSize(%q) = %d, want an error", bad, n)
		}
	}
}

func TestStockSimTargetBytes(t *testing.T) {
	cases := []struct {
		name   string
		size   string
		engine string
		want   int64
	}{
		{"blank takes the default", "", "postgres", stockSimDefaultTargetBytes},
		{"explicit size is honoured", "20G", "mysql", 20 << 30},
		{"off disables growth", "off", "mysql", 0},
		{"none disables growth", "None", "mongodb", 0},
		{"zero disables growth", "0", "mongodb", 0},
		// Valkey's tick history is a capped stream: a size target there can
		// never be met, so the node is deployed without one whatever is typed.
		{"valkey never grows", "50G", "valkey", 0},
		{"valkey ignores the default too", "", "valkey", 0},
		// A typo must not quietly change the deployed size — stockSimSizeIssues
		// blocks the deploy, and until it does the default stands.
		{"unparseable falls back to the default", "banana", "mysql", stockSimDefaultTargetBytes},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := designNode{SSTargetSize: c.size}
			if got := stockSimTargetBytes(n, c.engine); got != c.want {
				t.Errorf("stockSimTargetBytes(%q, %q) = %d, want %d", c.size, c.engine, got, c.want)
			}
		})
	}
}

func TestStockSimSizeIssues(t *testing.T) {
	// A size that cannot be met is a warning, not an error: the node still
	// deploys and still works, it just will not grow.
	warn := stockSimSizeIssues(designNode{Label: "sim1", SSTargetSize: "5G"}, "valkey")
	if len(warn) != 1 || warn[0].Level != "warning" {
		t.Fatalf("valkey with a size target: got %+v, want one warning", warn)
	}
	// A size that cannot be parsed is an error, because the deployed size would
	// not be the one the user asked for.
	bad := stockSimSizeIssues(designNode{Label: "sim1", SSTargetSize: "banana"}, "mysql")
	if len(bad) != 1 || bad[0].Level != "error" {
		t.Fatalf("unparseable size: got %+v, want one error", bad)
	}
	// A size too small to push a database off its buffer pool is worth saying
	// out loud, but is not wrong.
	small := stockSimSizeIssues(designNode{Label: "sim1", SSTargetSize: "64Mi"}, "postgres")
	if len(small) != 1 || small[0].Level != "info" {
		t.Fatalf("tiny size: got %+v, want one info", small)
	}
	for _, quiet := range []string{"", "off", "5G", "500G"} {
		if got := stockSimSizeIssues(designNode{Label: "sim1", SSTargetSize: quiet}, "mysql"); len(got) != 0 {
			t.Errorf("size %q on mysql: got %+v, want no issues", quiet, got)
		}
	}
}
