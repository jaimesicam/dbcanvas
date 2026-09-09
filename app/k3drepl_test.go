package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// k3dFrame builds a PXC-operator Kubernetes frame for the tests below.
func k3dFrame(id, label string) designFrame {
	return designFrame{
		ID: id, Type: "k3d", Label: label,
		K3DOperator: "pxc", K3DExposePXC: "clusterip", K3DExposeHAProxy: "loadbalancer",
		K3DProxy: "haproxy", SeaweedFSNodeID: "sw1",
	}
}

func k3dReplDoc(edgeType string) designDoc {
	return designDoc{
		Frames: []designFrame{k3dFrame("f1", "cluster1"), k3dFrame("f2", "cluster2")},
		Nodes: []designNode{
			{ID: "n1", Type: "k3d", FrameID: "f1"},
			{ID: "n2", Type: "k3d", FrameID: "f2"},
		},
		Edges: []designEdge{{ID: "e1", Type: edgeType, From: edgeEnd{Node: "f1"}, To: edgeEnd{Node: "f2"}}},
	}
}

func TestK3DReplLinksResolveFrames(t *testing.T) {
	links := k3dReplLinks(k3dReplDoc("async"))
	if len(links) != 1 {
		t.Fatalf("want 1 link, got %d", len(links))
	}
	if links[0].Src.ID != "f1" || links[0].Dst.ID != "f2" {
		t.Errorf("From is the source and To the replica; got %s → %s", links[0].Src.ID, links[0].Dst.ID)
	}
	if links[0].Channel != "cluster1_to_cluster2" {
		t.Errorf("channel = %q", links[0].Channel)
	}
}

// Bidirectional is not two links. The operator holds a cluster carrying an inbound channel
// read-only, so expanding it both ways would leave a topology with nowhere to write — and would
// try to seed each cluster from the other.
func TestK3DReplBidirIsOneWay(t *testing.T) {
	links := k3dReplLinks(k3dReplDoc("bidir"))
	if len(links) != 1 {
		t.Fatalf("bidirectional must resolve to one link, got %d", len(links))
	}
	if links[0].Src.ID != "f1" {
		t.Errorf("the drawn direction must win; got source %s", links[0].Src.ID)
	}
}

// A node-to-node replication edge belongs to replication.go and must not be picked up here, or a
// PXC cluster pair would be set up twice by two different mechanisms.
func TestK3DReplIgnoresMemberLinks(t *testing.T) {
	doc := designDoc{
		Frames: []designFrame{{ID: "f1", Type: "pxc", Label: "a"}, {ID: "f2", Type: "pxc", Label: "b"}},
		Nodes: []designNode{
			{ID: "n1", Type: "pxc", FrameID: "f1"},
			{ID: "n2", Type: "pxc", FrameID: "f2"},
		},
		Edges: []designEdge{{ID: "e1", Type: "async", From: edgeEnd{Node: "n1"}, To: edgeEnd{Node: "n2"}}},
	}
	if got := k3dReplLinks(doc); len(got) != 0 {
		t.Fatalf("a member-to-member link is not a Kubernetes link, got %d", len(got))
	}
	// ...and the reverse: the member resolver must not claim a frame-to-frame one.
	if got := replicationLinks(k3dReplDoc("async")); len(got) != 0 {
		t.Fatalf("a frame-to-frame link is not a member link, got %d", len(got))
	}
}

func TestK3DReplOnlyPXCOperator(t *testing.T) {
	doc := k3dReplDoc("async")
	doc.Frames[1].K3DOperator = "psmdb"
	if got := k3dReplLinks(doc); len(got) != 0 {
		t.Fatalf("only the PXC operator replicates cross-cluster, got %d links", len(got))
	}
}

func TestK3DReplRole(t *testing.T) {
	doc := k3dReplDoc("async")
	if role, ch := k3dReplRole(doc, "f1"); role != "source" || ch != "cluster1_to_cluster2" {
		t.Errorf("f1 = %q/%q, want source", role, ch)
	}
	if role, _ := k3dReplRole(doc, "f2"); role != "replica" {
		t.Errorf("f2 = %q, want replica", role)
	}
	if role, _ := k3dReplRole(doc, "nope"); role != "" {
		t.Errorf("an unlinked frame has no role, got %q", role)
	}
}

// A cluster name that is not a legal MySQL channel name must not reach CHANGE REPLICATION SOURCE.
func TestK3DReplChannelNameIsSafe(t *testing.T) {
	ch := k3dReplChannelName(
		designFrame{Label: "east-1.prod"},
		designFrame{Label: "west 2"},
	)
	for _, r := range ch {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
		if !ok {
			t.Fatalf("channel %q contains %q", ch, r)
		}
	}
	if !strings.Contains(ch, "_to_") {
		t.Errorf("channel %q should name both ends", ch)
	}
}

