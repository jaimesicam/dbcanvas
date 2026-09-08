package main

import (
	"bytes"
	"compress/gzip"
	"os"
	"strings"
	"testing"
)

// testdata/cluster-dump.tar.gz is a real pt-k8s-debug-collector archive, not a
// hand-written fixture: percona-toolkit 3.6.0's collector run with -resource pxc
// against a k3s v1.36.4 cluster carrying a PerconaXtraDBCluster CRD and three
// deliberately broken workloads — a StatefulSet that cannot be scheduled
// (200Gi memory request), a Deployment whose container exits 1 on a loop, and a
// pod pulling from a registry that does not resolve. The layout this parser reads
// is documented nowhere, so the fixture is the specification.
func loadOpDump(t *testing.T) *opModel {
	t.Helper()
	data, err := os.ReadFile("testdata/cluster-dump.tar.gz")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	m, err := parseOpDump(data, "cluster-dump.tar.gz")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m
}

func TestOpSummaryReadsARealArchive(t *testing.T) {
	m := loadOpDump(t)

	if len(m.Nodes) != 1 || !m.Nodes[0].Ready {
		t.Fatalf("nodes = %+v, want one Ready node", m.Nodes)
	}
	if !strings.HasPrefix(m.Kubernetes, "v1.36") {
		t.Errorf("kubernetes = %q, want the kubelet version", m.Kubernetes)
	}
	// The collector writes a file per resource per namespace whether or not
	// anything exists, so most of the archive is empty lists. Only namespaces
	// that actually hold pods should reach the model.
	var haveDemo bool
	for _, ns := range m.Namespaces {
		if ns.Name == "demo-db" {
			haveDemo = true
			if ns.Problems == 0 {
				t.Errorf("demo-db problems = 0, want the broken pods counted")
			}
		}
	}
	if !haveDemo {
		t.Fatalf("namespaces = %+v, want demo-db", m.Namespaces)
	}
}

func TestOpSummaryFindsShortWorkloads(t *testing.T) {
	m := loadOpDump(t)
	want := map[string][2]int{
		"cluster1-pxc": {0, 3}, // StatefulSet, unschedulable
		"crasher":      {0, 2}, // Deployment, crash-looping
	}
	seen := map[string]bool{}
	for _, w := range m.Workloads {
		if exp, ok := want[w.Name]; ok && w.Namespace == "demo-db" {
			seen[w.Name] = true
			if w.Ready != exp[0] || w.Desired != exp[1] {
				t.Errorf("%s %s = %d/%d, want %d/%d", w.Kind, w.Name, w.Ready, w.Desired, exp[0], exp[1])
			}
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("workload %s missing from the model", name)
		}
	}
	// Short workloads sort first — the reason to open the page is what is down.
	if len(m.Workloads) > 0 && !m.Workloads[0].short() {
		t.Errorf("workloads[0] = %+v, want a short one first", m.Workloads[0])
	}
}

func TestOpSummaryClassifiesBrokenPods(t *testing.T) {
	m := loadOpDump(t)
	reasons := map[string]string{}
	for _, p := range m.Pods {
		reasons[p.Name] = p.Reason
	}
	if got := reasons["puller"]; got != "ImagePullBackOff" && got != "ErrImagePull" {
		t.Errorf("puller reason = %q, want an image-pull failure", got)
	}
	if got := reasons["cluster1-pxc-0"]; got != "Unschedulable" {
		t.Errorf("cluster1-pxc-0 reason = %q, want Unschedulable", got)
	}
	// The unschedulable pod's message is the scheduler's own explanation, which
	// is the single most useful string in the whole archive for that failure.
	for _, p := range m.Pods {
		if p.Name == "cluster1-pxc-0" && !strings.Contains(p.Message, "Insufficient memory") {
			t.Errorf("cluster1-pxc-0 message = %q, want the scheduler's reason", p.Message)
		}
	}
	var crashers int
	for name, reason := range reasons {
		if strings.HasPrefix(name, "crasher-") {
			crashers++
			// The capture is one instant: of these two identically-broken pods,
			// one was caught mid-crash (Error) and the other in the moment
			// between restarts, Running and Ready (Restarting). Both must be
			// reported — a crash loop that happens to be up is still a crash loop.
			switch reason {
			case "CrashLoopBackOff", "Error", "Restarting":
			default:
				t.Errorf("%s reason = %q, want a crash reason", name, reason)
			}
		}
	}
	if crashers != 2 {
		t.Errorf("crashers = %d, want 2", crashers)
	}
	// The restart count has to reach the model, since for the Running one it is
	// the only sign anything is wrong.
	for _, p := range m.Pods {
		if strings.HasPrefix(p.Name, "crasher-") && p.Restarts == 0 {
			t.Errorf("%s restarts = 0, want the crash loop counted", p.Name)
		}
	}
	// A healthy pod must never reach the model — a summary of fine pods is not one.
	for _, p := range m.Pods {
		if p.Reason == "" {
			t.Errorf("pod %s has no reason and should not be listed", p.Name)
		}
	}
}

