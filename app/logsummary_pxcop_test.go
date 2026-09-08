package main

// logsummary_pxcop_test.go — the Log Summary against a real Percona Operator for MySQL
// (PXC) cluster.
//
// Every fixture under testdata/logsummary/k*/ came off one live cluster — PXC operator
// 1.20.0 running PXC 8.4.8-8.1 on k3s v1.36.3, three members behind HAProxy, backing up to
// a SeaweedFS S3 endpoint — driven through the scenario in the directory's name while it
// was under continuous write load. Each directory holds the operator Deployment's log, the
// binlog collector's when it was running, and each member's mysqld error log read off its
// volume.
//
//	k01-bootstrap         the cluster created from cr.yaml and reaching ready
//	k04-pod-kill          cluster1-pxc-1 force-deleted under load: eviction and rejoin
//	k06-netem-partition   cluster1-pxc-2 cut off with netem — and killed by its own
//	                      liveness probe 25 s later
//	k07-smart-update      spec.pxc.configuration changed (gcache 1G, fc_limit 16,
//	                      fc_debug 1, suspect_timeout PT10S) → a rolling restart
//	k10-pitr-restore      a restore to a point in time, and the members' logs it erased
//	k14-final             the whole operator log: two restores, a PITR gap, a failing
//	                      backup, and the collector's own account beside it

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- format

// TestOperatorLogParsesAsItsOwnEngine is the first thing that has to be true: an operator
// log must not be filed as MySQL. It carries plenty of MySQL vocabulary — every reconcile
// record names a PerconaXtraDBCluster — and the sniff is what keeps a controller's log out
// of a database's catalogue.
func TestOperatorLogParsesAsItsOwnEngine(t *testing.T) {
	b := lsLoadScenario(t, "k14-final")
	var op, pitr, members int
	for _, s := range b.Sources {
		switch s.Flavour {
		case lsFlavourPXCOperator:
			op++
			if s.Engine != pktEngineOperator {
				t.Errorf("operator source engine = %q, want %q", s.Engine, pktEngineOperator)
			}
		case lsFlavourPXCPITR:
			pitr++
		case lsFlavourGalera:
			members++
		}
	}
	if op != 1 || pitr != 1 || members != 3 {
		t.Fatalf("sources: %d operator, %d collector, %d Galera members; want 1/1/3", op, pitr, members)
	}
}

// TestOperatorStackTraceFoldsIntoOneEvent is the operator-log twin of the crash-block
// test. controller-runtime prints the Go stack of a failed reconcile underneath the
// record, unindented, and every one of those lines would otherwise be an event named after
// a function in sigs.k8s.io.
func TestOperatorStackTraceFoldsIntoOneEvent(t *testing.T) {
	recs := lsFoldOperator(lsReadFixture(t, "k14-final", "operator.log"))
	folded := false
	for _, r := range recs {
		if !strings.Contains(r.Text, "Reconciler error") {
			continue
		}
		if strings.Contains(strings.Join(r.Body, "\n"), "sigs.k8s.io/controller-runtime") {
			folded = true
		}
	}
	if !folded {
		t.Fatal("no Reconciler error record carries its stack trace in the body — it is being split into events")
	}
	for _, r := range recs {
		if strings.HasPrefix(r.Text, "sigs.k8s.io/") || strings.HasPrefix(r.Text, "github.com/percona/") {
			t.Fatalf("line %d became a record of its own: %q", r.Line, r.Text)
		}
	}
}

// TestOperatorDuplicateKeysBothSurvive is the reason the field object is not decoded into
// a map. Every reconcile record writes `name` twice — the cluster, then whatever the
// message is about — and encoding/json keeps only the second.
func TestOperatorDuplicateKeysBothSurvive(t *testing.T) {
	const line = `{"controller": "pxc-controller", "PerconaXtraDBCluster": {"name":"cluster1","namespace":"pxc"}, ` +
		`"namespace": "pxc", "name": "cluster1", "name": "178f8-daily-backup", "schedule": "0 0 * * *"}`
	f, ok := lsOpParseFields(line)
	if !ok {
		t.Fatal("field object did not parse")
	}
	if got := f.first("name"); got != "cluster1" {
		t.Errorf("first(name) = %q, want cluster1 — the object being reconciled", got)
	}
	if got := f.last("name"); got != "178f8-daily-backup" {
		t.Errorf("last(name) = %q, want the scheduled job's name", got)
	}
	if got := f.last("schedule"); got != "0 0 * * *" {
		t.Errorf("schedule = %q", got)
	}
}