// ---------------------------------------------------------------- validation

func k3dReplValidate(doc designDoc) []issue {
	var out []issue
	pairs := map[string]bool{}
	for _, e := range doc.Edges {
		if !isReplEdge(e) {
			continue
		}
		if ends, ok := k3dReplEdgeFrames(doc, e); ok {
			out = append(out, k3dReplIssues(doc, ends, e, pairs)...)
		}
	}
	return out
}

func hasIssue(issues []issue, level, substr string) bool {
	for _, i := range issues {
		if i.Level == level && strings.Contains(i.Message, substr) {
			return true
		}
	}
	return false
}

func TestK3DReplValidationAcceptsAGoodPair(t *testing.T) {
	issues := k3dReplValidate(k3dReplDoc("async"))
	if hasError(issues) {
		t.Fatalf("a well-formed link must not error: %+v", issues)
	}
	// It is still worth two warnings: the replica's data is about to be replaced, and the source's
	// Service type is being overridden.
	if !hasIssue(issues, "warning", "restored from a backup") {
		t.Error("the destructive seed must be warned about")
	}
	if !hasIssue(issues, "warning", "LoadBalancer") {
		t.Error("overriding the frame's Service type must be warned about")
	}
}

func TestK3DReplValidationNeedsABackupStore(t *testing.T) {
	doc := k3dReplDoc("async")
	doc.Frames[1].SeaweedFSNodeID = ""
	if !hasIssue(k3dReplValidate(doc), "error", "SeaweedFS backup store") {
		t.Error("a replica with nowhere to restore from must be an error")
	}
	// Two different stores is fine, and used to be refused. The restore is handed the source
	// store's endpoint by the backup itself, and its credentials are copied into the replica's
	// cluster under a name of their own (k3dReplSeedSecret) — so one object store per cluster,
	// which is how two sites are actually built, now validates.
	doc = k3dReplDoc("async")
	doc.Frames[1].SeaweedFSNodeID = "sw2"
	if iss := k3dReplValidate(doc); hasError(iss) {
		t.Errorf("two SeaweedFS nodes is a legitimate pair: %+v", iss)
	}
}

// Point-in-time recovery is the one thing a replication pair must not share, and only at bucket
// level: two binlog collectors uploading into one bucket interleave two streams, and neither of
// them can be replayed afterwards.
func TestK3DReplValidationPITRBuckets(t *testing.T) {
	clash := func(edit func(*designDoc)) []issue {
		doc := k3dReplDoc("async")
		doc.Frames[0].K3DPITR, doc.Frames[1].K3DPITR = true, true
		edit(&doc)
		return k3dReplValidate(doc)
	}
	// Same node, same bucket: refused.
	same := clash(func(d *designDoc) {
		d.Frames[0].K3DPITRBucket, d.Frames[1].K3DPITRBucket = "binlogs", "binlogs"
	})
	if !hasIssue(same, "error", "same bucket") {
		t.Errorf("two collectors in one bucket must be an error: %+v", same)
	}
	// A bucket each: fine.
	if iss := clash(func(d *designDoc) {
		d.Frames[0].K3DPITRBucket, d.Frames[1].K3DPITRBucket = "binlogs-1", "binlogs-2"
	}); hasError(iss) {
		t.Errorf("a bucket each is the arrangement we are asking for: %+v", iss)
	}
	// Different SeaweedFS nodes: the same bucket NAME is a different bucket.
	if iss := clash(func(d *designDoc) {
		d.Frames[1].SeaweedFSNodeID = "sw2"
		d.Frames[0].K3DPITRBucket, d.Frames[1].K3DPITRBucket = "binlogs", "binlogs"
	}); hasError(iss) {
		t.Errorf("one bucket name on two nodes is two buckets: %+v", iss)
	}
	// Neither names a bucket: both fall back to the same node's default, which is the same
	// bucket — the clash you get by simply ticking the box on both clusters, and the one most
	// worth catching. k3dPITRBucketOf is what has to see that they agree.
	if iss := clash(func(*designDoc) {}); !hasIssue(iss, "error", "same bucket") {
		t.Errorf("two clusters defaulting to one node's bucket is the clash: %+v", iss)
	}
	// Give them different backup buckets and the binlogs follow, so there is nothing to report.
	if iss := clash(func(d *designDoc) {
		d.Frames[0].SeaweedFSBucket, d.Frames[1].SeaweedFSBucket = "c1", "c2"
	}); hasError(iss) {
		t.Errorf("two backup buckets means two binlog buckets: %+v", iss)
	}
	// And the replica is told its collector starts switched off.
	warn := clash(func(d *designDoc) {
		d.Frames[0].K3DPITRBucket, d.Frames[1].K3DPITRBucket = "binlogs-1", "binlogs-2"
	})
	if !hasIssue(warn, "warning", "point-in-time recovery switched off") {
		t.Errorf("a replica's deferred collector must be said out loud: %+v", warn)
	}
	// PITR with no store at all is an error, and it is reported by k3dBackupIssues rather than
	// here: it is true of any frame, linked or not, and saying it twice for a linked one is noise.
	doc := k3dReplDoc("async")
	doc.Frames[1].K3DPITR = true
	doc.Frames[1].SeaweedFSNodeID = ""
	if hasIssue(k3dReplValidate(doc), "error", "no SeaweedFS backup store") {
		t.Error("the link validator must not repeat what k3dBackupIssues already reports")
	}
	if !hasIssue(k3dBackupIssues(doc.Frames[1], doc), "error", "no SeaweedFS backup store") {
		t.Error("the binlog collector uploads to S3 — without a store that is an error")
	}
	// And on an operator that has no binlog collector at all, the setting is ignored rather than
	// silently doing nothing.
	psmdb := designFrame{Label: "k3d-09", K3DOperator: "psmdb", K3DPITR: true, SeaweedFSNodeID: "sw1"}
	if !hasIssue(k3dBackupIssues(psmdb, doc), "warning", "is ignored") {
		t.Error("PITR on a non-PXC operator must say it does nothing")
	}
}