func TestOpSummaryReadsPodLogs(t *testing.T) {
	m := loadOpDump(t)
	// The archive's logs.txt is the *current* container instance's log, so a pod
	// captured just after a restart has only the first line or two of the new
	// run — the collector keeps no --previous log. Every crasher must therefore
	// have a tail, but only the one caught late in its run carries the error.
	var withTail, withError int
	for _, p := range m.Pods {
		if !strings.HasPrefix(p.Name, "crasher-") {
			continue
		}
		if len(p.LogTail) == 0 {
			t.Errorf("%s has no log tail", p.Name)
			continue
		}
		withTail++
		if strings.Contains(strings.Join(p.LogTail, "\n"), "cannot reach cluster1-pxc") {
			withError++
		}
	}
	if withTail != 2 {
		t.Errorf("crashers with a log tail = %d, want 2", withTail)
	}
	if withError == 0 {
		t.Error("no crasher log carried the container's own error line")
	}
}

func TestOpSummaryReadsCustomResources(t *testing.T) {
	m := loadOpDump(t)
	if len(m.CRs) != 1 {
		t.Fatalf("crs = %d, want exactly the one PerconaXtraDBCluster (empty CR lists must be skipped)", len(m.CRs))
	}
	cr := m.CRs[0]
	if cr.Kind != "PerconaXtraDBCluster" || cr.Name != "cluster1" || cr.Namespace != "demo-db" {
		t.Errorf("cr = %+v, want PerconaXtraDBCluster demo-db/cluster1", cr)
	}
	if cr.Group != "pxc.percona.com" {
		t.Errorf("group = %q, want pxc.percona.com parsed out of the file name", cr.Group)
	}
	if cr.State != "initializing" {
		t.Errorf("state = %q, want initializing", cr.State)
	}
	if cr.Version != "1.15.0" {
		t.Errorf("version = %q, want spec.crVersion", cr.Version)
	}
	comps := map[string][2]int{}
	for _, c := range cr.Components {
		comps[c.Name] = [2]int{c.Ready, c.Size}
	}
	if comps["pxc"] != [2]int{1, 3} {
		t.Errorf("pxc component = %v, want 1/3", comps["pxc"])
	}
	if comps["haproxy"] != [2]int{2, 2} {
		t.Errorf("haproxy component = %v, want 2/2", comps["haproxy"])
	}
	var sawError bool
	for _, c := range cr.Conditions {
		if c.Type == "Error" && strings.Contains(c.Message, "cluster1-secrets") {
			sawError = true
		}
	}
	if !sawError {
		t.Errorf("conditions = %+v, want the ErrorReconcile condition", cr.Conditions)
	}
}

func TestOpSummaryVerdictsAndFindings(t *testing.T) {
	m := loadOpDump(t)
	tone := map[string]string{}
	for _, v := range m.Verdicts {
		tone[v.Key] = v.Tone
	}
	// Every question is answered, including the ones whose answer is "fine".
	for _, key := range []string{"nodes", "workloads", "pods", "operator"} {
		if tone[key] == "" {
			t.Errorf("no verdict for %q — a missing question reads as unchecked", key)
		}
	}
	if tone["nodes"] != "good" {
		t.Errorf("nodes verdict = %q, want good (the one node is Ready)", tone["nodes"])
	}
	for _, key := range []string{"workloads", "pods", "operator"} {
		if tone[key] != "bad" {
			t.Errorf("%s verdict = %q, want bad on this capture", key, tone[key])
		}
	}
	if len(m.Findings) == 0 {
		t.Fatal("no findings")
	}
	if m.Findings[0].Severity != "critical" {
		t.Errorf("findings[0] = %+v, want critical first", m.Findings[0])
	}
}

func TestOpSummaryRejectsRubbish(t *testing.T) {
	if _, err := parseOpDump([]byte("not a gzip"), "x"); err == nil {
		t.Error("want an error on a non-gzip upload")
	}
	// A valid gzip that is not a cluster-dump must say so rather than render empty.
	if _, err := parseOpDump(gzipBytes(t, "hello"), "x"); err == nil {
		t.Error("want an error on a gzip that is not a tar")
	}
}

// gzipBytes makes a valid gzip stream that is not a tar, for the rubbish check.
func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	zw.Close()
	return buf.Bytes()
}

// testdata/cluster-dump-pxc.tar.gz is a second real archive, and a much richer
// one: percona-toolkit 3.6.0's collector run with -resource pxc against a live
// Percona XtraDB Cluster operator 1.20.0 deployment — three PXC members, HAProxy,
// PMM, S3 backup storage — captured deliberately *while a backup was running*, so
// it carries an in-flight PerconaXtraDBClusterBackup. It is what the deployment,
// image, secret, backup, certificate and storage panels are written against.
func loadPXCDump(t *testing.T) *opModel {
	t.Helper()
	data, err := os.ReadFile("testdata/cluster-dump-pxc.tar.gz")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	m, err := parseOpDump(data, "cluster-dump-pxc.tar.gz")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return m
}