// TestPITRCollectorFoldsItsContinuationLines: `Peer list updated` is worthless without the
// two lines under it, which carry the members it can actually see.
func TestPITRCollectorFoldsItsContinuationLines(t *testing.T) {
	recs := lsFoldPITR(lsReadFixture(t, "k14-final", "pitr.log"))
	found := false
	for _, r := range recs {
		if r.Text != "Peer list updated" {
			continue
		}
		body := strings.Join(r.Body, "\n")
		if strings.Contains(body, "now [") && strings.Contains(body, "-pxc-") {
			found = true
		}
	}
	if !found {
		t.Fatal("no `Peer list updated` record carries its member list")
	}
}

// TestCollectorSidecarUnwraps: the fallback path for a member that is not running reads
// the log-collector sidecar's stdout, which is the error log inside a JSON envelope. What
// comes out has to be exactly the error log, or the Galera parser sees nothing it knows.
func TestCollectorSidecarUnwraps(t *testing.T) {
	raw := `{"log":"2026-08-22T05:21:16.881342Z 2 [Note] [MY-000000] [WSREP] Synchronized with group, ready for connections\n","file":"/var/lib/mysql/mysqld-error.log"}
{"log":"2026-08-22T05:21:18.387170Z 0 [System] [MY-013172] [Server] Received SHUTDOWN from user <via user signal>.\n","file":"/var/lib/mysql/mysqld-error.log"}
Fluent Bit v1.9.9`
	out := lsK8sUnwrapCollector(raw)
	if !strings.Contains(out, "[WSREP] Synchronized with group") {
		t.Errorf("envelope not unwrapped:\n%s", out)
	}
	if strings.Contains(out, `"file":`) {
		t.Errorf("envelope survived into the output:\n%s", out)
	}
	// A line that is not an envelope is kept, not dropped: silently swallowing what a
	// parser does not recognise is how the one line that mattered goes missing.
	if !strings.Contains(out, "Fluent Bit") {
		t.Errorf("a non-envelope line was dropped:\n%s", out)
	}
	// And the result has to parse as the Galera log it is.
	recs := lsFoldMySQL(out)
	if len(recs) < 2 || recs[0].Subsys != "WSREP" {
		t.Fatalf("unwrapped text does not parse as a MySQL log: %+v", recs)
	}
}

// ---------------------------------------------------------------- catalogue

// TestOperatorVocabulary asserts the records this feature was built to read are recognised
// rather than falling through to "an unclassified line".
func TestOperatorVocabulary(t *testing.T) {
	b := lsLoadScenario(t, "k14-final")
	for _, want := range []string{
		"Backup started",
		"Backup succeeded",
		"Restore is stopping the cluster",
		"Restore job started",
		"Restore is preparing the cluster",
		"Restore is starting the cluster",
		"Binlog collector cache invalidated",
		"Rolling restart started (smart update)",
		"Rolling restart finished",
		"Member is back and ready",
		"Operator became the leader",
	} {
		if lsHasLabel(b, want) == nil {
			t.Errorf("no event labelled %q", want)
		}
	}
	// The two whose text is built from a field, so an exact label cannot be asserted.
	for _, prefix := range []string{"PITR can reach ", "Primary pod: ", "Cluster is running PXC "} {
		found := false
		for _, e := range b.Events {
			if strings.HasPrefix(e.Label, prefix) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no event whose label starts with %q — the field was not read", prefix)
		}
	}
}

