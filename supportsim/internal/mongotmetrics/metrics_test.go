package mongotmetrics

import (
	"os"
	"strings"
	"testing"
)

// testdata/metrics.txt is a real :9946/metrics from Percona Search for MongoDB 1.70.4.
func TestSummarizeRealEndpoint(t *testing.T) {
	f, err := os.Open("testdata/metrics.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s := Summarize(Parse(f))
	if s.Series < 1000 {
		t.Fatalf("parsed %d series", s.Series)
	}
	if s.VectorSearch.Count == 0 || s.VectorSearch.P50 <= 0 || s.VectorSearch.P99 < s.VectorSearch.P50 {
		t.Errorf("vectorSearch latency: %+v", s.VectorSearch)
	}
	if s.HeapUsed <= 0 || s.HeapMax <= 0 {
		t.Errorf("heap %v / %v", s.HeapUsed, s.HeapMax)
	}
	if len(s.Indexes) != 4 {
		t.Fatalf("want the desk's 4 indexes, got %d", len(s.Indexes))
	}
	for id, x := range s.Indexes {
		if x.SizeBytes <= 0 || x.Status != "STEADY" || (x.Type != "vector_search" && x.Type != "search") {
			t.Errorf("index %s: %+v", id, x)
		}
	}
}

func TestParseLabelsWithCommasAndQuotes(t *testing.T) {
	s := Parse(strings.NewReader(`m{a="x,y",b="z"} 2.5E1
# HELP ignored
plain 3`))
	if len(s) != 2 || s[0].Labels["a"] != "x,y" || s[0].Labels["b"] != "z" || s[0].Value != 25 || s[1].Name != "plain" || s[1].Value != 3 {
		t.Errorf("%+v", s)
	}
}
