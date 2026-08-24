package main

// logsummary_pxcop_config.go — what the cluster was actually configured with, read out of
// its own logs, and what to change.
//
// This is the half of the feature that answers "and what should I set it to". It exists
// because of one line every Galera member writes on every start:
//
//	[Note] [Galera] Passing config to GCS: … evs.suspect_timeout = PT5S; …
//	  gcache.size = 128M; … gcs.fc_debug = 0; gcs.fc_limit = 100; …
//
// which is the complete, effective wsrep provider configuration — not what cr.yaml asked
// for, but what the provider resolved it to. It is the only place in any of these logs
// that says what a setting IS rather than what its effects looked like, and on an
// operator-managed cluster it is the only way to see the operator's defaults at all: the
// shipped cr.yaml has no `configuration:` section for pxc, so a reader of cr.yaml sees no
// numbers whatsoever.
//
// Every threshold below was chosen against a measured run on a real cluster (PXC operator
// 1.20.0, PXC 8.4.8-8.1, three members, k3s v1.36.3). Where a number is asserted, the
// measurement that produced it is named at the rule.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// lsPXCConfig is one member's effective provider configuration, plus the two numbers the
// log reports separately.
type lsPXCConfig struct {
	// Provider is every `key = value` from the GCS config line, in the provider's own
	// spelling. Kept whole rather than as named fields: the advice below reads a dozen of
	// them and a reader may want any of the ninety.
	Provider map[string]string `json:"provider,omitempty"`
	// Version is the server as it identified itself ("8.4.8-8.1").
	Version string `json:"version,omitempty"`
	// FCInterval is the flow-control interval the member last announced. It is derived —
	// gcs.fc_limit scaled by the square root of the member count — so it is evidence about
	// the cluster's size as well as about the setting.
	FCInterval int `json:"fcInterval,omitempty"`
	// FCDebugRecords is how many `FC: queue size` records the file holds, and FCDebugSpan
	// how many seconds they cover. Both exist to support one measured claim — see
	// lsPXCAdvice.
	FCDebugRecords int     `json:"fcDebugRecords,omitempty"`
	FCDebugSpan    float64 `json:"fcDebugSpan,omitempty"`
}

var (
	lsGCSConfig = regexp.MustCompile(`^Passing config to GCS:\s*(.*)$`)
	lsSrvVer    = regexp.MustCompile(`/mysqld \(mysqld ([\d.]+-[\w.]+)\)`)
)

// lsPXCScanConfig reads a member's configuration out of its own records.
//
// The LAST occurrence wins, deliberately. A tail that spans a rolling restart holds the
// configuration from before the change and the configuration from after it, and the one a
// reader is being advised about is the one the member is running now.
func lsPXCScanConfig(recs []lsRecord) *lsPXCConfig {
	var cfg lsPXCConfig
	found := false
	fcFirst, fcLast := 0.0, 0.0
	for _, r := range recs {
		if m := lsGCSConfig.FindStringSubmatch(r.Text); m != nil {
			cfg.Provider = lsParseProviderOptions(m[1])
			found = true
		}
		if m := lsSrvVer.FindStringSubmatch(r.Text); m != nil {
			cfg.Version = m[1]
		}
		if m := lsFCInterval.FindStringSubmatch(r.Text); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				cfg.FCInterval = n
				found = true
			}
		}
		if lsFCQueue.MatchString(r.Text) {
			cfg.FCDebugRecords++
			if fcFirst == 0 {
				fcFirst = r.TS
			}
			fcLast = r.TS
		}
	}
	if fcLast > fcFirst {
		cfg.FCDebugSpan = fcLast - fcFirst
	}
	if !found && cfg.Version == "" {
		return nil
	}
	return &cfg
}