// TestOperatorErrorsAreNotItsLevels is this catalogue's version of the lesson at the top of
// LOG_SUMMARY.md, and it goes both ways.
//
//   - `reconcile replication error` is INFO and means the operator could not reach the
//     database at all. Filing it by its level makes it background.
//   - `ERROR Reconciler error` is emitted once per retry with an exponential backoff, so
//     one broken thing is dozens of them. Filing those as bad makes a single fault look
//     like a catastrophe.
func TestOperatorErrorsAreNotItsLevels(t *testing.T) {
	b := lsLoadScenario(t, "k14-final")
	var info, errs int
	for _, e := range b.Events {
		if e.Class != lsClassReconcile {
			continue
		}
		if strings.HasPrefix(e.Label, "Operator could not reach the cluster") {
			if e.Level != "INFO" {
				t.Errorf("expected the operator's own reachability failure at INFO, got %q", e.Level)
			}
			if e.Sev != lsSevWarn {
				t.Errorf("an INFO record that means 'cannot reach the database' was filed as %q", e.Sev)
			}
			info++
		}
		if strings.HasPrefix(e.Label, "Reconcile failed") {
			if e.Level != "ERROR" {
				t.Errorf("expected controller-runtime's retry record at ERROR, got %q", e.Level)
			}
			if e.Sev == lsSevBad {
				t.Errorf("a retry-backoff record was filed as bad; one fault would read as dozens of failures")
			}
			errs++
		}
	}
	if info == 0 || errs == 0 {
		t.Fatalf("fixture no longer contains both kinds (%d INFO-errors, %d ERROR-retries)", info, errs)
	}
}

// ---------------------------------------------------------------- verdicts

// TestRestoreIsAFullOutage: the operator's own sequence says the cluster was scaled to
// zero, and the finding has to measure it rather than merely report that a restore
// happened.
func TestRestoreIsAFullOutage(t *testing.T) {
	b := lsLoadScenario(t, "k14-final")
	f := lsHasFinding(b, "pxcop-restore")
	if f == nil {
		t.Fatal("no restore finding")
	}
	if f.Until <= f.At {
		t.Fatalf("restore outage not measured: at=%v until=%v", f.At, f.Until)
	}
	if !strings.Contains(f.Title, "the cluster was down for") {
		t.Errorf("title does not name the outage: %q", f.Title)
	}
	// The two consequences a reader only discovers afterwards.
	for _, want := range []string{"data directory", "binlog collector"} {
		if !strings.Contains(f.Advice, want) {
			t.Errorf("advice does not mention %q: %s", want, f.Advice)
		}
	}
}

// TestPITRGapIsReported. A gap is silent data loss with a delay on it: the collector goes
// on uploading, so the bucket keeps growing and nothing looks wrong until somebody tries
// to restore across the hole.
func TestPITRGapIsReported(t *testing.T) {
	b := lsLoadScenario(t, "k14-final")
	f := lsHasFinding(b, "pxcop-pitr-gap")
	if f == nil {
		t.Fatal("no PITR gap finding on a bundle whose logs contain one")
	}
	if f.Sev != lsSevBad {
		t.Errorf("gap severity = %q, want bad", f.Sev)
	}
	if !strings.Contains(f.Advice, "full backup") {
		t.Errorf("advice does not say to take a new base backup: %s", f.Advice)
	}
}

// TestSmartUpdateIsReadFromTheOperatorAlone — the rolling restart, its order and its
// duration exist only in the operator's log. The members' logs show three restarts and
// cannot say they were one operation or which member the writes were on.
func TestSmartUpdateIsReadFromTheOperatorAlone(t *testing.T) {
	b := lsLoadScenario(t, "k07-smart-update")
	f := lsHasFinding(b, "pxcop-rollout")
	if f == nil {
		t.Fatal("no rolling-restart finding")
	}
	if f.Until <= f.At {
		t.Error("rolling restart not measured")
	}
	if !strings.Contains(f.Detail, "restarted it last") {
		t.Errorf("the finding does not name the primary or the order: %s", f.Detail)
	}
}

