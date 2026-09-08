package main

// opsummary_logs.go — the bridge from a cluster-dump to Log Summary.
//
// A pt-k8s-debug-collector archive is, in large part, a log bundle wearing a
// different hat. It carries every pod's log, the operator's log, each database's
// own error log, and the namespace's Kubernetes Events — and DBCanvas already has
// a classifier for every one of those:
//
//	logsummary_k8sevents.go  Kubernetes Events (a JSON List of Event objects)
//	logsummary_pxcop.go      the PXC operator's zap log,  + psmdbop / pgop / psop
//	logsummary_galera.go     a Galera member's error log
//	logsummary_postgres.go   PostgreSQL, mongo.go, valkey.go …
//
// So Operator Summary does not get a second set of log parsers. It turns the
// archive into []lsInput and calls lsBuild, which is a pure function — bytes in,
// classified events, phases and findings out — and every rule Log Summary learns
// from then on applies to a cluster-dump for free.
//
// The one conversion needed is events.yaml: lsSniffK8sEvents recognises the JSON
// form, and the collector writes YAML. sigs.k8s.io/yaml round-trips it exactly,
// which is the whole reason that dependency is already here.

import (
	"path"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// opLogBundleLimit caps how much log text is handed to lsBuild from one archive.
// A cluster-dump of a busy cluster carries every pod's full log; lsBuild is happy
// to parse a few million lines, but the result has to travel to a browser.
const opLogBundleLimit = 64 << 20

// opLogInputs picks the log-shaped files out of an archive and labels them the way
// Log Summary expects. Engine is left empty wherever the sniffers can do better
// than a filename can: an operator log, a Galera member's log and a plain MySQL
// error log are all "logs.txt" or "mysqld-error.log", and lsSniffEngine tells them
// apart by vocabulary.
func opLogInputs(f opFiles) []lsInput {
	var inputs []lsInput
	total := 0
	add := func(name, origin, engine string, data []byte) {
		if len(data) == 0 || total+len(data) > opLogBundleLimit {
			return
		}
		total += len(data)
		inputs = append(inputs, lsInput{Name: name, Path: name, Origin: origin, Engine: engine, Data: data})
	}

	names := make([]string, 0, len(f))
	for name := range f {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		parts := strings.Split(name, "/")
		ns := parts[0]
		base := path.Base(name)
		switch {
		// Kubernetes Events. The collector writes a v1 List as YAML; the classifier
		// reads the JSON form, and this is the only rewriting anywhere in the bridge.
		case len(parts) == 2 && base == "events.yaml":
			if isSystemNamespace(ns) {
				continue
			}
			js, err := yaml.YAMLToJSON(f[name])
			if err != nil || len(js) == 0 {
				continue
			}
			add(ns+"/events", ns, pktEngineK8sEvents, js)

		// A pod's stdout. Named "stdout" rather than after the pod, because that is
		// what it is: the collector runs
		// `kubectl logs <pod> --namespace <ns> --all-containers` (its own
		// errors.txt records the command), so this file is EVERY container's
		// stdout concatenated, with nothing marking where one ends and the next
		// begins — --all-containers does not prefix lines.
		//
		// That matters for how it reads. lsSniffEngine classifies the file as a
		// whole, by dominant vocabulary, so a pod whose sidecars are noisier than
		// its database gets filed under the sidecar: a PSMDB member came out as
		// the PBM agent, and of three identical PG instances two sniffed as
		// patroni and one as neither. Calling it "k3d-01-rs0-0/logs.txt" reads
		// like the mongod log; it is a mixture that happens to contain it.
		case base == "logs.txt" && len(parts) == 3:
			if isSystemNamespace(ns) {
				continue
			}
			add(parts[1]+" (stdout)", ns+"/"+parts[1], "", f[name])

		// The database's own error log, which is a different document from the pod
		// log that wraps it: this is where Galera state transfers, crash recovery
		// and InnoDB complaints are written.
		// The database's own error log, which is a different document from the pod
		// stdout that wraps it. The collector copies these off the filesystem — and
		// only for the MySQL family, which is why a PXC capture carries a real
		// server log and a MongoDB or PostgreSQL one does not.
		case base == "mysqld-error.log" || base == "mysqld.post.processing.log":
			if len(parts) < 3 || isSystemNamespace(ns) {
				continue
			}
			add(parts[1]+"/"+base, ns+"/"+parts[1], "", f[name])
		}
	}
	return inputs
}

// opBuildLogBundle runs the archive's logs through Log Summary's classifier. Nil
// when the archive carries nothing log-shaped, so the panel can be absent rather
// than empty.
func opBuildLogBundle(f opFiles) *lsBundle {
	inputs := opLogInputs(f)
	if len(inputs) == 0 {
		return nil
	}
	return lsBuild(inputs)
}

// opDigestLogs reduces a built bundle to what belongs on this page. The bundle
// itself can hold a hundred thousand events and has a whole page built to explore
// them; what Operator Summary owes the reader is the verdict and the handful of
// worst lines behind it.
func opDigestLogs(b *lsBundle) *opLogs {
	if b == nil {
		return nil
	}
	out := &opLogs{Sources: len(b.Sources), Events: len(b.Events)}
	for _, f := range b.Finding {
		out.Findings = append(out.Findings, opLogFinding{
			Severity: f.Sev,
			Title:    f.Title,
			Detail:   strings.TrimSpace(f.Detail + " " + f.Advice),
		})
	}
	// The worst events, newest first within a severity. lsBuild has already
	// collapsed repeats, so a line that appeared four hundred times is one event
	// carrying its own count.
	sevRank := map[string]int{"crit": 0, "critical": 0, "error": 1, "warn": 2, "warning": 2}
	rank := func(s string) int {
		if r, ok := sevRank[strings.ToLower(s)]; ok {
			return r
		}
		return 3
	}
	worst := make([]lsEvent, 0, len(b.Events))
	for _, e := range b.Events {
		if rank(e.Sev) <= 2 {
			worst = append(worst, e)
		}
	}
	sort.SliceStable(worst, func(i, j int) bool {
		if ri, rj := rank(worst[i].Sev), rank(worst[j].Sev); ri != rj {
			return ri < rj
		}
		return worst[i].TS > worst[j].TS
	})
	if len(worst) > 20 {
		worst = worst[:20]
	}
	for _, e := range worst {
		row := opLogEvent{
			Severity: e.Sev, Kind: firstNonEmpty(e.Label, e.Class),
			Message: truncate(firstNonEmpty(e.Meaning, e.Message), 300), At: e.Time,
		}
		if e.Src >= 0 && e.Src < len(b.Sources) {
			row.Source = b.Sources[e.Src].Name
			row.Node = b.Sources[e.Src].Node
		}
		out.Worst = append(out.Worst, row)
	}
	return out
}

// opAnalyse parses an archive and registers its logs as a Log Summary bundle, so
// the full timeline is one click away.
//
// This is what makes the integration hold for an *uploaded* archive. Classification
// never depended on where the archive came from — parseOpDump runs the same
// classifiers either way — but without a registered bundle there was nothing for
// the Log Summary page to open, so a support engineer's cluster-dump from a
// customer's cluster got the digest and nothing more. Now it gets the timeline,
// the per-source ranges and the event browser, exactly like a capture taken here.
func (a *App) opAnalyse(u User, data []byte, source, origin string, stackID int64, stack string) (*opModel, error) {
	m, err := parseOpDump(data, source)
	if err != nil {
		return nil, err
	}
	if len(m.logInputs) == 0 {
		return m, nil
	}
	rec := lsNewBundle(u, "Operator Summary · "+source, origin, stackID, stack, m.logInputs)
	if m.Logs == nil {
		m.Logs = &opLogs{}
	}
	m.Logs.BundleID = rec.ID
	return m, nil
}