func TestOpSummaryIdentifiesTheDeployment(t *testing.T) {
	m := loadPXCDump(t)
	d := m.Deployment
	if d == nil {
		t.Fatal("no deployment identified")
	}
	if d.Operator != "pxc" {
		t.Errorf("operator = %q, want pxc", d.Operator)
	}
	// The operator's version is the tag on its own Deployment image — the only
	// place in the archive it is actually stated.
	if d.Version != "1.20.0" || d.OperatorName != "percona/percona-xtradb-cluster-operator" {
		t.Errorf("operator image = %s:%s, want percona/percona-xtradb-cluster-operator:1.20.0", d.OperatorName, d.Version)
	}
	if d.CRVersion != "1.20.0" {
		t.Errorf("crVersion = %q, want 1.20.0", d.CRVersion)
	}
	if d.PMM == "" {
		t.Errorf("pmm = %q, want the pmm-client image this deployment runs", d.PMM)
	}
	if d.Kubernetes == "" {
		t.Error("kubernetes version missing")
	}
}

func TestOpSummaryListsDeploymentImages(t *testing.T) {
	m := loadPXCDump(t)
	byRepo := map[string]string{}
	for _, img := range m.Images {
		byRepo[img.Repo] = img.Tag
		if img.Count == 0 || len(img.Used) == 0 {
			t.Errorf("image %s has no users", img.Image)
		}
	}
	for repo, want := range map[string]string{
		"percona/percona-xtradb-cluster":          "8.4.8-8.1",
		"percona/haproxy":                         "2.8.18-1",
		"percona/pmm-client":                      "3.8.0",
		"percona/percona-xtrabackup":              "8.4.0-5.1", // only present because a backup was running
		"percona/percona-xtradb-cluster-operator": "1.20.0",
	} {
		if byRepo[repo] != want {
			t.Errorf("image %s = %q, want %q", repo, byRepo[repo], want)
		}
	}
	// The cluster's own plumbing is not part of "what is this deployment made of".
	for _, img := range m.Images {
		if strings.Contains(img.Repo, "metallb") || strings.Contains(img.Repo, "local-path") || strings.Contains(img.Repo, "coredns") {
			t.Errorf("system image %s should not be in the deployment's image list", img.Image)
		}
	}
}

func TestOpSummaryMapsSecretReferences(t *testing.T) {
	m := loadPXCDump(t)
	kinds := map[string]string{}
	for _, s := range m.Secrets {
		kinds[s.Name] = s.Kind
	}
	// Mounted by pods.
	for _, want := range []string{"k3d-00-ssl", "k3d-00-ssl-internal", "k3d-00-vault"} {
		if kinds[want] == "" {
			t.Errorf("secret %s missing", want)
		}
	}
	// Named by the custom resource but mounted by nothing — this is the case that
	// matters, because a renamed or missing one is a classic operator failure and
	// nothing else in the archive would mention it.
	if kinds["k3d-00-backup-s3"] != "referenced" {
		t.Errorf("k3d-00-backup-s3 kind = %q, want referenced (it is named by the CR's backup storage)", kinds["k3d-00-backup-s3"])
	}
	// No content, ever: the collector does not dump Secret objects and this must
	// not invent a way to.
	for _, s := range m.Secrets {
		if strings.Contains(strings.ToLower(s.Kind), "value") || strings.Contains(s.Name, "=") {
			t.Errorf("secret entry looks like it carries content: %+v", s)
		}
	}
}

func TestOpSummaryReadsAnInFlightBackup(t *testing.T) {
	m := loadPXCDump(t)
	if len(m.Backups) != 1 {
		t.Fatalf("backups = %d, want the one that was running during the capture", len(m.Backups))
	}
	b := m.Backups[0]
	if b.Name != "backup-diag-1" || b.Cluster != "k3d-00" {
		t.Errorf("backup = %+v, want backup-diag-1 of k3d-00", b)
	}
	if !backupRunning(b.State) {
		t.Errorf("state = %q, want a running state", b.State)
	}
	if b.Storage != "seaweedfs" || b.StorageType != "s3" {
		t.Errorf("storage = %s/%s, want seaweedfs/s3", b.Storage, b.StorageType)
	}
	if !strings.HasPrefix(b.Destination, "s3://") {
		t.Errorf("destination = %q, want the s3 destination", b.Destination)
	}
	if b.Image == "" {
		t.Error("backup image missing — it is what actually took the backup")
	}
	// A capture taken mid-backup must say so rather than implying success.
	var tone string
	for _, v := range m.Verdicts {
		if v.Key == "backups" {
			tone = v.Tone
		}
	}
	if tone != "warn" {
		t.Errorf("backups verdict = %q, want warn for an in-flight backup", tone)
	}
}