// TestLivenessProbeKillIsNotACleanStop is the finding this catalogue exists for.
//
// cluster1-pxc-2 was cut off with netem at 05:38:56. It shifted SYNCED -> OPEN at
// 05:39:03 — correct behaviour, a member with no primary component refuses queries and
// waits. At 05:39:28, twenty-five seconds later, its log says:
//
//	[System] [MY-013172] Received SHUTDOWN from user <via user signal>
//
// Nobody stopped it. Its liveness probe asks wsrep whether it is Primary, the answer was
// no, and kubelet killed the container. The record it wrote is byte-for-byte what a
// deliberate `systemctl stop` writes, so read on its own it is maintenance.
func TestLivenessProbeKillIsNotACleanStop(t *testing.T) {
	b := lsLoadScenario(t, "k06-netem-partition")
	f := lsHasFinding(b, "pxcop-probe-restart")
	if f == nil {
		t.Fatal("a member shut down while non-primary was not identified as a probe kill")
	}
	if f.Sev != lsSevBad {
		t.Errorf("severity = %q, want bad", f.Sev)
	}
	if !strings.Contains(f.Advice, "kubectl") || !strings.Contains(f.Advice, "livenessProbe") {
		t.Errorf("advice names neither where the reason is nor the setting: %s", f.Advice)
	}
	// And the raw evidence is really in the fixture, so the test fails if the corpus is
	// ever replaced by something that does not contain it.
	raw := lsReadFixture(t, "k06-netem-partition", "cluster1-pxc-2.err")
	if !strings.Contains(raw, "Received SHUTDOWN from user <via user signal>") {
		t.Fatal("fixture no longer contains the shutdown record the finding is built on")
	}
	if !strings.Contains(raw, "Shifting SYNCED -> OPEN") {
		t.Fatal("fixture no longer contains the member leaving the primary component")
	}
}

// TestOperatorIsSilentAboutMembers. The corpus's whole argument in one assertion: the
// member logs of k04-pod-kill hold an eviction, a rejoin and a state transfer, and the
// operator's log over the same window holds PITR timelines and user grants.
func TestOperatorIsSilentAboutMembers(t *testing.T) {
	b := lsLoadScenario(t, "k04-pod-kill")
	if f := lsHasFinding(b, "pxcop-operator-silent"); f == nil {
		t.Fatal("no finding that the operator's log missed what the members' logs contain")
	}
	// The operator's own records in the minutes around the kill are only its heartbeats.
	opText := lsReadFixture(t, "k04-pod-kill", "operator.log")
	for _, mustNot := range []string{"pxc-1 evicted", "member lost", "unreachable"} {
		if strings.Contains(opText, mustNot) {
			t.Fatalf("the operator log now mentions %q — the premise of this finding has changed", mustNot)
		}
	}
}

// TestProviderConfigIsReadFromTheLog. The `Passing config to GCS` line is the only place
// an operator-managed cluster's wsrep settings exist: cr.yaml ships with no
// spec.pxc.configuration at all.
func TestProviderConfigIsReadFromTheLog(t *testing.T) {
	b := lsLoadScenario(t, "k01-bootstrap")
	var cfg *lsPXCConfig
	for _, s := range b.Sources {
		if s.PXCCfg != nil {
			cfg = s.PXCCfg
			break
		}
	}
	if cfg == nil || cfg.Provider == nil {
		t.Fatal("no provider configuration was read from any member")
	}
	if got := cfg.Provider["gcache.size"]; got != "128M" {
		t.Errorf("gcache.size = %q, want the operator's shipped default 128M", got)
	}
	if got := cfg.Provider["gcs.fc_limit"]; got != "100" {
		t.Errorf("gcs.fc_limit = %q, want 100", got)
	}
	if cfg.FCInterval != 173 {
		// 100 × √3, which is the whole reason the interval is worth reporting: it is a
		// statement about the cluster's size as well as about the setting.
		t.Errorf("flow-control interval = %d, want 173 (fc_limit 100 scaled for three members)", cfg.FCInterval)
	}
	if cfg.Version == "" {
		t.Error("no server version was read")
	}
}

// TestTuningAdviceFiresOnTheShippedDefaults. The point of the advice is that a cluster
// nobody has touched is already misconfigured for the platform it is on.
func TestTuningAdviceFiresOnTheShippedDefaults(t *testing.T) {
	b := lsLoadScenario(t, "k01-bootstrap")
	f := lsHasFinding(b, "pxcop-settings")
	if f == nil {
		t.Fatal("no configuration finding")
	}
	for _, want := range []string{"gcache.size", "128M", "evs.suspect_timeout", "livenessProbe"} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("the configuration report does not mention %q", want)
		}
	}
	if !strings.Contains(f.Advice, "spec.pxc.configuration") {
		t.Errorf("the advice does not say where to change any of it: %s", f.Advice)
	}
	if f.Sev != lsSevWarn {
		t.Errorf("severity = %q — a 128M gcache under an operator that restarts pods is a warning", f.Sev)
	}
}