// k3dPITRBucketOf answers "which bucket do this frame's binary logs go to" from the design alone,
// which is what makes the clash check above correct without resolving anything against a node.
func TestK3DPITRBucketOf(t *testing.T) {
	if got := k3dPITRBucketOf(designFrame{K3DPITRBucket: " binlogs "}); got != "binlogs" {
		t.Errorf("an explicit bucket wins, trimmed: %q", got)
	}
	if got := k3dPITRBucketOf(designFrame{SeaweedFSBucket: "backups"}); got != "backups" {
		t.Errorf("no choice means the backup bucket: %q", got)
	}
	if got := k3dPITRBucketOf(designFrame{}); got != "" {
		t.Errorf("nothing chosen anywhere is the node's default, named by neither: %q", got)
	}
}

func TestK3DReplValidationRejectsAMixedLink(t *testing.T) {
	doc := k3dReplDoc("async")
	doc.Nodes = append(doc.Nodes, designNode{ID: "ps1", Type: "ps", Label: "ps-01"})
	doc.Edges = []designEdge{{ID: "e1", Type: "async", From: edgeEnd{Node: "f1"}, To: edgeEnd{Node: "ps1"}}}
	if !hasIssue(k3dReplValidate(doc), "error", "another Kubernetes cluster") {
		t.Error("a Kubernetes cluster linked to a node outside it must be an error")
	}
}

func TestK3DReplValidationRejectsADuplicate(t *testing.T) {
	doc := k3dReplDoc("async")
	doc.Edges = append(doc.Edges, designEdge{ID: "e2", Type: "async", From: edgeEnd{Node: "f2"}, To: edgeEnd{Node: "f1"}})
	if !hasIssue(k3dReplValidate(doc), "error", "Duplicate replication link") {
		t.Error("a second link between the same pair must be an error")
	}
}

// The source's database pods each take an address out of the frame's own MetalLB block, on top of
// its proxy tier. Running the block dry leaves Services Pending, which reads as a hung deploy.
func TestK3DReplValidationFitsTheAddressBlock(t *testing.T) {
	doc := k3dReplDoc("async")
	if hasIssue(k3dReplValidate(doc), "error", "LoadBalancer addresses") {
		t.Fatalf("3 database pods + HAProxy's 2 fits in %d", k3dPoolSize)
	}
	if got := k3dProxyLBServices(doc.Frames[0]); got != 2 {
		t.Errorf("an exposed HAProxy publishes two Services, got %d", got)
	}
	doc.Frames[0].K3DProxy = "proxysql"
	doc.Frames[0].K3DExposeProxySQL = "loadbalancer"
	if got := k3dProxyLBServices(doc.Frames[0]); got != 1 {
		t.Errorf("an exposed ProxySQL publishes one Service, got %d", got)
	}
	doc.Frames[0].K3DProxy = "haproxy"
	doc.Frames[0].K3DExposeHAProxy = "clusterip"
	if got := k3dProxyLBServices(doc.Frames[0]); got != 0 {
		t.Errorf("a ClusterIP proxy takes no address, got %d", got)
	}
}

func TestK3DReplValidationSaysBidirIsOneWay(t *testing.T) {
	if !hasIssue(k3dReplValidate(k3dReplDoc("bidir")), "warning", "bidirectional is not available") {
		t.Error("a bidirectional link must say it will be set up one way")
	}
}

// ---------------------------------------------------------------- reading the source's addresses

