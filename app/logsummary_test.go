package main

// logsummary_test.go — the Log Summary against real Percona XtraDB Cluster logs.
//
// Every fixture under testdata/logsummary/ came off a live three-node PXC 8.0.46 cluster
// while the scenario in its directory name was performed on it. Nothing here is synthetic,
// and that is the point: a classifier written against invented log lines classifies
// invented log lines.
//
//	s01-bootstrap          a cluster created from nothing, then joined by two members
//	s03-crash-kill9        SIGKILL on pxc02 under write load, eviction, auto-restart, IST
//	s04-graceful-restart   systemctl restart on pxc03 — the clean departure
//	s05a-ftwrl-desync      FLUSH TABLES WITH READ LOCK held for 50 s on pxc03
//	s05b-flow-control      a hard write flood with a slowed member — see TestFlowControl
//	s06-network-partition  pxc03 cut off ports 4567/4568/4444 with tc netem for 52 s
//	s07-sst-rejoin         the aborted node started again, rejoining by full SST
//	s08-crash-signal11     SIGSEGV to mysqld on pxc02 under load — a real crash, with the
//	                       handler's own block, which `kill -9` can never produce

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// lsLoadScenario reads one scenario directory as a bundle, in a stable order so a test
// can refer to source 0 and mean the same file every time.
func lsLoadScenario(t *testing.T, name string) *lsBundle {
	t.Helper()
	dir := filepath.Join("testdata", "logsummary", name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		// .err is MySQL's error log, .log is mongod's. Both are "the server's own log",
		// which is the only thing this loader cares about.
		if !e.IsDir() && (strings.HasSuffix(e.Name(), ".err") || strings.HasSuffix(e.Name(), ".log")) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatalf("no .err fixtures in %s", dir)
	}
	var inputs []lsInput
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		inputs = append(inputs, lsInput{Name: n, Origin: "upload", Data: data})
	}
	return lsBuild(inputs)
}

// lsHasFinding reports whether a finding with this id is present.
func lsHasFinding(b *lsBundle, id string) *lsFinding {
	for i := range b.Finding {
		if b.Finding[i].ID == id {
			return &b.Finding[i]
		}
	}
	return nil
}

// lsHasLabel reports whether any event carries this label.
func lsHasLabel(b *lsBundle, label string) *lsEvent {
	for i := range b.Events {
		if b.Events[i].Label == label {
			return &b.Events[i]
		}
	}
	return nil
}

