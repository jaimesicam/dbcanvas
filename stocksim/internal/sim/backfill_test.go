package sim

import (
	"testing"
	"time"

	"stocksim/internal/store"
)

func TestParseSize(t *testing.T) {
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
		got, err := ParseSize(in)
		if err != nil {
			t.Errorf("ParseSize(%q): unexpected error %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSize(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"five", "5X", "-1G", "G", "GiB"} {
		if n, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) = %d, want an error", bad, n)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	for in, want := range map[int64]string{
		0:             "0 B",
		999:           "999 B",
		5 << 30:       "5.00 GiB",
		512 << 20:     "512.0 MiB",
		3 << 40:       "3.00 TiB",
		1<<30 + 1<<29: "1.50 GiB",
	} {
		if got := FormatBytes(in); got != want {
			t.Errorf("FormatBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestBuildHistoryWalksBackwards pins the two properties the backfill depends
// on: every generated tick is in the past relative to where it started, and
// each security gets an equal share of the batch. A batch that drifted forward
// in time would overwrite the live sparklines it is supposed to sit behind.
func TestBuildHistoryWalksBackwards(t *testing.T) {
	secs := []store.Security{
		{ID: "a", Symbol: "AAA", Sector: "Technology", LastPrice: 100},
		{ID: "b", Symbol: "BBB", Sector: "Utilities", LastPrice: 50},
		{ID: "c", Symbol: "CCC", Sector: "Energy", LastPrice: 7},
	}
	end := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	ticks := buildHistory(secs, end, 30, newAgentRand())

	if len(ticks) != 30 {
		t.Fatalf("got %d ticks, want 30", len(ticks))
	}
	perSec := map[string]int{}
	for _, tk := range ticks {
		if tk.TS.After(end) {
			t.Fatalf("tick at %s is after the batch's end %s", tk.TS, end)
		}
		if tk.Price < 0.01 {
			t.Errorf("tick for %s priced at %v, want at least 0.01", tk.Symbol, tk.Price)
		}
		if tk.SecurityID == "" || tk.Symbol == "" {
			t.Errorf("tick is missing its security: %+v", tk)
		}
		perSec[tk.SecurityID]++
	}
	for _, s := range secs {
		if perSec[s.ID] != 10 {
			t.Errorf("security %s got %d of 30 ticks, want 10", s.ID, perSec[s.ID])
		}
	}
	// 30 ticks over 3 securities is 10 seconds of history.
	if want := end.Add(-9 * time.Second); !ticks[len(ticks)-1].TS.Equal(want) {
		t.Errorf("last tick at %s, want %s", ticks[len(ticks)-1].TS, want)
	}
	if buildHistory(nil, end, 10, newAgentRand()) != nil {
		t.Error("buildHistory with no securities should return nothing")
	}
}