// TestTuningAdviceReadsTheTunedCluster. The same code against the same cluster after
// spec.pxc.configuration was set has to report the new numbers, and stop advising about
// the ones that were fixed.
func TestTuningAdviceReadsTheTunedCluster(t *testing.T) {
	b := lsLoadScenario(t, "k07-smart-update")
	var cfg *lsPXCConfig
	for _, s := range b.Sources {
		if s.PXCCfg != nil && s.PXCCfg.Provider != nil {
			cfg = s.PXCCfg
			break
		}
	}
	if cfg == nil {
		t.Fatal("no provider configuration")
	}
	// The LAST configuration in the file, not the first: this fixture spans the restart.
	if got := cfg.Provider["gcache.size"]; got != "1G" {
		t.Errorf("gcache.size = %q, want 1G — the tail spans a rolling restart and the last one is what is running", got)
	}
	tips := lsPXCAdvice(cfg, 0, 0)
	for _, tip := range tips {
		if tip.Key == "gcache.size" && tip.Sev != lsSevOK {
			t.Errorf("gcache.size at 1G is still being advised against: %s", tip.Why)
		}
	}
	// fc_debug=1 was set in the same change, and the measurement says it does not pay.
	var fcDebug *lsPXCTip
	for i := range tips {
		if tips[i].Key == "gcs.fc_debug" {
			fcDebug = &tips[i]
		}
	}
	if fcDebug == nil {
		t.Fatal("gcs.fc_debug=1 produced no advice")
	}
	if cfg.FCDebugRecords == 0 {
		t.Error("no FC: queue size records counted, so the advice cannot cite this source")
	}
	if cfg.FCDebugSpan > 10 {
		t.Errorf("FC records span %.1fs — the claim that they only cover the member's own join no longer holds", cfg.FCDebugSpan)
	}
}

// TestOperatorSourcesAreNotCountedAsUnavailable. An operator answers no queries at any
// time. Letting it into the "time spent not answering queries" measurement would report
// every healthy cluster's controller as a multi-minute outage.
func TestOperatorSourcesAreNotCountedAsUnavailable(t *testing.T) {
	b := lsLoadScenario(t, "k14-final")
	f := lsHasFinding(b, "unavailability")
	if f == nil {
		return // nothing was unavailable in this window, which is also a pass
	}
	for _, s := range b.Sources {
		if s.Engine != pktEngineOperator {
			continue
		}
		for _, idx := range f.Sources {
			if idx == s.Idx {
				t.Errorf("%s (a %s log) is counted in the unavailability finding", s.Name, s.Flavour)
			}
		}
	}
}

// lsReadFixture reads one file out of a scenario directory, for the tests that assert
// against the raw text rather than against the parse. Those assertions are the guard on
// the corpus itself: a finding built on a record that is no longer in the fixture is a
// finding built on nothing.
func lsReadFixture(t *testing.T, scenario, file string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "logsummary", scenario, file))
	if err != nil {
		t.Fatalf("read %s/%s: %v", scenario, file, err)
	}
	return string(data)
}

// TestTwoRestoresAreTwoFindings guards the bug that live verification found.
//
// k14-final's operator log holds two restores about thirteen minutes apart. Measuring from
// the first one's `stopping cluster` to the last member that came back reported a
// 16-minute outage for a restore that took six — because the second restore had erased the
// members' logs, so the earliest member record in the bundle is thirteen minutes after the
// first restore began. Each restore is measured inside its own window.
func TestTwoRestoresAreTwoFindings(t *testing.T) {
	b := lsLoadScenario(t, "k14-final")
	var restores []lsFinding
	for _, f := range b.Finding {
		if f.ID == "pxcop-restore" {
			restores = append(restores, f)
		}
	}
	if len(restores) != 2 {
		t.Fatalf("got %d restore findings, want 2 — the fixture holds two", len(restores))
	}
	for _, f := range restores {
		d := f.Until - f.At
		if d <= 0 || d > 10*60 {
			t.Errorf("restore at %v measured %.0fs — each one took about 3–6 minutes", f.At, d)
		}
	}
	if restores[0].At > restores[1].At {
		restores[0], restores[1] = restores[1], restores[0]
	}
	if strings.Contains(restores[0].Title, "point-in-time") {
		t.Errorf("the first restore was a plain full restore: %q", restores[0].Title)
	}
	if !strings.Contains(restores[1].Title, "point-in-time") {
		t.Errorf("the second restore replayed binary logs: %q", restores[1].Title)
	}
}