func lsCountLabel(b *lsBundle, label string) int {
	n := 0
	for _, e := range b.Events {
		if e.Label == label {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- basics

func TestLogSummaryRecognisesPXC(t *testing.T) {
	b := lsLoadScenario(t, "s01-bootstrap")
	if len(b.Sources) != 3 {
		t.Fatalf("want 3 sources, got %d", len(b.Sources))
	}
	for _, s := range b.Sources {
		if s.Engine != pktEngineMySQL {
			t.Errorf("%s: engine %q, want mysql", s.Name, s.Engine)
		}
		if s.Flavour != lsFlavourGalera {
			t.Errorf("%s: flavour %q, want galera", s.Name, s.Flavour)
		}
		// The node's own name is read out of the log, not out of the file name — an
		// uploaded log is often called something else entirely.
		if !strings.HasPrefix(s.Node, "pxc") {
			t.Errorf("%s: node name %q, want the name from the log", s.Name, s.Node)
		}
		if s.FirstTS == 0 || s.LastTS <= s.FirstTS {
			t.Errorf("%s: no usable time range (%f → %f)", s.Name, s.FirstTS, s.LastTS)
		}
	}
}

// TestLogSummaryDropsNoise guards the ratio that makes the page readable. Three PXC logs
// are ~850 lines here and most of them are internal bookkeeping; if a change starts
// promoting those, this catches it before the UI does.
func TestLogSummaryDropsNoise(t *testing.T) {
	b := lsLoadScenario(t, "s01-bootstrap")
	lines := 0
	for _, s := range b.Sources {
		lines += s.Lines
	}
	if b.Summary.Events == 0 {
		t.Fatal("no events at all")
	}
	if b.Summary.Events > lines/3 {
		t.Errorf("kept %d events from %d lines — the noise filter is not working",
			b.Summary.Events, lines)
	}
	for _, e := range b.Events {
		if strings.Contains(e.Message, "wsrep_notify_cmd is not defined") {
			t.Errorf("event %d is pure noise: %q", e.No, e.Message)
			break
		}
	}
}

// ---------------------------------------------------------------- multi-line records

// TestLogSummaryFoldsViewBlocks is the test for the design decision the whole feature
// rests on. The header of a view record says nothing; every fact is in the lines below it.
func TestLogSummaryFoldsViewBlocks(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "logsummary", "s03-crash-kill9", "pxc01.err"))
	if err != nil {
		t.Fatal(err)
	}
	recs := lsFoldMySQL(string(data))
	found := false
	for _, r := range recs {
		if !strings.Contains(r.Text, "Current view of cluster") {
			continue
		}
		if len(r.Body) == 0 {
			t.Fatalf("view record at line %d folded no body", r.Line)
		}
		memb, _, left, part := lsViewMembers(r.Body)
		if len(part) > 0 {
			found = true
			if len(memb) != 2 {
				t.Errorf("partition view: %d members, want 2", len(memb))
			}
			if len(left) != 0 {
				t.Errorf("a SIGKILLed member must not appear under `left`, got %v", left)
			}
		}
	}
	if !found {
		t.Fatal("no view record with a `partitioned` section — folding is broken")
	}
}

// TestLogSummaryQuorumBody reads `members = 2/3` out of a Quorum results block, which is
// likewise entirely in the continuation lines.
func TestLogSummaryQuorumBody(t *testing.T) {
	b := lsLoadScenario(t, "s01-bootstrap")
	var seen bool
	for _, e := range b.Events {
		if e.Class != lsClassQuorum || e.Total == 0 {
			continue
		}
		seen = true
		if e.Members > e.Total {
			t.Errorf("event %d: %d of %d members is impossible", e.No, e.Members, e.Total)
		}
		if e.Primary == "" {
			t.Errorf("event %d: quorum result with no component verdict", e.No)
		}
	}
	if !seen {
		t.Fatal("no quorum result carried a member count — the body was not parsed")
	}
}

// ---------------------------------------------------------------- scenarios

// TestLogSummaryCrash — SIGKILL on a member under load.
//
// The survivors must report it as a PARTITION (the member vanished), never as a clean
// departure, and the victim's own log must show the wsrep position recovery that only
// happens after an unclean stop.
func TestLogSummaryCrash(t *testing.T) {
	b := lsLoadScenario(t, "s03-crash-kill9")

	if f := lsHasFinding(b, "partition"); f == nil {
		t.Error("no partition finding for a SIGKILLed member")
	} else if f.Sev != lsSevBad {
		t.Errorf("partition finding severity %q, want bad", f.Sev)
	}
	if lsHasFinding(b, "clean-stop") != nil {
		t.Error("a SIGKILL was reported as a clean stop")
	}
	// --wsrep-recover runs on every start; what marks this one is the real position it
	// found, which is only there because the previous mysqld died.
	if lsHasLabel(b, "Restarted after an unclean stop") == nil {
		t.Error("the victim's unclean restart was not recognised")
	}
	if lsHasFinding(b, "unclean-restart") == nil {
		t.Error("no finding for the unclean restart")
	}
	if lsHasLabel(b, "Peer declared inactive") == nil {
		t.Error("the survivors' eviction of the dead member was not recognised")
	}
	// The member came back by IST, which is the cheap path and is good news.
	if lsHasLabel(b, "State transfer complete") == nil {
		t.Error("the rejoin's state transfer was not recognised")
	}
	if f := lsHasFinding(b, "unavailable"); f == nil {
		t.Error("no unavailability finding for a node that was down and then joining")
	}
}

// TestLogSummaryGracefulRestart — the same shape of outage, deliberately caused, must read
// completely differently. This is the pair of tests that proves the distinction is real
// rather than an accident of wording.
func TestLogSummaryGracefulRestart(t *testing.T) {
	b := lsLoadScenario(t, "s04-graceful-restart")

	if lsHasFinding(b, "clean-stop") == nil {
		t.Error("a systemctl restart was not recognised as a clean departure")
	}
	if f := lsHasFinding(b, "partition"); f != nil {
		t.Errorf("a clean restart was reported as a partition: %s", f.Detail)
	}
	if lsHasLabel(b, "Shutdown requested") == nil {
		t.Error("the SHUTDOWN record was not recognised")
	}
	if lsHasLabel(b, "Left the cluster cleanly") == nil {
		t.Error("the SELF-LEAVE was not recognised")
	}
	// Nobody had to wait out a suspect timeout, because the member announced itself.
	if e := lsHasLabel(b, "Peer suspected"); e != nil {
		t.Errorf("a clean departure should not produce a suspect timeout, got event %d", e.No)
	}
}

// TestLogSummaryPartition — a member cut off at the network for 52 seconds.
//
// The richest fixture: the isolated node reports itself alone and non-primary while the
// other two report a healthy two-member cluster, and both are telling the truth. It ends
// with a real [ERROR] and an aborted mysqld.
func TestLogSummaryPartition(t *testing.T) {
	b := lsLoadScenario(t, "s06-network-partition")

	if f := lsHasFinding(b, "quorum"); f == nil {
		t.Fatal("no quorum finding for an isolated member")
	} else if !strings.Contains(f.Title, "split") {
		t.Errorf("quorum finding should say the cluster split, got %q", f.Title)
	}
	if lsHasFinding(b, "disagreement") == nil {
		t.Error("the nodes reported different memberships and it was not flagged")
	}
	if lsHasFinding(b, "crash") == nil {
		t.Error("the isolated node aborted and it was not flagged")
	}
	if lsHasLabel(b, "Aborting: will never receive state") == nil {
		t.Error("the abort's own record was not recognised")
	}
	if lsHasLabel(b, "Alone in the cluster") == nil && lsHasLabel(b, "Lost the primary component") == nil {
		t.Error("the isolated node's loss of quorum was not recognised")
	}
	// Peer timeouts repeat every three seconds for the whole outage; they must fold.
	e := lsHasLabel(b, "Peer went quiet")
	if e == nil {
		t.Fatal("peer timeouts were not recognised")
	}
	if lsCountLabel(b, "Peer went quiet") > 12 {
		t.Errorf("%d separate 'peer went quiet' rows — repeats are not collapsing",
			lsCountLabel(b, "Peer went quiet"))
	}
}

// TestLogSummaryDesync — FLUSH TABLES WITH READ LOCK, the backup that quietly takes a node
// out of the read pool while leaving it perfectly reachable.
func TestLogSummaryDesync(t *testing.T) {
	b := lsLoadScenario(t, "s05a-ftwrl-desync")

	f := lsHasFinding(b, "desync")
	if f == nil {
		t.Fatal("a 50-second desync was not recognised")
	}
	if f.Until <= f.At {
		t.Error("the desync's resync was not paired with it, so no duration was reported")
	}
	if lsHasLabel(b, "Provider paused") == nil {
		t.Error("the provider pause was not recognised")
	}
	if lsHasLabel(b, "Provider resumed") == nil {
		t.Error("the provider resume was not recognised")
	}
	// Nothing here was an outage: no crash, no partition, no lost quorum.
	for _, id := range []string{"crash", "partition", "quorum"} {
		if lsHasFinding(b, id) != nil {
			t.Errorf("a backup's desync was reported as %q", id)
		}
	}
}

// TestLogSummaryBootstrap — a cluster created from nothing.
func TestLogSummaryBootstrap(t *testing.T) {
	b := lsLoadScenario(t, "s01-bootstrap")

	f := lsHasFinding(b, "bootstrap")
	if f == nil {
		t.Fatal("the bootstrap was not recognised")
	}
	// Exactly one node bootstrapped, which is correct and must not be the loud version.
	if f.Sev != lsSevWarn {
		t.Errorf("a single bootstrap should be a warning, got %q", f.Sev)
	}
	if len(f.Sources) != 1 {
		t.Errorf("bootstrap attributed to %d sources, want 1", len(f.Sources))
	}
	if lsHasLabel(b, "Synced and serving") == nil {
		t.Error("no node was recorded as reaching SYNCED")
	}
}

// TestLogSummarySST — the rejoin that needed a full physical copy.
func TestLogSummarySST(t *testing.T) {
	b := lsLoadScenario(t, "s07-sst-rejoin")
	f := lsHasFinding(b, "sst")
	if f == nil {
		t.Fatal("a full SST was not recognised")
	}
	if f.Until <= f.At {
		t.Error("the SST was not paired with its completion, so no duration was reported")
	}
	if lsHasLabel(b, "State received") == nil {
		t.Error("the received snapshot was not recognised")
	}
}

// TestLogSummaryFlowControl is the honesty test.
//
// The s05b fixture is what a hard write flood against a deliberately slowed member left in
// the error log. The cluster measurably applied flow control during it —
// wsrep_flow_control_recv went to 10 and wsrep_flow_control_paused_ns to 91,676,189 — and
// the log recorded ONE line, the interval. So the feature must never report "no flow
// control" from silence; it reports that the log cannot tell you, and where to look
// instead.
func TestLogSummaryFlowControl(t *testing.T) {
	b := lsLoadScenario(t, "s05b-flow-control")

	f := lsHasFinding(b, "flow-control")
	if f == nil {
		t.Fatal("no flow-control finding — silence would be read as 'none happened'")
	}
	if !strings.Contains(f.Advice, "wsrep_flow_control_paused") {
		t.Error("the flow-control finding must point at the status variables that do measure it")
	}
	// The one line that IS there was a threshold far below the default, which is worth
	// promoting from information to a warning.
	if f.Sev != lsSevWarn {
		t.Errorf("an interval of [2, 2] should raise the finding to a warning, got %q", f.Sev)
	}
	if e := lsHasLabel(b, "Flow-control interval"); e == nil {
		t.Error("the interval record itself was not recognised")
	} else if e.Sev != lsSevWarn {
		t.Errorf("interval [2, 2] classified %q, want warn", e.Sev)
	}
}

// ---------------------------------------------------------------- timeline

// TestLogSummaryPhasesTile checks the property the "what was the cluster doing at t"
// lookup depends on: every source's phases cover the whole window with no gaps and no
// overlaps.
func TestLogSummaryPhasesTile(t *testing.T) {
	for _, name := range []string{"s01-bootstrap", "s03-crash-kill9", "s06-network-partition"} {
		b := lsLoadScenario(t, name)
		for _, s := range b.Sources {
			var track []lsPhase
			for _, p := range b.Phases {
				if p.Src == s.Idx {
					track = append(track, p)
				}
			}
			if len(track) == 0 {
				t.Errorf("%s/%s: no phases at all", name, s.Name)
				continue
			}
			if track[0].From != b.Summary.FirstTS {
				t.Errorf("%s/%s: track starts at %f, window at %f",
					name, s.Name, track[0].From, b.Summary.FirstTS)
			}
			for i, p := range track {
				if p.To <= p.From {
					t.Errorf("%s/%s: phase %d has no duration", name, s.Name, i)
				}
				if i > 0 && track[i-1].To != p.From {
					t.Errorf("%s/%s: gap or overlap between phase %d and %d", name, s.Name, i-1, i)
				}
			}
			if last := track[len(track)-1]; last.To != b.Summary.LastTS {
				t.Errorf("%s/%s: track ends at %f, window at %f",
					name, s.Name, last.To, b.Summary.LastTS)
			}
		}
	}
}

// TestLogSummaryStateAtPartition is the question the whole page exists to answer, asked of
// the moment a member was cut off: two nodes SYNCED with three members between them,
// and the isolated one alone and not serving.
func TestLogSummaryStateAtPartition(t *testing.T) {
	b := lsLoadScenario(t, "s06-network-partition")
	iso := lsHasLabel(b, "Alone in the cluster")
	if iso == nil {
		if iso = lsHasLabel(b, "Lost the primary component"); iso == nil {
			t.Fatal("could not find the moment of isolation")
		}
	}
	at := iso.TS + 1
	synced, notSynced := 0, 0
	for _, s := range b.Sources {
		p, ok := lsStateAt(b.Phases, s.Idx, at)
		if !ok {
			t.Fatalf("%s: no phase covering %f", s.Name, at)
		}
		if p.State == lsStateSynced {
			synced++
		} else {
			notSynced++
		}
	}
	if synced < 2 {
		t.Errorf("only %d node(s) SYNCED during a minority partition, want the 2-node majority", synced)
	}
	if notSynced < 1 {
		t.Error("the isolated node was not shown as out of service")
	}
}

// TestLogSummaryBucketsCoverWindow checks the swimlane data: one row per source per
// bucket, and nothing dropped between them.
func TestLogSummaryBucketsCoverWindow(t *testing.T) {
	b := lsLoadScenario(t, "s03-crash-kill9")
	const n = 40
	buckets := lsBucketise(b.Events, b.Sources, b.Summary.FirstTS, b.Summary.LastTS, n)
	if len(buckets) != n*len(b.Sources) {
		t.Fatalf("%d buckets, want %d", len(buckets), n*len(b.Sources))
	}
	total := 0
	for _, bk := range buckets {
		total += bk.Count
		if bk.Count != bk.OK+bk.Warn+bk.Bad+bk.Info {
			t.Errorf("bucket src %d/%d: severities do not sum to the count", bk.Src, bk.I)
		}
	}
	want := 0
	for _, e := range b.Events {
		if e.Repeat > 1 {
			want += e.Repeat
		} else {
			want++
		}
	}
	if total != want {
		t.Errorf("buckets hold %d events, bundle has %d", total, want)
	}
}

// ---------------------------------------------------------------- healthy

// TestLogSummaryQuietIsGood covers the finding a healthy cluster produces. Thirty seconds
// of continuous inserts across three PXC nodes wrote nothing at all to any error log —
// so an empty bundle has to be reported as "nothing happened, and here is how to tell that
// apart from the wrong file", not as an empty page.
func TestLogSummaryQuietIsGood(t *testing.T) {
	b := lsBuild([]lsInput{{Name: "pxc01.err", Origin: "upload", Data: []byte("")}})
	f := lsHasFinding(b, "quiet")
	if f == nil {
		t.Fatal("an empty log produced no finding at all")
	}
	if f.Sev != lsSevOK {
		t.Errorf("severity %q, want ok", f.Sev)
	}
	if !strings.Contains(f.Detail, "wrong file") {
		t.Error("the finding must name the other explanation for silence")
	}
}

// TestLogSummaryDisjointSources catches the mistake that otherwise draws a perfectly
// plausible and completely wrong timeline: two logs from different periods.
func TestLogSummaryDisjointSources(t *testing.T) {
	a, err := os.ReadFile(filepath.Join("testdata", "logsummary", "s01-bootstrap", "pxc01.err"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := os.ReadFile(filepath.Join("testdata", "logsummary", "s07-sst-rejoin", "pxc03.err"))
	if err != nil {
		t.Fatal(err)
	}
	b := lsBuild([]lsInput{
		{Name: "early.err", Data: a},
		{Name: "late.err", Data: c},
	})
	if !b.Summary.Disjoint {
		t.Fatal("two non-overlapping logs were not reported as disjoint")
	}
	if lsHasFinding(b, "disjoint") == nil {
		t.Error("no finding warned that these logs cannot be compared")
	}
}

// TestLogSummaryClockOffset checks the one misalignment parsing cannot fix. Shifting a
// source by an hour must move its events and its phases together — a half-applied offset
// would put a node's events outside its own state track.
func TestLogSummaryClockOffset(t *testing.T) {
	dir := filepath.Join("testdata", "logsummary", "s03-crash-kill9")
	load := func(offset float64) *lsBundle {
		var inputs []lsInput
		for _, n := range []string{"pxc01.err", "pxc02.err", "pxc03.err"} {
			data, err := os.ReadFile(filepath.Join(dir, n))
			if err != nil {
				t.Fatal(err)
			}
			in := lsInput{Name: n, Data: data}
			if n == "pxc02.err" {
				in.Offset = offset
			}
			inputs = append(inputs, in)
		}
		return lsBuild(inputs)
	}
	base, shifted := load(0), load(3600)
	var b0, b1 float64
	for _, s := range base.Sources {
		if s.Name == "pxc02.err" {
			b0 = s.FirstTS
		}
	}
	for _, s := range shifted.Sources {
		if s.Name == "pxc02.err" {
			b1 = s.FirstTS
		}
	}
	if d := b1 - b0; d < 3599 || d > 3601 {
		t.Errorf("offset moved the source by %f s, want 3600", d)
	}
	// An hour of skew puts the sources on different days' worth of window; the coverage
	// check has to notice.
	if !shifted.Summary.Disjoint && shifted.Summary.Overlap > 60 {
		t.Errorf("an hour of skew left %f s of claimed overlap", shifted.Summary.Overlap)
	}
}

// ---------------------------------------------------------------- other engines

// TestLogSummaryOtherEngines checks the adapter that reuses the Packet Inspector's
// PostgreSQL / MongoDB / Valkey classifiers. PXC is where the depth is; these must still
// parse, place themselves in time, and pick up a severity.
func TestLogSummaryOtherEngines(t *testing.T) {
	cases := []struct {
		name, engine, text string
	}{
		{"postgres", pktEnginePostgres,
			"2026-08-14 01:42:31.123 UTC [123] FATAL:  the database system is starting up\n" +
				"2026-08-14 01:42:32.500 UTC [124] LOG:  database system is ready to accept connections\n"},
		{"mongodb", pktEngineMongoDB,
			`{"t":{"$date":"2026-08-14T01:42:31.123+00:00"},"s":"I","c":"NETWORK","id":22943,"ctx":"listener","msg":"Connection accepted","attr":{"remote":"172.29.0.5:41234","connectionId":7}}` + "\n"},
		{"valkey", pktEngineValkey,
			"1:M 14 Aug 2026 01:42:31.123 * Ready to accept connections tcp\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := lsBuild([]lsInput{{Name: c.name + ".log", Data: []byte(c.text)}})
			if got := b.Sources[0].Engine; got != c.engine {
				t.Fatalf("sniffed engine %q, want %q", got, c.engine)
			}
			if len(b.Events) == 0 {
				t.Fatal("no events parsed")
			}
			for _, e := range b.Events {
				if e.TS == 0 {
					t.Errorf("event %d has no timestamp, so it cannot be placed on a timeline", e.No)
				}
				if e.Sev == "" {
					t.Errorf("event %d has no severity", e.No)
				}
			}
		})
	}
}

// ---------------------------------------------------------------- filtering

// TestLogSummaryQueryFilters covers the range and filter logic the list endpoint uses.
func TestLogSummaryQueryFilters(t *testing.T) {
	b := lsLoadScenario(t, "s03-crash-kill9")
	mid := (b.Summary.FirstTS + b.Summary.LastTS) / 2

	count := func(q lsQuery) int {
		n := 0
		for _, e := range b.Events {
			if q.match(e) {
				n++
			}
		}
		return n
	}
	all := count(lsQuery{Src: -1})
	if all != len(b.Events) {
		t.Fatalf("an empty query matched %d of %d events", all, len(b.Events))
	}
	if n := count(lsQuery{Src: 0}); n == 0 || n >= all {
		t.Errorf("source filter matched %d of %d — expected a proper subset", n, all)
	}
	if n := count(lsQuery{Src: -1, Sev: []string{lsSevBad}}); n == 0 {
		t.Error("severity filter found no bad events in a crash scenario")
	}
	if n := count(lsQuery{Src: -1, Class: lsClassMember}); n == 0 {
		t.Error("class filter found no membership events in a crash scenario")
	}
	early := count(lsQuery{Src: -1, ToTS: mid})
	late := count(lsQuery{Src: -1, FromTS: mid})
	if early == 0 || late == 0 || early+late < all {
		t.Errorf("time split lost events: %d before + %d after, %d total", early, late, all)
	}
	// Search must reach the meaning text too — that is where the words an operator would
	// actually type ("donor", "quorum") live for records whose message is terse.
	if n := count(lsQuery{Src: -1, Search: "donor"}); n == 0 {
		t.Error("search for 'donor' found nothing in a scenario with a state transfer")
	}
}

// TestLogSummaryEventsAreOrdered guards the merge: one numbering, in time order, across
// every source.
func TestLogSummaryEventsAreOrdered(t *testing.T) {
	b := lsLoadScenario(t, "s06-network-partition")
	for i, e := range b.Events {
		if e.No != i+1 {
			t.Fatalf("event at index %d numbered %d", i, e.No)
		}
		if i > 0 && e.TS < b.Events[i-1].TS {
			t.Fatalf("event %d is earlier than event %d", e.No, e.No-1)
		}
	}
	seen := map[int]bool{}
	for _, e := range b.Events {
		seen[e.Src] = true
	}
	if len(seen) != len(b.Sources) {
		t.Errorf("only %d of %d sources contributed events", len(seen), len(b.Sources))
	}
}

// TestLogSummarySettledState covers the flaw the first live run exposed.
//
// Cutting a member off produced three records within 370 µs of each other: a view saying
// "1 member, non-primary" while the node was still nominally SYNCED, then NON-PRIMARY, then
// `Shifting SYNCED -> OPEN`. Every one is real, and so are the microsecond-wide phases
// between them — but a readout that lands in the first reports "SYNCED, 1 member,
// non-primary", three facts that were briefly all true and together describe nothing.
//
// So a state lookup must skip past phases too short to be a state at all.
func TestLogSummarySettledState(t *testing.T) {
	b := lsLoadScenario(t, "s06-network-partition")
	iso := lsHasLabel(b, "Alone in the cluster")
	if iso == nil {
		t.Fatal("could not find the moment of isolation")
	}
	// The isolated source, sampled a hair after its own view record.
	p, ok := lsStateAt(b.Phases, iso.Src, iso.TS+0.00001)
	if !ok {
		t.Fatal("no phase covers the instant")
	}
	if p.State == lsStateSynced {
		t.Errorf("a node reported alone and non-primary was read as %s — the lookup landed in a transition",
			p.State)
	}
	if p.To-p.From < lsSettledMS {
		t.Errorf("lsStateAt returned a %f s phase, shorter than the settled floor", p.To-p.From)
	}
	// lsPhaseAt is the unsmoothed view and must still return the literal sliver, because
	// the timeline draws it and the two callers want different things.
	if q, ok := lsPhaseAt(b.Phases, iso.Src, iso.TS+0.00001); ok && q.To-q.From >= lsSettledMS {
		t.Error("lsPhaseAt smoothed over the transition; only lsStateAt should")
	}
}

// ---------------------------------------------------------------- crashes

// TestLogSummarySignal11 covers the gap that shipped: MySQL's crash handler writes in a
// format of its own, and until lsCrashHeader existed none of it was a header — so the whole
// block, signal and backtrace included, folded into the BODY of whatever record came
// before. On the live cluster that record was "Member synced with group": a crash was being
// reported, in green, as good news.
//
// The corpus hid it because the only crash fixture was made with `kill -9`. SIGKILL cannot
// be caught, so no handler runs and no block is written. This fixture is a real SIGSEGV.
func TestLogSummarySignal11(t *testing.T) {
	b := lsLoadScenario(t, "s08-crash-signal11")

	e := lsHasLabel(b, "Server crashed — signal 11")
	if e == nil {
		t.Fatal("a signal-11 crash was not recognised")
	}
	if e.Sev != lsSevBad {
		t.Errorf("crash severity %q, want bad", e.Sev)
	}
	if e.Class != lsClassCrash {
		t.Errorf("crash class %q, want %q", e.Class, lsClassCrash)
	}
	// The handler's output is the bug report; it has to survive as the record's detail.
	if !strings.Contains(e.Detail, "stack_bottom") || !strings.Contains(e.Detail, "#0 ") {
		t.Error("the backtrace was not folded into the crash record")
	}
	if !strings.Contains(e.Message, "Percona XtraDB Cluster") {
		t.Errorf("the crash record does not name the server build: %q", e.Message)
	}
	// A crashed process is not in whatever state it was in when it died.
	if e.State != lsStateDown {
		t.Errorf("state after a crash is %q, want %q", e.State, lsStateDown)
	}
	if f := lsHasFinding(b, "crash"); f == nil {
		t.Error("no crash finding")
	} else if !strings.Contains(f.Detail, "signal 11") {
		t.Errorf("the crash finding does not name the signal: %q", f.Detail)
	}
	// And the survivors must still read it as a loss, not a clean stop.
	if lsHasFinding(b, "partition") == nil {
		t.Error("the survivors did not report the crashed member as lost")
	}
}

// TestLogSummaryCrashHeaderForms pins the two timestamp layouts the crash handler uses —
// MySQL 8 writes RFC3339 with a Z, older builds write YYMMDD HH:MM:SS — and, just as
// importantly, that an ordinary record is NOT mistaken for one.
func TestLogSummaryCrashHeaderForms(t *testing.T) {
	crashes := []string{
		"2026-08-14T07:51:05Z UTC - mysqld got signal 11 ;",
		"260814  7:51:05 UTC - mysqld got signal 6 ;",
		"2026-08-14T07:51:05Z - mysqld got signal 11 ;",
	}
	for _, line := range crashes {
		if !lsCrashHeader.MatchString(line) {
			t.Errorf("crash header not matched: %q", line)
		}
	}
	notCrashes := []string{
		"2026-08-14T01:42:31.045860Z 0 [Note] [MY-000000] [Galera] Current view of cluster",
		"Most likely, you have hit a bug, but this error can also be caused by hardware.",
		" #0 0x7c27e246dc2f <unknown>",
	}
	for _, line := range notCrashes {
		if lsCrashHeader.MatchString(line) {
			t.Errorf("ordinary line matched as a crash header: %q", line)
		}
	}
}

// TestLogSummaryCrashEvidenceIsNeverLost is the invariant that would have caught the
// signal-11 bug on the day it was written, without anyone thinking to test for crashes.
//
// It is asserted against the RAW fixture text rather than against the parsed events,
// because folding can lose a record in two different quiet ways and only this catches
// both. An unrecognised header is absorbed into the record above it — on the live cluster
// the crash block ended up inside "Member synced with group", severity *good*. And if there
// is no record above it, because the excerpt begins there, it is dropped outright: that is
// what happened to the s08 fixture, where the crash produced no event at all.
//
// So the rule is simply: if a line in the file says the server crashed, an event must say
// the server crashed, at that line.
func TestLogSummaryCrashEvidenceIsNeverLost(t *testing.T) {
	// Lines that can only be a crash. Deliberately not the whole crash block — "stack_bottom"
	// and the frame lines belong in the crash record's DETAIL, which is correct.
	needles := []string{"mysqld got signal", "Assertion failure", "mysqld: Terminated"}

	dirs, err := os.ReadDir(filepath.Join("testdata", "logsummary"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, _ := filepath.Glob(filepath.Join("testdata", "logsummary", d.Name(), "*.err"))
		if len(files) == 0 {
			continue
		}
		b := lsLoadScenario(t, d.Name())
		sort.Strings(files)
		for src, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for i, line := range strings.Split(string(raw), "\n") {
				hit := ""
				for _, n := range needles {
					if strings.Contains(line, n) {
						hit = n
					}
				}
				if hit == "" {
					continue
				}
				// Accounted for means one of two things, and only two. Either the line
				// IS a crash event, or it was deliberately folded into one — which is
				// what happens to Percona Server's second copy of the crash block, the
				// MY-013951 re-emission of the same report. What must never happen is
				// the line ending up inside a record that is not about a crash, or
				// nowhere at all.
				accounted := false
				for _, e := range b.Events {
					if e.Src != src || e.Class != lsClassCrash {
						continue
					}
					if e.Line == i+1 {
						accounted = true
						break
					}
					// Compare against the message, not the whole line: a folded body holds
					// what the record SAID, with the header stripped, which is exactly
					// what the parser keeps.
					msg := strings.TrimSpace(line)
					if m := lsMySQLHeader.FindStringSubmatch(line); m != nil {
						msg = strings.TrimSpace(m[6])
					}
					if e.Line < i+1 && strings.Contains(e.Detail, msg) {
						accounted = true
						break
					}
				}
				if !accounted {
					t.Errorf("%s/%s line %d says %q and no crash event accounts for it — "+
						"the record was absorbed into a non-crash record, or dropped",
						d.Name(), filepath.Base(f), i+1, hit)
				}
			}
		}
	}
}

// TestLogSummaryNothingBadHidesInGoodNews is the invariant that would have caught the
// signal-11 bug on the day it was written, without anyone thinking to test for crashes.
//
// Folding is what makes this feature work and also what makes it dangerous: an
// unrecognised header does not go missing, it gets absorbed into the record above it. So
// no event the page calls good or background may have crash evidence buried in its body.
func TestLogSummaryNothingBadHidesInGoodNews(t *testing.T) {
	needles := []string{"got signal", "Assertion failure", "stack_bottom",
		"Attempting backtrace", "mysqld: Terminated", "Need to abort"}
	dirs, err := os.ReadDir(filepath.Join("testdata", "logsummary"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		// A scenario directory can legitimately hold nothing: a healthy cluster under load
		// writes nothing to its error log, which is a finding rather than a fixture.
		if fs, _ := filepath.Glob(filepath.Join("testdata", "logsummary", d.Name(), "*.err")); len(fs) == 0 {
			continue
		}
		b := lsLoadScenario(t, d.Name())
		for _, e := range b.Events {
			if e.Sev == lsSevBad || e.Sev == lsSevWarn {
				continue
			}
			for _, n := range needles {
				if strings.Contains(e.Detail, n) {
					t.Errorf("%s: event %d (%q, sev %s) has %q buried in its detail — "+
						"an unrecognised header was absorbed into a record above it",
						d.Name(), e.No, e.Label, e.Sev, n)
				}
			}
		}
	}
}

// ---------------------------------------------------------------- asynchronous replication
//
// The `r0*` fixtures came off a live three-node Percona Server 8.0.46-37 GTID topology —
// one source, two replicas — captured while doing the thing that produces them:
//
//	r02-dupkey-conflict     a row written straight onto a replica, then the same key from
//	                        the source: the SQL thread stops with 1062
//	r03-repl-auth-fail      the replication user's password rotated under a running replica
//	r04-binlog-purged       PURGE BINARY LOGS while a replica was stopped and behind
//	r05-replica-lag         a replica held 61 s behind by a table lock — and silent about it
//	r06-stop-start-replica  a clean STOP REPLICA / START REPLICA
//	r07-source-crash        SIGSEGV to the source with both replicas connected
//	r08-replica-crash       SIGSEGV to a replica mid-apply, and its XA crash recovery
//	r09-source-unreachable  100% packet loss on 3306 from a replica for 80 s

// TestLogSummaryReplicationConflict — the classic async failure: the replica's copy of a
// row is not what the source is describing, so the applier stops.
func TestLogSummaryReplicationConflict(t *testing.T) {
	b := lsLoadScenario(t, "r02-dupkey-conflict")

	e := lsHasLabel(b, "Replication stopped applying an event")
	if e == nil {
		t.Fatal("a 1062 applier stop was not recognised")
	}
	if e.Sev != lsSevBad {
		t.Errorf("severity %q, want bad", e.Sev)
	}
	// The facts an operator needs before they can decide anything: which row, which table,
	// which transaction.
	for _, want := range []string{"1062", "accounts.PRIMARY", "replab.accounts", "transaction"} {
		if !strings.Contains(e.Message, want) {
			t.Errorf("the record does not name %q: %q", want, e.Message)
		}
	}
	f := lsHasFinding(b, "replication-broken")
	if f == nil {
		t.Fatal("no replication-broken finding")
	}
	if f.Sev != lsSevBad || !strings.Contains(f.Title, "still broken") {
		t.Errorf("a replica that never recovered should be reported as still broken, got %q/%q", f.Sev, f.Title)
	}
}

// TestLogSummaryReplicationCannotConnect — a replica whose credentials stopped working.
//
// The retry policy is the point: one error, then sixty days of silence.
func TestLogSummaryReplicationCannotConnect(t *testing.T) {
	b := lsLoadScenario(t, "r03-repl-auth-fail")

	e := lsHasLabel(b, "Replica cannot connect to its source")
	if e == nil {
		t.Fatal("an I/O thread connect failure was not recognised")
	}
	if !strings.Contains(e.Message, "1045") {
		t.Errorf("the access-denied error number is missing: %q", e.Message)
	}
	if !strings.Contains(e.Message, "60 days") {
		t.Errorf("the retry policy should be spelled out in days, got %q", e.Message)
	}
	if !strings.Contains(e.Meaning, "write nothing further") {
		t.Error("the record must say that the server will retry silently")
	}
}

// TestLogSummaryBinlogPurged — the async equivalent of "IST impossible, falling back to
// SST": the replica cannot catch up by itself any more.
func TestLogSummaryBinlogPurged(t *testing.T) {
	b := lsLoadScenario(t, "r04-binlog-purged")

	e := lsHasLabel(b, "The source purged binary logs this replica still needed")
	if e == nil {
		t.Fatal("a 1236 purged-binlog stop was not recognised")
	}
	if e.Sev != lsSevBad {
		t.Errorf("severity %q, want bad", e.Sev)
	}
	// The missing GTID range is what says how far gone it is.
	if !strings.Contains(e.Message, "missing") || !strings.Contains(e.Message, ":2053-4545") {
		t.Errorf("the missing GTID range was not extracted: %q", e.Message)
	}
	if !strings.Contains(e.Meaning, "rebuilt") {
		t.Error("the record must say the replica has to be rebuilt")
	}
}

// TestLogSummarySourceCrash — a crash on the SOURCE, seen from both sides.
func TestLogSummarySourceCrash(t *testing.T) {
	b := lsLoadScenario(t, "r07-source-crash")

	if lsHasLabel(b, "Server crashed — signal 11") == nil {
		t.Error("the source's crash was not recognised")
	}
	// Percona Server writes the crash block twice — raw, then again as MY-013951. One
	// crash must produce one crash record, not twenty-odd.
	if n := lsCountLabel(b, "Server crashed"); n > 1 {
		t.Errorf("%d 'Server crashed' rows for one crash — the re-emitted block is not folding", n)
	}
	// The replica's side of it.
	if lsHasLabel(b, "Replication stream broke") == nil {
		t.Error("the replica did not report losing the stream")
	}
	if e := lsHasLabel(b, "Replica lost its source"); e == nil {
		t.Error("the replica's failed reconnect was not recognised")
	}
	// And it came back, which the verdict has to pair up and time.
	f := lsHasFinding(b, "replication-broken")
	if f == nil {
		t.Fatal("no replication finding")
	}
	if !strings.Contains(f.Detail, "recovered") || !strings.Contains(f.Detail, "back after") {
		t.Errorf("the recovery was not paired with the failure: %q", f.Detail)
	}
}

// TestLogSummaryReplicaCrashRecovery — a crashed replica coming back through XA recovery.
func TestLogSummaryReplicaCrashRecovery(t *testing.T) {
	b := lsLoadScenario(t, "r08-replica-crash")

	if lsHasLabel(b, "Crash recovery running") == nil {
		t.Error("XA crash recovery on restart was not recognised")
	}
	if lsHasLabel(b, "Crash recovery finished") == nil {
		t.Error("the end of crash recovery was not recognised")
	}
	if lsHasLabel(b, "Replica connected to its source") == nil {
		t.Error("the replica's reconnect after recovery was not recognised")
	}
}

// TestLogSummarySilentReconnect is the finding built on an ABSENCE, and the fixture proves
// the absence is real: cutting a replica off its source with 100% packet loss for 80
// seconds produced no disconnect record at all — the I/O thread blocked until
// slave_net_timeout and then reconnected, and the only trace is the line saying it had.
func TestLogSummarySilentReconnect(t *testing.T) {
	b := lsLoadScenario(t, "r09-source-unreachable")

	// The premise: nothing in this log reports a failure.
	for _, e := range b.Events {
		if e.Sev == lsSevBad {
			t.Fatalf("the fixture was supposed to contain no failure record, found %q", e.Label)
		}
	}
	f := lsHasFinding(b, "silent-reconnect")
	if f == nil {
		t.Fatal("an unexplained reconnect was not flagged — an 80-second outage would read as a quiet log")
	}
	// It must not overclaim: a manual START REPLICA looks identical from the log.
	if !strings.Contains(f.Advice, "START REPLICA") {
		t.Error("the finding must name the other explanation for the same records")
	}
	if !strings.Contains(f.Advice, "slave_net_timeout") {
		t.Error("the finding must explain why the disconnect was never written")
	}
}

// TestLogSummaryReplicaLagIsInvisible is the async twin of the flow-control finding.
// A replica 61 seconds behind wrote nothing about it, so silence must never be reported
// as health.
func TestLogSummaryReplicaLagIsInvisible(t *testing.T) {
	b := lsLoadScenario(t, "r02-dupkey-conflict")
	f := lsHasFinding(b, "replica-lag")
	if f == nil {
		t.Fatal("no lag finding — silence would be read as 'no lag'")
	}
	if !strings.Contains(f.Advice, "Seconds_Behind_Source") ||
		!strings.Contains(f.Advice, "replication_applier_status_by_worker") {
		t.Error("the finding must point at what does measure lag")
	}
	// It is scoped: a log with no replication in it should not carry this note.
	g := lsLoadScenario(t, "s01-bootstrap")
	if lsHasFinding(g, "replica-lag") != nil {
		t.Error("the replication-lag note appeared on a log with no asynchronous replication")
	}
}

// TestLogSummaryReplicationHealthy — the good news has to be recognised too, or a healthy
// replica's log is a page of nothing.
func TestLogSummaryReplicationHealthy(t *testing.T) {
	b := lsLoadScenario(t, "r06-stop-start-replica")
	e := lsHasLabel(b, "Replica connected to its source")
	if e == nil {
		t.Fatal("a successful replication start was not recognised")
	}
	if e.Sev != lsSevOK {
		t.Errorf("severity %q, want ok", e.Sev)
	}
	if !strings.Contains(e.Message, "source") {
		t.Errorf("the record does not name the source: %q", e.Message)
	}
}

// TestLogSummaryStandaloneStateTrack — a server that is not a cluster member has no wsrep
// state machine, and must not be judged by one.
//
// The first version reused the Galera vocabulary for every MySQL log, so a plain replica
// sat in CLOSED from its first start-up record onward — nothing ever moved it out. A live,
// entirely healthy three-node replication topology was reported as three servers that had
// not served a query in thirteen minutes.
func TestLogSummaryStandaloneStateTrack(t *testing.T) {
	b := lsLoadScenario(t, "r07-source-crash")

	for _, s := range b.Sources {
		if s.Flavour == lsFlavourGalera {
			t.Fatalf("%s was read as a Galera member; the fixture is plain replication", s.Name)
		}
	}
	// The source crashed and came back, so its track must show both, and end up running.
	var sawDown, sawUp bool
	for _, p := range b.Phases {
		if p.Src != 0 {
			continue
		}
		switch p.State {
		case lsStateDown:
			sawDown = true
		case lsStateUp:
			sawUp = true
		case lsStateSynced, lsStateJoiner, lsStateJoined, lsStateDonor, lsStatePrim:
			t.Errorf("a non-cluster server was given the Galera state %q", p.State)
		}
	}
	if !sawDown {
		t.Error("the crash did not put the source in DOWN")
	}
	if !sawUp {
		t.Error("the restart did not put the source back in RUNNING")
	}
	// And the unavailability finding must count only the real outage, not the whole window.
	if f := lsHasFinding(b, "unavailable"); f != nil {
		if strings.Contains(f.Detail, "CLOSED") {
			t.Errorf("a non-cluster server was reported as CLOSED: %q", f.Detail)
		}
	}
}

// TestLogSummaryCrashFindingIsNotDuplicated — one crash, one entry, even on a build that
// re-emits the whole crash report through the error log.
func TestLogSummaryCrashFindingIsNotDuplicated(t *testing.T) {
	b := lsLoadScenario(t, "r07-source-crash")
	f := lsHasFinding(b, "crash")
	if f == nil {
		t.Fatal("no crash finding")
	}
	if strings.Contains(f.Detail, "Crash report") {
		t.Errorf("the re-emitted report lines were listed as crashes of their own: %q", f.Detail)
	}
	if !strings.Contains(f.Detail, "signal 11") {
		t.Errorf("the crash finding does not name the signal: %q", f.Detail)
	}
}

// TestLogSummaryServingStates pins the predicate the unavailability finding rests on.
// Two states mean "answering queries" and they belong to different worlds: SYNCED for a
// cluster member, RUNNING for a server that is not one.
func TestLogSummaryServingStates(t *testing.T) {
	for _, st := range []string{lsStateSynced, lsStateUp} {
		if !lsStateServes(st) {
			t.Errorf("%s should count as serving", st)
		}
	}
	for _, st := range []string{lsStateJoiner, lsStateJoined, lsStateDonor, lsStatePrim,
		lsStateOpen, lsStateClosed, lsStateDown, lsStateStarting, "UNKNOWN"} {
		if lsStateServes(st) {
			t.Errorf("%s should not count as serving", st)
		}
	}
	// And end to end: a healthy replication topology must not be reported as an outage.
	b := lsLoadScenario(t, "r06-stop-start-replica")
	if f := lsHasFinding(b, "unavailable"); f != nil {
		t.Errorf("a healthy replica was reported as not serving: %q", f.Detail)
	}
}