func TestOpSummaryReadsCertificateExpiry(t *testing.T) {
	m := loadPXCDump(t)
	if len(m.Certs) < 4 {
		t.Fatalf("certs = %d, want the ssl and ssl-internal secrets' ca.crt and tls.crt", len(m.Certs))
	}
	// Soonest expiry first: the panel exists for that one number.
	if m.Certs[0].DaysLeft == nil {
		t.Fatal("first cert has no expiry")
	}
	for i := 1; i < len(m.Certs); i++ {
		if m.Certs[i].DaysLeft != nil && *m.Certs[i].DaysLeft < *m.Certs[0].DaysLeft {
			t.Errorf("certs are not sorted by expiry: %d before %d", *m.Certs[0].DaysLeft, *m.Certs[i].DaysLeft)
		}
	}
	var sawLeaf, sawCA bool
	for _, c := range m.Certs {
		if c.Entry == "tls.crt" && c.Subject == "O = PXC" {
			sawLeaf = true
		}
		if c.Entry == "ca.crt" {
			sawCA = true
		}
		// These clusters have no cert-manager, so everything is operator-signed by
		// its own "Root CA" — which is worth stating rather than leaving implied.
		if !c.SelfSigned {
			t.Errorf("cert %s/%s should be detected as self-signed (issuer %q)", c.Secret, c.Entry, c.Issuer)
		}
	}
	if !sawLeaf || !sawCA {
		t.Errorf("want both a leaf and a CA certificate, got %+v", m.Certs)
	}
}

func TestOpSummaryReadsStorage(t *testing.T) {
	m := loadPXCDump(t)
	if len(m.Storage) != 3 {
		t.Fatalf("pvcs = %d, want one per PXC member", len(m.Storage))
	}
	for _, v := range m.Storage {
		if v.Status != "Bound" {
			t.Errorf("%s is %s, want Bound", v.Name, v.Status)
		}
		if v.Capacity == "" || v.StorageClass == "" {
			t.Errorf("pvc %+v is missing capacity or storage class", v)
		}
	}
}

func TestOpSummaryAnswersEveryQuestionOnARealCluster(t *testing.T) {
	m := loadPXCDump(t)
	got := map[string]bool{}
	for _, v := range m.Verdicts {
		got[v.Key] = true
	}
	// Backups and certs only appear when the capture has them — this one does.
	for _, key := range []string{"nodes", "workloads", "pods", "backups", "certs", "operator"} {
		if !got[key] {
			t.Errorf("no verdict for %q on a capture that contains it", key)
		}
	}
}

// The failed-backup path, from a real failure: a PerconaServerMongoDBBackup that
// died with `state: error, error: "starting deadline seconds exceeded"`. A backup
// whose failure goes unnoticed is discovered when it is needed, so this is the
// loudest thing the summary can say.
func TestOpSummaryReportsFailedBackups(t *testing.T) {
	m := &opModel{
		Available: map[string]bool{},
		Backups: []opBackup{
			{Kind: "PerconaServerMongoDBBackup", Namespace: "psmdb", Name: "backup-diag-1",
				State: "error", Error: "starting deadline seconds exceeded"},
			{Kind: "PerconaXtraDBClusterBackup", Namespace: "pxc", Name: "nightly", State: "Succeeded"},
		},
	}
	computeOpFindings(m)
	computeOpVerdicts(m)

	var found *opFinding
	for i := range m.Findings {
		if strings.Contains(m.Findings[i].Title, "backup-diag-1") {
			found = &m.Findings[i]
		}
	}
	if found == nil {
		t.Fatal("the failed backup produced no finding")
	}
	if found.Severity != "critical" {
		t.Errorf("severity = %q, want critical", found.Severity)
	}
	// The operator's own error text is the answer; paraphrasing it loses the answer.
	if found.Detail != "starting deadline seconds exceeded" {
		t.Errorf("detail = %q, want the operator's own error", found.Detail)
	}
	for _, v := range m.Verdicts {
		if v.Key == "backups" {
			if v.Tone != "bad" {
				t.Errorf("backups verdict = %q, want bad", v.Tone)
			}
			if !strings.Contains(v.Title, "1 of 2") {
				t.Errorf("title = %q, want the failed count against the total", v.Title)
			}
		}
	}
}

func TestOpBackupStateHelpers(t *testing.T) {
	// The operators disagree on spelling, so these match on substance.
	for _, s := range []string{"error", "Error", "failed", "Failed", "FAILED"} {
		if !backupFailed(s) {
			t.Errorf("backupFailed(%q) = false", s)
		}
	}
	for _, s := range []string{"Running", "Starting", "requested", "Waiting"} {
		if !backupRunning(s) {
			t.Errorf("backupRunning(%q) = false", s)
		}
	}
	for _, s := range []string{"Succeeded", "ready", ""} {
		if backupFailed(s) || backupRunning(s) {
			t.Errorf("%q should be neither failed nor running", s)
		}
	}
}