// TestReconcileFailuresAreOneFinding. Five faults over an afternoon are one operator
// having a bad afternoon, not five outages — and live verification produced exactly that:
// five findings whose titles differed only in a duration.
func TestReconcileFailuresAreOneFinding(t *testing.T) {
	b := lsLoadScenario(t, "k14-final")
	n := 0
	for _, f := range b.Finding {
		if f.ID == "pxcop-reconcile" {
			n++
		}
	}
	if n > 1 {
		t.Fatalf("%d reconcile findings; they must be reported together", n)
	}
}

// TestOperatorIsNotAPartyToADisagreement. Found by looking at the rendered page, not by a
// test: the membership-disagreement finding listed "operator/cluster1 saw 0 member(s) and
// was LEADER" alongside two members that genuinely disagreed. A controller has no
// membership; a third opinion from something with no opinion is worse than silence.
func TestOperatorIsNotAPartyToADisagreement(t *testing.T) {
	b := lsLoadScenario(t, "k14-final")
	f := lsHasFinding(b, "disagreement")
	if f == nil {
		return
	}
	for _, s := range b.Sources {
		if s.Engine == pktEngineOperator && strings.Contains(f.Detail, s.Node) {
			t.Errorf("the %s source is quoted in the membership disagreement: %s", s.Flavour, f.Detail)
		}
	}
}

// TestSilentSourcesStillOverlap is the bug a reader spotted on the page: five logs tailed
// from ONE cluster in ONE request, reported as not covering a common period.
//
// Nothing was wrong with the arithmetic. The three members were healthy, and a healthy PXC
// member writes nothing — so their files stopped at 06:14:54, the line where they finished
// starting after a restore, while the read happened at 08:17. The binlog collector's pod
// had restarted at 06:23, so `kubectl logs deploy/…` began there. Latest start after
// earliest last-record: no intersection.
//
// A log that stops is a server that carried on and had nothing to report — which is what
// lsBuildPhases has always assumed and what lsOverlap did not. Coverage now ends at the
// read instant for anything tailed from a node.
func TestSilentSourcesStillOverlap(t *testing.T) {
	const hour = 3600.0
	read := 100000.0
	sources := []lsSource{
		// A member that said its last word two hours before it was read.
		{Idx: 0, FirstTS: read - 3*hour, LastTS: read - 2*hour, ReadAt: read},
		// A collector whose pod started after that member fell silent.
		{Idx: 1, FirstTS: read - 1.5*hour, LastTS: read - 60, ReadAt: read},
	}
	span, disjoint := lsOverlap(sources)
	if disjoint {
		t.Fatal("two logs read from the same cluster at the same instant were reported as disjoint")
	}
	// The common period is from the later start to the read instant.
	if want := 1.5 * hour; span < want-1 || span > want+1 {
		t.Errorf("overlap = %.0fs, want %.0fs (the later start to the read)", span, want)
	}

	// An UPLOAD keeps the old behaviour: nothing knows when the file was cut, so its last
	// record is genuinely all the evidence there is about how far it reaches.
	for i := range sources {
		sources[i].ReadAt = 0
	}
	if _, disjoint := lsOverlap(sources); !disjoint {
		t.Error("uploaded files that really do not overlap must still be reported as disjoint")
	}

	// And a real disjointness on a node bundle is still caught: one source's whole range
	// ends before another's begins, read instant or not.
	rotated := []lsSource{
		{Idx: 0, FirstTS: read - 9*hour, LastTS: read - 8*hour, ReadAt: read - 8*hour},
		{Idx: 1, FirstTS: read - 1*hour, LastTS: read - 60, ReadAt: read},
	}
	if _, disjoint := lsOverlap(rotated); !disjoint {
		t.Error("a source whose coverage ends before another's begins is still disjoint")
	}
}
