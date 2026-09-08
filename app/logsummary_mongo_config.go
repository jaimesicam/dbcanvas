package main

// logsummary_mongo_config.go — the server's own configuration, read out of its log.
//
// A mongod prints its entire configuration on the way up, in two lines:
//
//	id 21951 "Options set by command line" — the effective config, as JSON. Whether
//	  cacheSizeGB was pinned, what setParameter holds, which compressors, which role.
//
// and its storage-engine configuration in one more:
//
//	id 22315 "Opening WiredTiger" config:
//	  create,cache_size=14527M,session_max=33000,eviction=(threads_min=4,threads_max=4),…
//
// That single line is worth a great deal here. It says what the cache was actually set to
// — not what the config file asks for, what the engine got — and it says it once per
// startup, so a log that spans a restart carries BOTH values and can show what changed. It
// is also the one place the log can speak about configuration without inference.
//
// The rest of this file is the same idea applied to the warnings mongod prints and then
// nobody reads: swappiness, transparent huge pages, ulimits, deprecated parameters. Those
// are configuration findings by construction — the server has already decided they are
// wrong, and the only thing missing is somebody putting them in front of a person.
//
// The reason this matters more in dbcanvas than in most tools: every member of a stack is a
// container on ONE host. When three members each report a 14.5 GiB cache, that is 43.5 GiB
// of intent on a machine that has 29.4 GiB, and the bundle can see all three logs at once.
// Measured on exactly that setup, the workload ran at 111 TPS with p95 710 ms and the
// primary eventually aborted on a replication-lock timeout; with the caches pinned to
// 10/5/5 the same workload ran at 637 TPS with p95 71 ms.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// lsMongoConfig is what one member's log says about how it was configured.
type lsMongoConfig struct {
	CacheMB  float64 `json:"cacheMB,omitempty"`  // cache_size the engine actually opened with
	Pinned   bool    `json:"pinned,omitempty"`   // cacheSizeGB was set, rather than derived from host RAM
	EvictMax int     `json:"evictMax,omitempty"` // eviction=(threads_max=…)
	Sessions int     `json:"sessions,omitempty"` // session_max
	Startups int     `json:"startups,omitempty"` // how many times the engine was opened in this log
	Changed  bool    `json:"changed,omitempty"`  // the cache was not the same on every startup
	First    float64 `json:"first,omitempty"`    // when the first startup was seen
	// Tickets are the execution ticket counts, when somebody pinned them by hand. Read
	// from the startup options rather than from the deprecation warning the server logs:
	// that warning is written by the FTDC thread ENUMERATING parameters, so it appears on
	// every 8.0 member whether or not anyone set anything. Verified on a live member —
	// its context is "ftdc", not "initandlisten".
	Tickets  map[string]float64 `json:"tickets,omitempty"`
	Warnings []string           `json:"warnings,omitempty"`
}

var (
	lsCacheRe   = regexp.MustCompile(`cache_size=([0-9]+)([MG])`)
	lsEvictRe   = regexp.MustCompile(`threads_max=([0-9]+)`)
	lsSessionRe = regexp.MustCompile(`session_max=([0-9]+)`)
)