// An expired or nearly-expired certificate is a deadline nothing else in the
// archive carries.
func TestOpSummaryReportsCertificateExpiry(t *testing.T) {
	days := func(n int) *int { return &n }
	m := &opModel{
		Available: map[string]bool{},
		Certs: []opCert{
			{Secret: "cluster-ssl", Entry: "tls.crt", NotAfter: "Jan 1 00:00:00 2026 GMT", DaysLeft: days(-5)},
			{Secret: "cluster-ssl", Entry: "ca.crt", NotAfter: "Jan 1 00:00:00 2030 GMT", DaysLeft: days(900)},
		},
	}
	computeOpFindings(m)
	computeOpVerdicts(m)
	var critical bool
	for _, f := range m.Findings {
		if f.Severity == "critical" && strings.Contains(f.Title, "expired") {
			critical = true
		}
	}
	if !critical {
		t.Error("an expired certificate should be critical")
	}
	for _, v := range m.Verdicts {
		if v.Key == "certs" && v.Tone != "bad" {
			t.Errorf("certs verdict = %q, want bad when one has expired", v.Tone)
		}
	}

	// Inside 30 days is a warning, not a failure.
	m2 := &opModel{Available: map[string]bool{}, Certs: []opCert{{Secret: "s", Entry: "tls.crt", DaysLeft: days(12)}}}
	computeOpVerdicts(m2)
	for _, v := range m2.Verdicts {
		if v.Key == "certs" && v.Tone != "warn" {
			t.Errorf("certs verdict = %q, want warn at 12 days", v.Tone)
		}
	}
}

// A PVC that never bound is why a database never started, and says so.
func TestOpSummaryReportsUnboundStorage(t *testing.T) {
	m := &opModel{
		Available: map[string]bool{},
		Storage: []opPVC{
			{Namespace: "pxc", Name: "datadir-pxc-2", Status: "Pending", Requested: "6G", StorageClass: "local-path"},
			{Namespace: "pxc", Name: "datadir-pxc-0", Status: "Bound", Capacity: "6G"},
		},
	}
	computeOpFindings(m)
	var found bool
	for _, f := range m.Findings {
		if strings.Contains(f.Title, "datadir-pxc-2") && f.Severity == "critical" {
			found = true
		}
	}
	if !found {
		t.Errorf("an unbound PVC should be a critical finding, got %+v", m.Findings)
	}
}

