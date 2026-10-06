// Package mongotmetrics reads mongot's Prometheus endpoint (:9946/metrics) and pulls
// out the handful of numbers worth looking at while learning vector search: how long
// $vectorSearch and $search take inside mongot, how big each index is and how many
// documents it holds, how far behind the change stream it is, and the JVM heap.
//
// The endpoint has ~3,400 series, most of them thread-pool gauges. The per-index ones
// are labelled with the index's id (indexId_logString), the same id $listSearchIndexes
// reports, which is how a size on this page gets an index name next to it.
package mongotmetrics

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Sample is one line of the exposition format.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// Parse reads the Prometheus text format, skipping comments.
func Parse(r io.Reader) []Sample {
	var out []Sample
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		ln := strings.TrimSpace(sc.Text())
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		name, labels, rest := ln, map[string]string{}, ""
		if i := strings.IndexByte(ln, '{'); i >= 0 {
			j := strings.LastIndexByte(ln, '}')
			if j < i {
				continue
			}
			name, rest = ln[:i], strings.TrimSpace(ln[j+1:])
			for _, kv := range splitLabels(ln[i+1 : j]) {
				if k, v, ok := strings.Cut(kv, "="); ok {
					labels[k] = strings.Trim(v, `"`)
				}
			}
		} else if k, v, ok := strings.Cut(ln, " "); ok {
			name, rest = k, v
		}
		if f := strings.Fields(rest); len(f) > 0 {
			if v, err := strconv.ParseFloat(f[0], 64); err == nil {
				out = append(out, Sample{Name: name, Labels: labels, Value: v})
			}
		}
	}
	return out
}

// splitLabels splits a="x",b="y,z" on commas outside quotes.
func splitLabels(s string) []string {
	var out []string
	inQ, start := false, 0
	for i, r := range s {
		switch r {
		case '"':
			inQ = !inQ
		case ',':
			if !inQ {
				out = append(out, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	if start < len(s) {
		out = append(out, strings.TrimSpace(s[start:]))
	}
	return out
}

// Command is a mongot command's latency summary.
type Command struct {
	P50, P90, P99 float64 // seconds
	Count         float64
	Failures      float64
}

// Index is one index's numbers, keyed by its id.
type Index struct {
	ID        string  `json:"id"`
	Type      string  `json:"type"`
	SizeBytes float64 `json:"sizeBytes"`
	Docs      float64 `json:"docs"`
	LagMs     float64 `json:"lagMs"`
	Inserts   float64 `json:"inserts"`
	Updates   float64 `json:"updates"`
	Deletes   float64 `json:"deletes"`
	Status    string  `json:"status"`
}

// Summary is what the dashboard shows for one mongot.
type Summary struct {
	Endpoint     string           `json:"endpoint"`
	Error        string           `json:"error,omitempty"`
	VectorSearch Command          `json:"vectorSearch"`
	Search       Command          `json:"search"`
	HeapUsed     float64          `json:"heapUsed"`
	HeapMax      float64          `json:"heapMax"`
	Indexes      map[string]Index `json:"indexes"`
	Series       int              `json:"series"`
	Raw          []string         `json:"raw"` // the interesting lines, for the raw view
}

// Summarize turns samples into a Summary.
func Summarize(samples []Sample) Summary {
	s := Summary{Indexes: map[string]Index{}, Series: len(samples)}
	idx := func(id string) Index {
		x := s.Indexes[id]
		x.ID = id
		return x
	}
	cmd := func(c *Command, sm Sample, prefix string) {
		switch sm.Name {
		case prefix + "CommandTotalLatency_seconds":
			switch sm.Labels["quantile"] {
			case "0.5":
				c.P50 = sm.Value
			case "0.9":
				c.P90 = sm.Value
			case "0.99":
				c.P99 = sm.Value
			}
		case prefix + "CommandTotalLatency_seconds_count":
			c.Count = sm.Value
		case prefix + "CommandFailure_total":
			c.Failures = sm.Value
		}
	}
	for _, sm := range samples {
		cmd(&s.VectorSearch, sm, "mongot_command_vectorSearch")
		cmd(&s.Search, sm, "mongot_command_search")
		switch sm.Name {
		case "mongot_jvm_memory_used_bytes":
			if sm.Labels["area"] == "heap" {
				s.HeapUsed += sm.Value
			}
		case "mongot_jvm_memory_max_bytes":
			if sm.Labels["area"] == "heap" && sm.Value > 0 {
				s.HeapMax += sm.Value
			}
		}
		id := sm.Labels["indexId_logString"]
		if id == "" {
			continue
		}
		x := idx(id)
		if t := sm.Labels["indexType"]; t != "" {
			x.Type = t
		}
		switch sm.Name {
		case "mongot_index_stats_indexSizeBytes":
			x.SizeBytes = sm.Value
		case "mongot_index_stats_numLuceneDocs":
			x.Docs = sm.Value
		case "mongot_index_stats_indexing_replicationLagMs":
			x.LagMs = sm.Value
		case "mongot_index_stats_indexing_insert_total":
			x.Inserts = sm.Value
		case "mongot_index_stats_indexing_update_total":
			x.Updates = sm.Value
		case "mongot_index_stats_indexing_delete_total":
			x.Deletes = sm.Value
		case "mongot_index_stats_indexStatusCode":
			if sm.Value == 1 {
				x.Status = sm.Labels["status"]
			}
		}
		s.Indexes[id] = x
	}
	return s
}

// interesting decides which raw lines the raw view keeps: everything except the
// thread-pool and HTTP-server plumbing that makes up most of the endpoint.
func interesting(name string) bool {
	if strings.Contains(name, "executor") || strings.HasPrefix(name, "mongot_HealthCheckServer") {
		return false
	}
	return strings.HasPrefix(name, "mongot_index_stats_") || strings.HasPrefix(name, "mongot_command_") ||
		strings.HasPrefix(name, "mongot_jvm_memory") || strings.HasPrefix(name, "mongot_configState") ||
		strings.HasPrefix(name, "mongot_process") || strings.HasPrefix(name, "mongot_system")
}

// Fetch reads one endpoint (host:port) and summarizes it.
func Fetch(ctx context.Context, endpoint string) Summary {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(cctx, http.MethodGet, "http://"+endpoint+"/metrics", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Summary{Endpoint: endpoint, Error: err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Summary{Endpoint: endpoint, Error: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Summary{Endpoint: endpoint, Error: err.Error()}
	}
	samples := Parse(strings.NewReader(string(body)))
	s := Summarize(samples)
	s.Endpoint = endpoint
	for _, ln := range strings.Split(string(body), "\n") {
		name := ln
		if i := strings.IndexAny(ln, "{ "); i > 0 {
			name = ln[:i]
		}
		if !strings.HasPrefix(ln, "#") && interesting(name) {
			s.Raw = append(s.Raw, ln)
		}
	}
	sort.Strings(s.Raw)
	return s
}