// lsMongoScanConfig walks a member's records for the configuration it started with.
func lsMongoScanConfig(recs []lsMongoRecord) *lsMongoConfig {
	var c lsMongoConfig
	seen := map[float64]bool{}
	for _, r := range recs {
		switch r.ID {
		case 22315: // Opening WiredTiger
			cfg := r.str("config")
			if cfg == "" {
				continue
			}
			c.Startups++
			if c.First == 0 {
				c.First = r.TS
			}
			if m := lsCacheRe.FindStringSubmatch(cfg); m != nil {
				mb, _ := strconv.ParseFloat(m[1], 64)
				if m[2] == "G" {
					mb *= 1024
				}
				if len(seen) > 0 && !seen[mb] {
					c.Changed = true
				}
				seen[mb] = true
				c.CacheMB = mb // the last startup is the one in force
			}
			if m := lsEvictRe.FindStringSubmatch(cfg); m != nil {
				c.EvictMax, _ = strconv.Atoi(m[1])
			}
			if m := lsSessionRe.FindStringSubmatch(cfg); m != nil {
				c.Sessions, _ = strconv.Atoi(m[1])
			}
		case 21951: // Options set by command line — the effective configuration
			var opt struct {
				Storage struct {
					WiredTiger struct {
						EngineConfig struct {
							CacheSizeGB float64 `json:"cacheSizeGB"`
						} `json:"engineConfig"`
					} `json:"wiredTiger"`
				} `json:"storage"`
				SetParameter map[string]json.RawMessage `json:"setParameter"`
			}
			if raw, ok := r.Attr["options"]; ok {
				if err := json.Unmarshal(raw, &opt); err == nil {
					if opt.Storage.WiredTiger.EngineConfig.CacheSizeGB > 0 {
						c.Pinned = true
					}
					for k, v := range opt.SetParameter {
						if !strings.Contains(strings.ToLower(k), "concurrent") {
							continue
						}
						var n float64
						if json.Unmarshal(v, &n) != nil {
							var str string
							if json.Unmarshal(v, &str) == nil {
								n, _ = strconv.ParseFloat(str, 64)
							}
						}
						if c.Tickets == nil {
							c.Tickets = map[string]float64{}
						}
						c.Tickets[k] = n
					}
				}
			}
		case 8386700, 22178, 22181, 22184, 22186, 22188, 22190, 7180400:
			// The startup-warning family: swappiness, THP, ulimits, filesystem.
			c.Warnings = lsAddOnce(c.Warnings, strings.TrimSuffix(r.Msg, "."))
		}
	}
	if c.Startups == 0 && len(c.Warnings) == 0 {
		return nil
	}
	return &c
}

func lsAddOnce(v []string, s string) []string {
	for _, x := range v {
		if x == s {
			return v
		}
	}
	return append(v, s)
}

// ---------------------------------------------------------------- findings

// lsFindingMongoCacheBudget is the one that pays for this file. Each member reports the
// cache it opened with; a dbcanvas stack runs them all on one host; the sum is therefore a
// claim on one machine's memory, and the default sizing makes that claim per-process.
func lsFindingMongoCacheBudget(b *lsBundle) []lsFinding {
	var total float64
	var parts []string
	var srcs []int
	derived := 0
	for _, s := range b.Sources {
		if s.MongoCfg == nil || s.MongoCfg.CacheMB <= 0 {
			continue
		}
		total += s.MongoCfg.CacheMB
		if !s.MongoCfg.Pinned {
			derived++
		}
		parts = append(parts, fmt.Sprintf("%s %.1f GiB", lsNode(b, s.Idx), s.MongoCfg.CacheMB/1024))
		srcs = append(srcs, s.Idx)
	}
	if len(parts) == 0 {
		return nil
	}
	// One member says nothing about a budget — the finding is the SUM across members that
	// share a machine, which is what a stack is.
	if len(parts) == 1 {
		return []lsFinding{{
			ID: "mongo-cache-config", Sev: "info",
			Title:   fmt.Sprintf("WiredTiger cache configured at %.1f GiB", total/1024),
			Detail:  fmt.Sprintf("%s opened its storage engine with cache_size=%.0fM.", parts[0], total),
			Advice:  "That is the whole cache this member can use. If other database processes share the machine, check that their caches plus this one still fit — mongod sizes its own cache from the machine's total memory and cannot see its neighbours.",
			Sources: srcs,
		}}
	}
	sort.Strings(parts)
	f := lsFinding{ID: "mongo-cache-config", Sources: srcs,
		Detail: "Configured at startup: " + strings.Join(parts, ", ") + "."}
	if derived >= 2 {
		// Not an estimate: the startup options say nobody set cacheSizeGB, so each of
		// these members took half the MACHINE — the same machine, all of them.
		f.Sev = "bad"
		f.Title = fmt.Sprintf("%d members each sized their cache as though alone on the host — %.1f GiB between them", derived, total/1024)
		f.Advice = "None of them has cacheSizeGB set, so each derived its cache from the machine's total memory: half of RAM minus a gigabyte, computed as if it were the only process there. Running as containers on one host, that is the same memory promised several times over. Pin storage.wiredTiger.engineConfig.cacheSizeGB on every member so the total leaves the host around 8 GiB for everything else, and weight it towards the member serving the workload rather than splitting it evenly. Measured on this hardware: three members deriving 14.19 GiB each on a 29.4 GiB host gave 111 TPS at p95 710 ms and ended with the primary aborting on a replication-lock timeout; 6/6/6 gave 377 TPS; 10/5/5 gave 637 TPS at p95 71 ms."
	} else {
		f.Sev = "info"
		f.Title = fmt.Sprintf("%d members ask for %.1f GiB of cache between them", len(parts), total/1024)
		f.Advice = "These caches were set deliberately. They are claims on the same host's memory, so what matters is the total: leave the machine around 8 GiB beyond the sum for the heap, connections and the file-system cache the engine reads its own blocks through. Weighting the total towards the member serving the workload beats splitting it evenly — measured on this hardware, the same 20 GiB gave 377 TPS as 6/6/6 and 637 TPS as 10/5/5."
	}
	return []lsFinding{f}
}