// lsParseProviderOptions splits `a = 1; b = 2; …` into a map. Values are kept as written —
// `PT5S`, `128M`, `1.0` — because that is the spelling the setting has to be given back in.
func lsParseProviderOptions(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// lsPXCBytes parses Galera's size spelling (128M, 1G, 2147483647) into bytes.
func lsPXCBytes(v string) (int64, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	mult := int64(1)
	switch v[len(v)-1] {
	case 'K', 'k':
		mult, v = 1<<10, v[:len(v)-1]
	case 'M', 'm':
		mult, v = 1<<20, v[:len(v)-1]
	case 'G', 'g':
		mult, v = 1<<30, v[:len(v)-1]
	case 'T', 't':
		mult, v = 1<<40, v[:len(v)-1]
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0, false
	}
	return int64(n * float64(mult)), true
}

// lsPXCSeconds parses an ISO-8601 duration the way Galera writes them: PT5S, PT0.5S,
// PT1M, PT24H.
func lsPXCSeconds(v string) (float64, bool) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "PT") {
		return 0, false
	}
	var total float64
	num := ""
	for _, c := range v[2:] {
		switch {
		case (c >= '0' && c <= '9') || c == '.':
			num += string(c)
		case c == 'H' || c == 'M' || c == 'S':
			f, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, false
			}
			switch c {
			case 'H':
				total += f * 3600
			case 'M':
				total += f * 60
			case 'S':
				total += f
			}
			num = ""
		default:
			return 0, false
		}
	}
	return total, num == ""
}

// lsPXCTip is one piece of advice about one setting.
type lsPXCTip struct {
	Key  string `json:"key"`  // the provider option or cr.yaml path
	Is   string `json:"is"`   // what it is now
	Want string `json:"want"` // what to set it to, and the cr.yaml that does it
	Why  string `json:"why"`  // what happens if it is left alone — measured, not asserted
	Sev  string `json:"sev"`
}