// A backup custom resource reporting "Running" while the pods doing the work are
// failing is the case reading the CR alone gets wrong — the operator retries the
// job, so the resource stays Running indefinitely while every attempt dies. Found
// live: a PXC backup sat at Running for ten minutes while its xb- pods failed
// against an unreachable S3 endpoint.
func TestOpSummaryCatchesARunningBackupWithFailingPods(t *testing.T) {
	m := &opModel{
		Available: map[string]bool{},
		Backups:   []opBackup{{Kind: "PerconaXtraDBClusterBackup", Namespace: "pxc", Name: "backup-diag-1", State: "Running"}},
		Pods: []opPod{
			{Namespace: "pxc", Name: "xb-backup-diag-1-csp92", Reason: "Error"},
			{Namespace: "pxc", Name: "xb-backup-diag-1-2xwdd", Reason: "Error"},
			{Namespace: "pxc", Name: "unrelated-pod", Reason: "Error"},
		},
	}
	computeOpFindings(m)
	var found *opFinding
	for i := range m.Findings {
		if strings.Contains(m.Findings[i].Title, "says Running") {
			found = &m.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("no finding for a running backup with failing pods: %+v", m.Findings)
	}
	if found.Severity != "critical" {
		t.Errorf("severity = %q, want critical", found.Severity)
	}
	if !strings.Contains(found.Title, "2 of its pods") {
		t.Errorf("title = %q, want the count of failing pods (and not the unrelated one)", found.Title)
	}
	if strings.Contains(found.Detail, "unrelated-pod") {
		t.Error("an unrelated failing pod must not be attributed to the backup")
	}
}

// Three real archives, one per Percona operator, all captured from live clusters:
// PXC 1.20.0, PSMDB 1.23.0 and PG 3.0.0. The operators disagree about almost every
// shape in their status, so a parser verified against one of them is verified
// against none.
func loadDump(t *testing.T, file string) *opModel {
	t.Helper()
	data, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	m, err := parseOpDump(data, file)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	return m
}

func TestOpSummaryReadsEveryOperator(t *testing.T) {
	for _, tc := range []struct {
		file, operator, version, kind, state string
	}{
		{"cluster-dump-pxc.tar.gz", "pxc", "1.20.0", "PerconaXtraDBCluster", "ready"},
		{"cluster-dump-psmdb.tar.gz", "psmdb", "1.23.0", "PerconaServerMongoDB", "initializing"},
		{"cluster-dump-pg.tar.gz", "pg", "3.0.0", "PerconaPGCluster", "ready"},
	} {
		t.Run(tc.operator, func(t *testing.T) {
			m := loadDump(t, tc.file)
			if m.Deployment == nil || m.Deployment.Operator != tc.operator {
				t.Fatalf("operator = %+v, want %s", m.Deployment, tc.operator)
			}
			if m.Deployment.Version != tc.version {
				t.Errorf("version = %q, want %q", m.Deployment.Version, tc.version)
			}
			if len(m.CRs) != 1 {
				t.Fatalf("crs = %d, want exactly one (empty CR lists in every other namespace must be skipped)", len(m.CRs))
			}
			if m.CRs[0].Kind != tc.kind || m.CRs[0].State != tc.state {
				t.Errorf("cr = %s/%s, want %s/%s", m.CRs[0].Kind, m.CRs[0].State, tc.kind, tc.state)
			}
			if len(m.CRs[0].Components) == 0 {
				t.Errorf("%s reported no components — the operators publish these under different shapes and both have to work", tc.kind)
			}
			if m.Deployment.PMM == "" {
				t.Errorf("pmm client not detected")
			}
			if len(m.Images) == 0 || len(m.Storage) == 0 {
				t.Errorf("images = %d, storage = %d, want both populated", len(m.Images), len(m.Storage))
			}
		})
	}
}

// PSMDB publishes its components as a *map* of replica-set name to status, where
// PXC publishes a flat object. Reading only the flat shape is why a healthy PSMDB
// cluster reported no components at all while its replica set sat at 3/3.
func TestOpSummaryReadsNestedCRComponents(t *testing.T) {
	m := loadDump(t, "cluster-dump-psmdb.tar.gz")
	var found bool
	for _, c := range m.CRs[0].Components {
		if c.Name == "replsets/rs0" {
			found = true
			if c.Ready != 3 || c.Size != 3 {
				t.Errorf("replsets/rs0 = %d/%d, want 3/3", c.Ready, c.Size)
			}
		}
	}
	if !found {
		t.Errorf("components = %+v, want replsets/rs0", m.CRs[0].Components)
	}
}

// The archive's log-shaped files go through Log Summary's classifiers rather than
// a second set written here. This asserts the bridge, not the classifiers: that
// Kubernetes Events, the operator's log and each database's error log are found,
// labelled, and produce findings.
func TestOpSummaryBridgesToLogSummary(t *testing.T) {
	data, err := os.ReadFile("testdata/cluster-dump-pxc.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	f, err := readOpArchive(data)
	if err != nil {
		t.Fatal(err)
	}
	inputs := opLogInputs(f)
	if len(inputs) == 0 {
		t.Fatal("no log inputs found in the archive")
	}
	// events.yaml is the one file that needs rewriting: the classifier reads the
	// JSON form and the collector writes YAML.
	var events, k8s int
	for _, in := range inputs {
		if strings.HasSuffix(in.Name, "/events") {
			events++
			if in.Engine != pktEngineK8sEvents {
				t.Errorf("%s engine = %q, want %q", in.Name, in.Engine, pktEngineK8sEvents)
			}
			if len(in.Data) == 0 || in.Data[0] != '{' {
				t.Errorf("%s was not converted to JSON — lsSniffK8sEvents will not recognise it", in.Name)
			}
			k8s++
		}
	}
	if events == 0 {
		t.Error("no Kubernetes Events input built from the archive")
	}

	b := lsBuild(inputs)
	byFlavour := map[string]int{}
	for _, s := range b.Sources {
		byFlavour[s.Flavour]++
	}
	// The sniffers, not filenames, decide these: a Galera member's error log and
	// an operator's zap log are both just "a log" until something reads them.
	if byFlavour["galera"] == 0 {
		t.Errorf("no source classified as galera, got %v", byFlavour)
	}
	if byFlavour["pxcoperator"] == 0 {
		t.Errorf("the operator log was not classified as a PXC operator log, got %v", byFlavour)
	}
	if byFlavour["k8sevents"] == 0 {
		t.Errorf("Kubernetes Events were not classified, got %v", byFlavour)
	}
	if len(b.Finding) == 0 {
		t.Error("Log Summary produced no findings from a real archive")
	}

	d := opDigestLogs(b)
	if d == nil || d.Sources != len(b.Sources) || len(d.Findings) != len(b.Finding) {
		t.Errorf("digest = %+v, want it to carry every source and finding", d)
	}
	if d.Events != len(b.Events) {
		t.Errorf("digest events = %d, want %d", d.Events, len(b.Events))
	}
	// The digest is a digest: it must not carry the whole event stream to a browser.
	if len(d.Worst) > 20 {
		t.Errorf("worst = %d events, want at most 20", len(d.Worst))
	}
}

// Galera's two state files decide whether a stopped cluster can come back.
func TestOpSummaryReadsGaleraState(t *testing.T) {
	m := loadDump(t, "cluster-dump-pxc.tar.gz")
	if len(m.Galera) != 3 {
		t.Fatalf("galera = %d, want one per PXC member", len(m.Galera))
	}
	for _, g := range m.Galera {
		if g.UUID == "" || g.Seqno == "" {
			t.Errorf("%s has no uuid/seqno: %+v", g.Pod, g)
		}
		// These members were running when the capture was taken, so gvwstate.dat
		// exists and grastate.dat has not been finalised.
		if !g.HasView {
			t.Errorf("%s should hold a view while running", g.Pod)
		}
	}
	// A running cluster has no member marked safe to bootstrap, which is worth
	// saying out loud rather than leaving as an absence.
	var warned bool
	for _, f := range m.Findings {
		if strings.Contains(f.Title, "safe to bootstrap") {
			warned = true
		}
	}
	if !warned {
		t.Error("want a finding about no member being safe to bootstrap")
	}
	// PSMDB and PG have no Galera state and must not invent any.
	if len(loadDump(t, "cluster-dump-psmdb.tar.gz").Galera) != 0 {
		t.Error("a MongoDB capture should have no Galera state")
	}
}

// The backup-log reader must not cry wolf. The first cut matched "error" anywhere,
// which flagged every `performance_schema/events_errors_su_152.sdi` xtrabackup
// streamed — a table name — in a log whose last line is `completed OK!`.
func TestOpSummaryBackupLogsDoNotCryWolf(t *testing.T) {
	m := loadDump(t, "cluster-dump-pxc.tar.gz")
	if len(m.BackupLogs) == 0 {
		t.Fatal("no backup logs read")
	}
	for _, l := range m.BackupLogs {
		for _, e := range l.Errors {
			if strings.Contains(e, ".sdi") || strings.Contains(e, "[Note]") {
				t.Errorf("%s/%s: %q is not an error", l.Pod, l.File, e)
			}
		}
		if l.CompletedOK && len(l.Errors) > 0 {
			t.Errorf("%s/%s says completed OK yet reports errors: %v", l.Pod, l.File, l.Errors)
		}
	}
	// A file holding only the collector's own `tar: … Cannot stat` is a file that
	// was never written, and says nothing about the cluster.
	for _, l := range m.BackupLogs {
		if len(l.Tail) == 1 && strings.HasPrefix(l.Tail[0], "tar:") {
			t.Errorf("%s/%s is collector noise and should have been dropped", l.Pod, l.File)
		}
	}
	// And a genuine failure still has to be caught.
	if !opBackupLogFailRe.MatchString("xbcloud: Probe failed. Please check your credentials and endpoint settings.") {
		t.Error("the real xbcloud failure is not matched")
	}
	if opBackupLogFailRe.MatchString("[Note] [Xtrabackup] Streaming performance_schema/events_errors_su_152.sdi") {
		t.Error("a table name containing 'errors' is matched as a failure")
	}
}

// The Log Summary integration must not depend on where the archive came from. An
// uploaded cluster-dump — from a customer's cluster this installation has never
// seen — gets the same classifiers and the same registered bundle as a capture
// taken here, so the timeline is one click away either way.
func TestOpSummaryRegistersLogsForAnUpload(t *testing.T) {
	app := newTestApp(t)
	u, err := app.store.CreateUser("dumper", "x", RoleUser, StatusApproved)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("testdata/cluster-dump-pxc.tar.gz")
	if err != nil {
		t.Fatal(err)
	}

	// "upload" is the origin an archive from elsewhere carries; it has no stack.
	m, err := app.opAnalyse(u, data, "customer-cluster.tar.gz", "upload", 0, "")
	if err != nil {
		t.Fatalf("analyse: %v", err)
	}
	if m.Logs == nil || m.Logs.BundleID == "" {
		t.Fatal("an uploaded archive produced no Log Summary bundle — the timeline would be unreachable")
	}
	if len(m.Logs.Findings) == 0 {
		t.Error("no log findings from an uploaded archive")
	}

	// The bundle is real, owned by the uploader, and holds every source.
	rec := lsGet(m.Logs.BundleID)
	if rec == nil {
		t.Fatalf("bundle %s was not registered", m.Logs.BundleID)
	}
	if rec.Origin != "upload" {
		t.Errorf("origin = %q, want upload", rec.Origin)
	}
	if len(rec.Sources) != m.Logs.Sources {
		t.Errorf("bundle has %d sources, digest says %d", len(rec.Sources), m.Logs.Sources)
	}
	// It must be reachable by the account that uploaded it, which is what makes
	// the handoff work at all.
	var mine bool
	for _, r := range lsListFor(u) {
		if r.ID == m.Logs.BundleID {
			mine = true
		}
	}
	if !mine {
		t.Error("the uploader cannot see their own bundle in Log Summary")
	}

	// parseOpDump on its own stays pure: it classifies but registers nothing, so
	// the tests above and any future caller get no global side effect.
	pure, err := parseOpDump(data, "x")
	if err != nil {
		t.Fatal(err)
	}
	if pure.Logs == nil || pure.Logs.BundleID != "" {
		t.Errorf("parseOpDump should classify but not register, got bundleId %q", pure.Logs.BundleID)
	}
}

// A pod's logs.txt is `kubectl logs <pod>`, which on a multi-container pod returns
// ONE container's output and not predictably the first. Measured across three live
// clusters: a PSMDB pod runs [mongod backup-agent pmm-client logs logrotate] and
// the archive held logrotate's stdout; a PG pod runs [database … pgbackrest
// pgbackrest-config] and two of three instances gave pgbackrest and pmm-client.
//
// So for MongoDB and PostgreSQL the server's own log is not in the archive at all,
// and the only reason a PXC capture has one is that the collector copies
// /var/lib/mysql/*.log off the filesystem separately. Left implicit that reads as
// "the logs were fine".
func TestOpSummarySaysWhichServerLogsAreMissing(t *testing.T) {
	for _, tc := range []struct {
		file, engine string
		want         int
	}{
		{"cluster-dump-psmdb.tar.gz", "mongod", 3}, // one per replica-set member
		{"cluster-dump-pg.tar.gz", "postgres", 3},  // one per instance
		{"cluster-dump-pxc.tar.gz", "", 0},         // collected off disk, so nothing to say
	} {
		t.Run(tc.file, func(t *testing.T) {
			m := loadDump(t, tc.file)
			var notes int
			for _, f := range m.Findings {
				if strings.Contains(f.Title, "server log for") {
					notes++
					if f.Severity != "note" {
						t.Errorf("severity = %q, want note — this is a gap in the capture, not a fault in the cluster", f.Severity)
					}
					if tc.engine != "" && !strings.Contains(f.Title, tc.engine) {
						t.Errorf("title = %q, want it to name %s", f.Title, tc.engine)
					}
					// The detail has to name the containers, or the reader cannot
					// tell what they are looking at instead.
					if !strings.Contains(f.Detail, "stdout") {
						t.Errorf("detail = %q, want it to explain the one-container-stdout limit", f.Detail)
					}
				}
			}
			if notes != tc.want {
				t.Errorf("missing-server-log notes = %d, want %d", notes, tc.want)
			}
		})
	}
}

// The pod stdout source is named for what it is. Calling it
// "k3d-01-rs0-0/logs.txt" in Log Summary reads like the mongod log and is not.
func TestOpSummaryNamesPodStdoutHonestly(t *testing.T) {
	data, err := os.ReadFile("testdata/cluster-dump-psmdb.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	f, err := readOpArchive(data)
	if err != nil {
		t.Fatal(err)
	}
	var sawStdout, sawBareLogsTxt int
	for _, in := range opLogInputs(f) {
		if strings.HasSuffix(in.Name, " (stdout)") {
			sawStdout++
		}
		if strings.HasSuffix(in.Name, "/logs.txt") {
			sawBareLogsTxt++
		}
	}
	if sawStdout == 0 {
		t.Error("no source named as pod stdout")
	}
	if sawBareLogsTxt != 0 {
		t.Errorf("%d sources are still named <pod>/logs.txt, which reads like the database's own log", sawBareLogsTxt)
	}
}

// testdata/cluster-dump-partial.tar.gz is a real capture taken while the cluster
// was still coming up — 176 KB against 379 KB for one of the same cluster minutes
// later. `kubectl logs --all-containers` fails for the whole pod if ANY container
// in it is still PodInitializing, so those pods contributed no log at all, and a
// mysqld that had not started had written no error log to collect either. The
// archive holds three pod logs and no mysqld-error.log.
//
// This is the case that looks most like a bug in Operator Summary and is not one,
// so the page has to say which of the two it is.
func TestOpSummaryFlagsACaptureTakenDuringStartup(t *testing.T) {
	m := loadDump(t, "cluster-dump-partial.tar.gz")

	var warned *opFinding
	for i := range m.Findings {
		if strings.Contains(m.Findings[i].Title, "still starting") {
			warned = &m.Findings[i]
		}
	}
	if warned == nil {
		t.Fatal("a capture taken mid-startup produced no warning — it reads as 'the logs were not parsed'")
	}
	if warned.Severity != "warning" {
		t.Errorf("severity = %q, want warning", warned.Severity)
	}
	if !strings.Contains(warned.Detail, "another capture") {
		t.Errorf("detail = %q, want it to say what to do about it", warned.Detail)
	}

	// The archive really does lack the server logs, so the log bundle is thin.
	// That is the fact the warning exists to explain, not something to paper over.
	if m.Logs == nil {
		t.Fatal("no log bundle at all")
	}
	var galera int
	for _, f := range m.Findings {
		if strings.Contains(f.Title, "mysqld-error") {
			galera++
		}
	}
	if m.Logs.Sources > 6 {
		t.Errorf("sources = %d, want the handful this degraded capture actually holds", m.Logs.Sources)
	}

	// A complete capture of the same cluster must NOT carry the warning.
	full := loadDump(t, "cluster-dump-pxc.tar.gz")
	for _, f := range full.Findings {
		if strings.Contains(f.Title, "still starting") {
			t.Errorf("a complete capture should not be flagged as taken during startup: %q", f.Title)
		}
	}
	// And it does read the PXC server logs, which is the whole contrast.
	if full.Logs == nil || full.Logs.Sources < 10 {
		t.Errorf("a complete capture should carry every log: %+v", full.Logs)
	}
}