// lsFindingMongoCacheChanged reports a cache that was resized mid-log — which is what a
// tuning iteration looks like from the outside, and is worth stating plainly because every
// other number in the bundle straddles the change.
func lsFindingMongoCacheChanged(b *lsBundle) []lsFinding {
	var out []lsFinding
	for _, s := range b.Sources {
		if s.MongoCfg == nil || !s.MongoCfg.Changed {
			continue
		}
		out = append(out, lsFinding{
			ID: "mongo-cache-changed", Sev: "info",
			Title:   fmt.Sprintf("%s was restarted with a different cache size", lsNode(b, s.Idx)),
			Detail:  fmt.Sprintf("The engine was opened %d times in this log and not always with the same cache_size; the last was %.1f GiB.", s.MongoCfg.Startups, s.MongoCfg.CacheMB/1024),
			Advice:  "Rates and totals in this bundle span both configurations. Compare like with like by narrowing to one side of the restart before reading anything about cache pressure or disk reads.",
			At:      s.MongoCfg.First,
			Sources: []int{s.Idx},
		})
	}
	return out
}

// lsFindingMongoStartupWarnings surfaces what the server already told us at boot.
func lsFindingMongoStartupWarnings(b *lsBundle) []lsFinding {
	var msgs []string
	var srcs []int
	for _, s := range b.Sources {
		if s.MongoCfg == nil {
			continue
		}
		for _, w := range s.MongoCfg.Warnings {
			msgs = lsAddOnce(msgs, w)
		}
		if len(s.MongoCfg.Warnings) > 0 {
			srcs = append(srcs, s.Idx)
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	return []lsFinding{{
		ID: "mongo-startup-warnings", Sev: "warn",
		Title:   fmt.Sprintf("%d host setting%s the server itself objected to", len(msgs), lsPluralS(len(msgs))),
		Detail:  strings.Join(msgs, " · "),
		Advice:  "These are printed once at startup, above the first line anybody reads, and then never again. Each is a host setting rather than a MongoDB one — swappiness, huge pages, ulimits — and each is fixed outside the database.",
		Sources: srcs,
	}}
}

// lsFindingMongoTicketsSet catches somebody having pinned the execution ticket counts.
func lsFindingMongoTicketsSet(b *lsBundle) []lsFinding {
	var names []string
	var srcs []int
	for _, s := range b.Sources {
		if s.MongoCfg == nil {
			continue
		}
		for k, v := range s.MongoCfg.Tickets {
			names = lsAddOnce(names, fmt.Sprintf("%s=%.0f", k, v))
		}
		if len(s.MongoCfg.Tickets) > 0 {
			srcs = append(srcs, s.Idx)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil
	}
	return []lsFinding{{
		ID: "mongo-tickets-pinned", Sev: "warn",
		Title:   "The execution ticket counts are set by hand",
		Detail:  "The server logged a deprecated parameter name at startup: " + strings.Join(names, ", ") + ".",
		Advice:  "8.0 sizes the read and write ticket pools dynamically from how fast the engine is retiring work, and a pinned value overrides that. Raising ticket counts to relieve a queue almost always makes latency worse, because the queue is short for a reason: the storage engine behind it is the slow part. Unset them unless there is a measurement that says otherwise, and if operations are queueing look at the cache and the device first.",
		Sources: srcs,
	}}
}