// Only the per-pod Services are sources, and the query that produces this list is deliberately
// broad — everything the cluster owns — because the operator labels the per-pod Services
// `component: external-service`, not `pxc`, so narrowing by that label returns none of them.
// So the fixture carries what the broad query really returns: the headless `<cluster>-pxc` Service
// and its `-unready` twin, which have no address of their own, and the HAProxy tier, which HAS a
// LoadBalancer address and is emphatically not a replication source — sending a replica to the
// proxy would have it replicate from whichever node the proxy happened to pick.
func TestK3DReplHostsSkipTheHeadlessServices(t *testing.T) {
	js := `{"items":[
	  {"spec":{"type":"ClusterIP","clusterIP":"None","selector":{"app.kubernetes.io/component":"pxc"}},"status":{}},
	  {"spec":{"type":"ClusterIP","clusterIP":"None","selector":{"app.kubernetes.io/component":"pxc"}},"status":{}},
	  {"spec":{"type":"LoadBalancer","selector":{"app.kubernetes.io/component":"haproxy"}},
	   "status":{"loadBalancer":{"ingress":[{"ip":"172.20.255.246"}]}}},
	  {"spec":{"type":"LoadBalancer","selector":{"app.kubernetes.io/component":"haproxy"}},
	   "status":{"loadBalancer":{"ingress":[{"ip":"172.20.255.247"}]}}},
	  {"spec":{"type":"LoadBalancer","selector":{"statefulset.kubernetes.io/pod-name":"cluster1-pxc-2"}},
	   "status":{"loadBalancer":{"ingress":[{"ip":"172.20.255.250"}]}}},
	  {"spec":{"type":"LoadBalancer","selector":{"statefulset.kubernetes.io/pod-name":"cluster1-pxc-0"}},
	   "status":{"loadBalancer":{"ingress":[{"ip":"172.20.255.248"}]}}},
	  {"spec":{"type":"LoadBalancer","selector":{"statefulset.kubernetes.io/pod-name":"cluster1-pxc-1"}},
	   "status":{"loadBalancer":{"ingress":[{"ip":"172.20.255.249"}]}}}
	]}`
	got := k3dReplHostsFromServices(js)
	want := []string{"172.20.255.248", "172.20.255.249", "172.20.255.250"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v — the proxy and the headless pair must not be in it", got, want)
	}
	for i := range want {
		if got[i] != want[i] { // sorted, so a redeploy writes the same list
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// A per-pod Service that has not been given an address yet is not a source. Reporting it as one
// would put an empty host into sourcesList, which the operator accepts and MySQL cannot use.
func TestK3DReplHostsIgnoreAPendingService(t *testing.T) {
	js := `{"items":[{"spec":{"type":"LoadBalancer","selector":{"statefulset.kubernetes.io/pod-name":"cluster1-pxc-0"}},
	  "status":{"loadBalancer":{}}}]}`
	if got := k3dReplHostsFromServices(js); len(got) != 0 {
		t.Fatalf("a Pending Service is not an address, got %v", got)
	}
}

// A ClusterIP per-pod Service is reachable inside its own cluster and nowhere else, so it is not
// an answer for a replica in another one — better to report none and say why.
func TestK3DReplHostsRejectClusterIP(t *testing.T) {
	js := `{"items":[{"spec":{"type":"ClusterIP","clusterIP":"10.43.70.32",
	  "selector":{"statefulset.kubernetes.io/pod-name":"cluster1-pxc-0"}},"status":{}}]}`
	if got := k3dReplHostsFromServices(js); len(got) != 0 {
		t.Fatalf("a ClusterIP is not reachable from another cluster, got %v", got)
	}
}

// ---------------------------------------------------------------- cr.yaml

func TestCRSourceChannel(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr.yaml")
	if err != nil {
		t.Skipf("no cr.yaml fixture: %v", err)
	}
	out := crTransform(string(raw), crOptions{
		Name: "cluster1", Proxy: "haproxy", ExposePXC: "LoadBalancer",
		SourceChannel: "cluster1_to_cluster2",
	})
	if !strings.Contains(out, "    replicationChannels:\n    - name: cluster1_to_cluster2\n      isSource: true\n") {
		t.Fatalf("the source channel is not in the pxc section:\n%s", crSection(out, "pxc"))
	}
	// Exactly one live `replicationChannels` and one live `expose` in the section — the shipped
	// examples stay commented out, and each is marked so uncommenting one is an informed choice.
	if n := countLive(crSection(out, "pxc"), "replicationChannels:"); n != 1 {
		t.Errorf("want 1 uncommented replicationChannels, got %d", n)
	}
	if n := countLive(crSection(out, "pxc"), "expose:"); n != 1 {
		t.Errorf("want 1 uncommented expose, got %d", n)
	}
	if !strings.Contains(out, "already set at the top of this section") {
		t.Error("the shipped commented block should carry the duplicate-key note")
	}
}

// A cluster that is not a replication source gets no channel at all, and no note.
func TestCRNoChannelWhenNotASource(t *testing.T) {
	raw, err := os.ReadFile("testdata/cr.yaml")
	if err != nil {
		t.Skipf("no cr.yaml fixture: %v", err)
	}
	out := crTransform(string(raw), crOptions{Name: "cluster2", Proxy: "haproxy", ExposePXC: "ClusterIP"})
	if n := countLive(crSection(out, "pxc"), "replicationChannels:"); n != 0 {
		t.Fatalf("a replica's cr.yaml must ship no channel (it is patched in after the seed restore), got %d", n)
	}
}

// crSection returns the lines of one 2-space spec section.
func crSection(doc, name string) string {
	var b strings.Builder
	in := false
	for _, ln := range strings.Split(doc, "\n") {
		ind, commented, body := crLine(ln)
		if !commented && ind == 2 && strings.HasSuffix(body, ":") {
			in = body == name+":"
		}
		if in {
			b.WriteString(ln + "\n")
		}
	}
	return b.String()
}

// countLive counts uncommented occurrences of a key.
func countLive(doc, key string) int {
	n := 0
	for _, ln := range strings.Split(doc, "\n") {
		_, commented, body := crLine(ln)
		if !commented && body == key {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- the built-in template

// The template is the one design in the repository that exercises this end to end, so it has to
// resolve to exactly the link it claims — and pass its own validation, which is what a user sees
// before they press Deploy.
func TestK3DReplTemplateIsALink(t *testing.T) {
	var tpl StackTemplate
	for _, c := range builtinTemplates() {
		if c.ID == builtinPrefix+"k3d-pxc-replication" {
			tpl = c
		}
	}
	if tpl.ID == "" {
		t.Fatal("the two-cluster replication template is missing")
	}
	var doc designDoc
	if err := json.Unmarshal(tpl.Design, &doc); err != nil {
		t.Fatalf("design does not parse: %v", err)
	}
	links := k3dReplLinks(doc)
	if len(links) != 1 {
		t.Fatalf("want exactly one link, got %d", len(links))
	}
	if links[0].Src.Label != "cluster1" || links[0].Dst.Label != "cluster2" {
		t.Errorf("got %s → %s", links[0].Src.Label, links[0].Dst.Label)
	}
	if issues := k3dReplValidate(doc); hasError(issues) {
		t.Errorf("the template must validate without errors: %+v", issues)
	}
	// The template puts both clusters on one SeaweedFS node with a bucket each — the smallest
	// stack that shows the feature. Two nodes are allowed (k3dReplSeedSecret), but a template is
	// a starting point, and one store is one fewer container to explain.
	if links[0].Src.SeaweedFSNodeID == "" || links[0].Src.SeaweedFSNodeID != links[0].Dst.SeaweedFSNodeID {
		t.Error("the template is built around a single shared SeaweedFS node")
	}
	if links[0].Src.SeaweedFSBucket == links[0].Dst.SeaweedFSBucket {
		t.Error("each cluster should own its own bucket, so the seed is readable as the source's")
	}
}

// ---------------------------------------------------------------- waiting for the frames

// The deploy hands each frame to a goroutine and returns, so this phase starts while the clusters
// it is about to link are still being created. Resolving straight to a container instead of
// waiting failed the link within a second of pressing Deploy — which is exactly what the first
// live run of this code did, and this is the regression test for it.
func TestK3DReplWaitsForAFrameStillDeploying(t *testing.T) {
	app := newTestApp(t)
	u, err := app.store.CreateUser("admin", "x", RoleAdmin, StatusApproved)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	doc := k3dReplDoc("async")
	design, _ := json.Marshal(doc)
	st, err := app.store.CreateStack("k8srepl", u.ID, "2h", nil, design)
	if err != nil {
		t.Fatalf("create stack: %v", err)
	}
	cfg, _ := json.Marshal(k3dConfig{Operator: "pxc", ClusterName: "cluster1", Namespace: "default"})
	// The state provisionK3DFrame leaves a member in for most of a deploy.
	app.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: "n1", State: DeployProvisioning, Config: cfg})

	// It must still be waiting a moment later, not have failed.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := app.k3dReplWaitFrame(ctx, st, doc, doc.Frames[0])
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("returned while the frame was still deploying: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	// ...and it resolves as soon as the frame reports running.
	app.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: "n1", ContainerID: "c1", State: DeployRunning, Config: cfg})
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a running frame must resolve: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("never noticed the frame had come up")
	}
}

// A frame that fails to deploy must end the wait, not hold the phase open until the deploy
// timeout — the link cannot be made and the log should say so while anyone is still watching.
func TestK3DReplGivesUpOnAFailedFrame(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("admin", "x", RoleAdmin, StatusApproved)
	doc := k3dReplDoc("async")
	design, _ := json.Marshal(doc)
	st, _ := app.store.CreateStack("k8srepl", u.ID, "2h", nil, design)
	app.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: "n1", State: DeployError})

	if _, _, err := app.k3dReplWaitFrame(context.Background(), st, doc, doc.Frames[0]); err == nil {
		t.Fatal("a failed frame must end the wait with an error")
	}
}