// lsPXCAdvice turns one member's effective configuration into advice.
//
// The rules are ordered by how much damage the default does, not by how interesting the
// setting is. The first of them fires on the shipped operator defaults, which is the
// point: a Percona Operator for MySQL cluster deployed from cr.yaml as it ships runs a
// 128 MB gcache, and nothing in Kubernetes tells you.
func lsPXCAdvice(cfg *lsPXCConfig, ist, sst int) []lsPXCTip {
	if cfg == nil || cfg.Provider == nil {
		return nil
	}
	p := cfg.Provider
	var out []lsPXCTip

	// 1. gcache.size — the setting that decides IST vs SST, and the one the operator
	//    leaves at Galera's own tiny default.
	if raw, ok := p["gcache.size"]; ok {
		n, valid := lsPXCBytes(raw)
		switch {
		case valid && n <= 128<<20:
			why := "gcache is the ring buffer of writesets a donor can replay to a member that comes back. At " + raw +
				" it holds seconds of a busy cluster, so a member restarted for any reason — a rolling restart, a node drain, an evicted pod — rejoins by copying the whole dataset instead of the gap."
			if sst > 0 {
				why += fmt.Sprintf(" This bundle contains %d full state transfer(s) and %d incremental one(s), which is that happening.", sst, ist)
			}
			out = append(out, lsPXCTip{
				Key: "gcache.size", Is: raw, Sev: lsSevWarn,
				Want: "a few GB — enough writesets to cover the longest restart you expect. `spec.pxc.configuration` → `[mysqld]` → `wsrep_provider_options=\"gcache.size=4G\"`, and size `spec.pxc.volumeSpec` to match, because the gcache file lives in the data directory",
				Why:  why,
			})
		case valid:
			out = append(out, lsPXCTip{
				Key: "gcache.size", Is: raw, Sev: lsSevOK,
				Want: "leave it",
				Why:  "large enough that an ordinary restart rejoins by IST rather than by copying the dataset.",
			})
		}
	}

	// 2. gcs.fc_limit — how far behind a member may fall before it pauses every writer.
	if raw, ok := p["gcs.fc_limit"]; ok {
		n, err := strconv.Atoi(raw)
		switch {
		case err == nil && n <= 32:
			out = append(out, lsPXCTip{
				Key: "gcs.fc_limit", Is: raw + lsPXCIntervalNote(cfg.FCInterval), Sev: lsSevWarn,
				Want: "100 (the default) unless you are deliberately bounding how stale a read can be",
				Why: "This is how many writesets a member may have queued before it tells every other member to stop writing. Low is not safe — it is the opposite of throughput: one member a few writesets behind pauses the whole cluster. " +
					"Measured on a three-member cluster at fc_limit 16 (interval 28): a member slowed to 800 ms RTT under load sent exactly one flow-control message, and the cluster's total pause was 3.5 microseconds. The setting is easy to trip and its effect is invisible in the log.",
			})
		case err == nil && n >= 512:
			out = append(out, lsPXCTip{
				Key: "gcs.fc_limit", Is: raw + lsPXCIntervalNote(cfg.FCInterval), Sev: lsSevWarn,
				Want: "closer to 100",
				Why:  "A member may fall this far behind before anything slows down for it. Reads from that member are correspondingly stale, and promoting it replays a long queue first.",
			})
		}
	}

	// 3. gcs.fc_debug — measured to cost more than it gives.
	if raw, ok := p["gcs.fc_debug"]; ok && raw != "0" {
		why := "gcs.fc_debug adds `FC: queue size` records. Measured on all three members of a cluster running fc_debug=1: 16–22 records each, every one inside the two seconds of that member's own join and none afterwards — including through a run that did trip flow control. It costs volume and does not make a pause visible."
		if cfg.FCDebugRecords > 0 {
			why += fmt.Sprintf(" This source holds %d of them, spanning %s.", cfg.FCDebugRecords, lsOpDur(cfg.FCDebugSpan))
		}
		out = append(out, lsPXCTip{
			Key: "gcs.fc_debug", Is: raw, Sev: lsSevWarn,
			Want: "0. Read `wsrep_flow_control_paused_ns`, `_sent`, `_recv` and `wsrep_local_recv_queue_avg` instead — or the same series in PMM, which the operator wires up for you",
			Why:  why,
		})
	}

	// 4. evs.suspect_timeout, against the platform it runs on.
	if raw, ok := p["evs.suspect_timeout"]; ok {
		if secs, valid := lsPXCSeconds(raw); valid && secs <= 5 {
			out = append(out, lsPXCTip{
				Key: "evs.suspect_timeout", Is: raw, Sev: lsSevInfo,
				Want: "PT10S–PT15S on Kubernetes, with `evs.inactive_timeout` raised to match — it has to stay well above the suspect timeout",
				Why: "Five seconds of silence and a member is declared suspect. On bare metal that is a reasonable smoke alarm; on Kubernetes an ordinary node event — an image pull, a CNI reprogram, a busy kubelet — can outlast it, and the cost of a false eviction is a rejoin, " +
					"which at the shipped gcache size is a full copy of the dataset.",
			})
		}
	}

	// 5. The most surprising thing about running PXC in Kubernetes, and it is not a
	//    provider option at all.
	out = append(out, lsPXCTip{
		Key: "spec.pxc.livenessProbe", Is: "the operator's defaults", Sev: lsSevInfo,
		Want: "raise `failureThreshold` (or `timeoutSeconds`) if members are being restarted during network events rather than left to rejoin",
		Why: "A PXC member's liveness probe asks wsrep whether it is Primary. A member on the wrong side of a partition is not, so the probe fails and kubelet kills the container — measured at 25 seconds after the member shifted SYNCED → OPEN. " +
			"In its own log that arrives as `Received SHUTDOWN from user <via user signal>`, which is indistinguishable from somebody stopping it on purpose. Kubernetes turns a member that would have rejoined by itself into a member that has to be rebuilt.",
	})

	// 6. The one default here that is better than a hand-built cluster's.
	if p["socket.ssl"] == "YES" {
		out = append(out, lsPXCTip{
			Key: "socket.ssl", Is: "YES", Sev: lsSevOK,
			Want: "leave it",
			Why:  "The operator issues the certificates and turns on TLS for the replication traffic and for state transfers. A hand-built PXC cluster usually has neither.",
		})
	}
	return out
}

func lsPXCIntervalNote(n int) string {
	if n <= 0 {
		return ""
	}
	return fmt.Sprintf(" (interval [%d, %d])", n, n)
}