// ---------------------------------------------------------------- the restore manifest

// A restore across clusters spells out the S3 location instead of naming a backup object, and the
// block it spells out is the source backup's own status. What matters is what survives the copy
// and what deliberately does not.
func TestK3DReplRestoreManifest(t *testing.T) {
	src := k3dReplBackupSource{
		Destination: "s3://backup1/cluster1-2026-09-09-01:30:05-full",
		VerifyTLS:   new(bool), // false
		S3: map[string]any{
			"bucket": "backup1", "region": "us-east-1",
			"endpointUrl":       "http://seaweedfs-01.example.net:8333",
			"forcePathStyle":    true,
			"prefix":            "",
			"credentialsSecret": "cluster1-backup-s3",
		},
	}
	raw, err := k3dReplRestoreManifest(src, "dbcanvas-seed-restore-cluster1-to-cluster2", "cluster2", "cluster2-backup-s3")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	var got struct {
		Kind string `json:"kind"`
		Spec struct {
			PXCCluster   string `json:"pxcCluster"`
			BackupSource struct {
				Destination string         `json:"destination"`
				VerifyTLS   *bool          `json:"verifyTLS"`
				S3          map[string]any `json:"s3"`
			} `json:"backupSource"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("does not parse: %v", err)
	}
	bs := got.Spec.BackupSource
	if got.Kind != "PerconaXtraDBClusterRestore" || got.Spec.PXCCluster != "cluster2" {
		t.Errorf("wrong kind/cluster: %s / %s", got.Kind, got.Spec.PXCCluster)
	}
	if bs.Destination != src.Destination {
		t.Errorf("destination = %q", bs.Destination)
	}
	// The replica's own credentials, opening the SOURCE's bucket.
	if bs.S3["credentialsSecret"] != "cluster2-backup-s3" {
		t.Errorf("credentialsSecret = %v, want the replica's own", bs.S3["credentialsSecret"])
	}
	if bs.S3["bucket"] != "backup1" {
		t.Errorf("bucket = %v, want the source's", bs.S3["bucket"])
	}
	// SeaweedFS does not do virtual-host bucket addressing, so this one has to survive the copy.
	if bs.S3["forcePathStyle"] != true {
		t.Errorf("forcePathStyle did not survive: %v", bs.S3["forcePathStyle"])
	}
	if bs.VerifyTLS == nil || *bs.VerifyTLS {
		t.Errorf("verifyTLS = %v, want false as the source recorded it", bs.VerifyTLS)
	}
	// A storageName would resolve against the REPLICA's own storage and restore the wrong bucket.
	if strings.Contains(string(raw), "storageName") {
		t.Error("storageName must not be carried across clusters")
	}
	if strings.Contains(string(raw), "sslSecretName") || strings.Contains(string(raw), "sslInternalSecretName") {
		t.Error("the source cluster's ssl secret names do not exist in the replica")
	}
}

// A status with no S3 block is the shape that produced "nil s3 backup status storage" and a restore
// that never created a Job — better to fail with something that names the cause.
func TestK3DReplRestoreManifestRefusesAnEmptySource(t *testing.T) {
	if _, err := k3dReplRestoreManifest(k3dReplBackupSource{Destination: "s3://b/x"}, "n", "cluster2", "s"); err == nil {
		t.Error("a source with no s3 block must be refused")
	}
	if _, err := k3dReplRestoreManifest(k3dReplBackupSource{S3: map[string]any{"bucket": "b"}}, "n", "cluster2", "s"); err == nil {
		t.Error("a source with no destination must be refused")
	}
}

// A Kubernetes object name is an RFC 1123 subdomain and a MySQL channel name is not — the channel
// this feature generates uses underscores, and naming the seed backup after it had the API server
// reject the object outright on the second live run. This is that regression.
func TestK3DReplObjectNameIsRFC1123(t *testing.T) {
	legal := func(name string) bool {
		if name == "" || len(name) > 63 {
			return false
		}
		for i, r := range name {
			alnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
			if alnum {
				continue
			}
			if r == '-' && i != 0 && i != len(name)-1 {
				continue
			}
			return false
		}
		return true
	}
	cases := [][2]string{
		{"cluster1", "cluster2"},
		{"East_1", "WEST.2"}, // the alphabets that differ
		{"  ", "??"},         // nothing usable at all
		{strings.Repeat("a", 40), strings.Repeat("b", 40)}, // longer than the cap
	}
	seen := map[string]bool{}
	for _, c := range cases {
		got := k3dReplObjectName(k3dReplSeedName, designFrame{Label: c[0]}, designFrame{Label: c[1]})
		if !legal(got) {
			t.Errorf("%q → %q is not a legal object name", c, got)
		}
		seen[got] = true
	}
	// ...and the channel name, which is the thing that broke, must not leak into it.
	src, dst := designFrame{Label: "cluster1"}, designFrame{Label: "cluster2"}
	if got := k3dReplObjectName(k3dReplSeedName, src, dst); strings.Contains(got, "_") {
		t.Errorf("%q carries a channel-style underscore", got)
	}
	// Two links in one stack must not collide on a name.
	other := k3dReplObjectName(k3dReplSeedName, designFrame{Label: "cluster3"}, designFrame{Label: "cluster4"})
	if other == k3dReplObjectName(k3dReplSeedName, src, dst) {
		t.Error("two different links produced the same object name")
	}
}

// ---------------------------------------------------------------- pruning

// Taking the LAST link off the canvas leaves no links and a cluster still replicating. An early
// return on "no links" would skip the prune entirely and leave the channel running for good, which
// is the one case this reconcile phase most obviously owes an answer to.
func TestK3DReplPruneRunsWithNoLinksLeft(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("admin", "x", RoleAdmin, StatusApproved)
	doc := k3dReplDoc("async")
	doc.Edges = nil // the link has been removed from the canvas
	design, _ := json.Marshal(doc)
	st, _ := app.store.CreateStack("k8srepl", u.ID, "2h", nil, design)

	// cluster2 still records what the last deploy made it.
	cfg, _ := json.Marshal(k3dConfig{
		Operator: "pxc", ClusterName: "cluster2", Namespace: "default",
		ReplRole: "replica", ReplChannel: "cluster1_to_cluster2", ReplSeededFrom: "s3://backup1/x",
	})
	app.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: "n2", ContainerID: "c2", State: DeployRunning, Config: cfg})

	if got := app.k3dReplRecordedRole(st, doc, doc.Frames[1]); got != "replica" {
		t.Fatalf("the recorded role must be readable without the frame running: %q", got)
	}
	// With no links at all there is still a frame to consider, so the phase must not bail out.
	if len(k3dReplLinks(doc)) != 0 {
		t.Fatal("the fixture should have no links")
	}
	// The kubectl call cannot run here, so what is asserted is the decision: the frame is examined
	// (a recorded replica, not wanted by any link) rather than skipped.
	wanted := map[string]bool{}
	considered := 0
	for _, f := range doc.Frames {
		if k3dReplCapable(f) && !wanted[f.ID] && app.k3dReplRecordedRole(st, doc, f) == "replica" {
			considered++
		}
	}
	if considered != 1 {
		t.Fatalf("want cluster2 considered for pruning, got %d frames", considered)
	}
}

// A cluster that was never a replica must not be waited for — the wait is the expensive part, and
// there is nothing to stop on a source or an unlinked cluster.
func TestK3DReplPruneIgnoresANonReplica(t *testing.T) {
	app := newTestApp(t)
	u, _ := app.store.CreateUser("admin", "x", RoleAdmin, StatusApproved)
	doc := k3dReplDoc("async")
	doc.Edges = nil
	design, _ := json.Marshal(doc)
	st, _ := app.store.CreateStack("k8srepl", u.ID, "2h", nil, design)
	cfg, _ := json.Marshal(k3dConfig{Operator: "pxc", ClusterName: "cluster1", ReplRole: "source"})
	app.store.UpsertDeployment(Deployment{StackID: st.ID, NodeID: "n1", State: DeployRunning, Config: cfg})

	if got := app.k3dReplRecordedRole(st, doc, doc.Frames[0]); got == "replica" {
		t.Error("a source must not be pruned as a replica")
	}
	if got := app.k3dReplRecordedRole(st, doc, doc.Frames[1]); got != "" {
		t.Errorf("a frame with no deployment has no role, got %q", got)
	}
}

// ---------------------------------------------------------------- the seed backup is fresh

// recordingEngine records the commands run against it and answers them, so an ordering that only
// exists as a sequence of kubectl calls can be asserted. The Engine interface is embedded rather
// than implemented: every method this test does not need stays nil, so a call to one panics by name
// instead of silently doing nothing.
type recordingEngine struct {
	Engine
	calls   []string
	replies map[string]string // substring of the command → stdout to answer with
}

func (r *recordingEngine) Exec(ctx context.Context, id string, cmd []string, env []string) (ExecResult, error) {
	return r.record(strings.Join(cmd, " ")), nil
}

// kubectlApply pipes the manifest on stdin, so what it created is only visible here — the command
// line is just "kubectl apply -f -". The manifest is appended to the recorded line so an assertion
// can name the kind that was applied.
func (r *recordingEngine) ExecInput(ctx context.Context, id, user string, cmd, env []string, stdin []byte) (ExecResult, error) {
	return r.record(strings.Join(cmd, " ") + " <<< " + string(stdin)), nil
}

func (r *recordingEngine) record(line string) ExecResult {
	r.calls = append(r.calls, line)
	for frag, out := range r.replies {
		if strings.Contains(line, frag) {
			return ExecResult{Stdout: out}
		}
	}
	return ExecResult{}
}

// The seed backup object's name is deterministic per link, so a re-seed applies over the Succeeded
// object left by the previous one, changes nothing, and is handed THAT backup's destination —
// restoring the replica to where the source was an hour ago while reporting a fresh seed. Observed
// live: a second Re-seed came back with the first seed's timestamp. So the old object must be
// deleted before the new one is created, and this asserts that order.
func TestK3DReplSeedDeletesThePreviousBackupFirst(t *testing.T) {
	app := newTestApp(t)
	doc := k3dReplDoc("async")
	link := k3dReplLinks(doc)[0]
	name := k3dReplObjectName(k3dReplSeedName, link.Src, link.Dst)

	rec := &recordingEngine{replies: map[string]string{
		// The backup is Succeeded the first time it is read, so the seed does not sit in its poll.
		"get pxc-backup": `{"status":{"state":"Succeeded","destination":"s3://backup1/new-full",
		  "verifyTLS":false,"s3":{"bucket":"backup1","endpointUrl":"http://sw:8333","region":"us-east-1"}}}`,
		"get pxc-restore": `{"status":{"state":"Succeeded"}}`,
	}}
	ctx := withEngine(context.Background(), rec)
	srcCfg := k3dConfig{ClusterName: "cluster1", Namespace: "default", BackupRepo: "SeaweedFS S3"}
	dstCfg := k3dConfig{ClusterName: "cluster2", Namespace: "default", BackupRepo: "SeaweedFS S3"}

	dest, err := app.k3dReplSeed(ctx, Stack{ID: 1}, doc, link, "src", srcCfg, "dst", dstCfg, func(string) {})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if dest != "s3://backup1/new-full" {
		t.Errorf("destination = %q, want the one the backup reported", dest)
	}

	del, create := -1, -1
	for i, c := range rec.calls {
		if del < 0 && strings.Contains(c, "delete pxc-backup "+name) {
			del = i
		}
		if create < 0 && strings.Contains(c, "apply") && strings.Contains(c, "PerconaXtraDBClusterBackup") {
			create = i
		}
	}
	if del < 0 {
		t.Fatalf("the previous seed backup is never deleted; calls: %v", rec.calls)
	}
	if create < 0 {
		t.Fatalf("no backup is created; calls: %v", rec.calls)
	}
	if del > create {
		t.Errorf("the delete must come first, got delete at %d and create at %d", del, create)
	}
}

// A replication pair is allowed one object store each, which is how two sites really are built.
// The restore then reads the backup out of the SOURCE's bucket, at the source's endpoint, with the
// source's keys — and a Secret is named rather than embedded, so those keys have to be copied into
// the replica's cluster under a name that resolves there. Reusing the replica's own backup secret
// (which is all that was needed while both clusters had to share one store) fails with a 403 that
// reads like a missing backup.
func TestK3DReplSeedSecretPerStore(t *testing.T) {
	app := newTestApp(t)
	doc := k3dReplDoc("async")
	link := k3dReplLinks(doc)[0]
	dstCfg := k3dConfig{ClusterName: "cluster2", Namespace: "default"}

	// One shared store: the replica's own secret already holds the right keys, so nothing is
	// created — and nothing has to be resolvable, which is what keeps the common path cheap.
	rec := &recordingEngine{}
	name, err := app.k3dReplSeedSecret(withEngine(context.Background(), rec), Stack{ID: 1}, link, "dst", dstCfg, func(string) {})
	if err != nil {
		t.Fatalf("shared store: %v", err)
	}
	if name != "cluster2-backup-s3" {
		t.Errorf("secret = %q, want the replica's own backup secret", name)
	}
	for _, c := range rec.calls {
		if strings.Contains(c, "create secret") {
			t.Errorf("nothing should be created for a shared store, got %q", c)
		}
	}

	// Two stores, and the source's is not deployed in this stack: the seed fails with a reason,
	// rather than quietly naming a secret that opens the wrong store.
	twoStores := k3dReplDoc("async")
	twoStores.Frames[1].SeaweedFSNodeID = "sw2"
	link2 := k3dReplLinks(twoStores)[0]
	if link2.Src.SeaweedFSNodeID == link2.Dst.SeaweedFSNodeID {
		t.Fatal("the fixture must have two different stores for this half of the test")
	}
	if _, err := app.k3dReplSeedSecret(withEngine(context.Background(), rec), Stack{ID: 1}, link2, "dst", dstCfg, func(string) {}); err == nil {
		t.Error("an unresolvable source store must fail the seed, not fall back to the wrong keys")
	}
}
