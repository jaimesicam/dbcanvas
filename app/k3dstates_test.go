package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// k3dstates_test.go — the rules behind the Kubernetes States canvas.
//
// What is worth pinning here is not "does it parse JSON" but the judgement: which
// objects are red, which are merely amber, and which are neither because finishing is
// what they were for. A canvas that calls a completed backup pod a failure is a canvas
// that teaches people to ignore red, so every one of those calls has a test.

// prop returns one property of an object, and whether it is there at all.
func prop(o k3dStateObj, key string) (k3dProp, bool) {
	for _, p := range o.Props {
		if p.Key == key {
			return p, true
		}
	}
	return k3dProp{}, false
}

func mustOne(t *testing.T, docs ...[]byte) k3dStateObj {
	t.Helper()
	objs := parseK3DStates(docs...)
	if len(objs) != 1 {
		t.Fatalf("got %d objects, want 1: %+v", len(objs), objs)
	}
	return objs[0]
}

// podJSON builds a one-pod list document from a status literal, so each case below is the
// status and nothing else.
func podJSON(name, status string) []byte {
	return []byte(`{"items":[{"kind":"Pod","metadata":{"name":"` + name + `","namespace":"pg","uid":"u-` + name + `"},` +
		`"spec":{"nodeName":"k3d-server-0"},"status":` + status + `}]}`)
}

func TestPodRunningAndReadyIsOK(t *testing.T) {
	o := mustOne(t, podJSON("db-0", `{"phase":"Running","podIP":"10.42.0.9","containerStatuses":[
		{"name":"database","ready":true,"restartCount":0,"state":{"running":{"startedAt":"2026-09-12T10:00:00Z"}}},
		{"name":"sidecar","ready":true,"restartCount":0,"state":{"running":{"startedAt":"2026-09-12T10:00:00Z"}}}]}`))
	if o.Tone != toneOK {
		t.Errorf("tone = %q, want ok (%+v)", o.Tone, o.Props)
	}
	if o.Summary != "2/2 Running" {
		t.Errorf("summary = %q", o.Summary)
	}
	if p, ok := prop(o, "Restarts"); ok {
		t.Errorf("a pod that has never restarted should have no Restarts row, got %+v", p)
	}
	if p, _ := prop(o, "Node"); p.Value != "k3d-server-0" {
		t.Errorf("Node = %q — the row comes from the spec, not the status", p.Value)
	}
}

// The case the whole feature exists for: a container that is not coming up on its own.
// The pod's phase is still "Running", so the phase alone would paint this green.
func TestPodWithCrashLoopIsBadAndNamesTheContainer(t *testing.T) {
	o := mustOne(t, podJSON("db-1", `{"phase":"Running","containerStatuses":[
		{"name":"database","ready":false,"restartCount":12,"state":{"waiting":{"reason":"CrashLoopBackOff","message":"back-off 5m0s"}}},
		{"name":"sidecar","ready":true,"restartCount":0,"state":{"running":{}}}]}`))
	if o.Tone != toneBad {
		t.Fatalf("tone = %q, want bad", o.Tone)
	}
	p, ok := prop(o, "database")
	if !ok || p.Value != "CrashLoopBackOff" || p.Tone != toneBad {
		t.Errorf("container row = %+v (ok=%v), want a bad CrashLoopBackOff row", p, ok)
	}
	// Restarts are amber, not red: the number is history, the container row is now.
	if p, _ := prop(o, "Restarts"); p.Value != "12" || p.Tone != toneWarn {
		t.Errorf("Restarts = %+v, want 12 as a warning", p)
	}
	if p, _ := prop(o, "Phase"); p.Tone != toneWarn {
		t.Errorf("Phase tone = %q — Running with an unready container is amber on its own", p.Tone)
	}
}

// ContainerCreating is every pod's first second. Amber, never red.
func TestPodStartingIsAWarningNotAFailure(t *testing.T) {
	o := mustOne(t, podJSON("db-2", `{"phase":"Pending","containerStatuses":[
		{"name":"database","ready":false,"restartCount":0,"state":{"waiting":{"reason":"ContainerCreating"}}}]}`))
	if o.Tone != toneWarn {
		t.Errorf("tone = %q, want warn (%+v)", o.Tone, o.Props)
	}
}

// A backup pod that ran and exited is the single most common object on a Percona lab
// cluster, and calling it red would make the canvas useless.
func TestCompletedPodIsDoneNotBad(t *testing.T) {
	o := mustOne(t, podJSON("backup-1", `{"phase":"Succeeded","containerStatuses":[
		{"name":"pgbackrest","ready":false,"restartCount":0,"state":{"terminated":{"reason":"Completed","exitCode":0}}}]}`))
	if o.Tone != toneDone {
		t.Fatalf("tone = %q, want done (%+v)", o.Tone, o.Props)
	}
	if !strings.Contains(o.Summary, "Completed") {
		t.Errorf("summary = %q, want it to say Completed", o.Summary)
	}
}

// The same shape with a non-zero exit is the opposite answer.
func TestPodTerminatedWithErrorIsBad(t *testing.T) {
	o := mustOne(t, podJSON("backup-2", `{"phase":"Failed","containerStatuses":[
		{"name":"pgbackrest","ready":false,"restartCount":0,"state":{"terminated":{"reason":"Error","exitCode":1}}}]}`))
	if o.Tone != toneBad {
		t.Fatalf("tone = %q, want bad", o.Tone)
	}
	if p, _ := prop(o, "pgbackrest"); !strings.Contains(p.Value, "exit 1") {
		t.Errorf("container row = %q, want the exit code in it", p.Value)
	}
}

// A pod nothing will schedule says so in a condition, and that sentence is the answer.
func TestUnschedulablePodExplainsItself(t *testing.T) {
	o := mustOne(t, podJSON("db-3", `{"phase":"Pending","conditions":[
		{"type":"PodScheduled","status":"False","reason":"Unschedulable","message":"0/1 nodes are available: 1 Insufficient memory."}]}`))
	p, ok := prop(o, "Unschedulable")
	if !ok || p.Tone != toneBad || !strings.Contains(p.Value, "Insufficient memory") {
		t.Errorf("Unschedulable row = %+v (ok=%v)", p, ok)
	}
}

// Deletion outranks everything: a Running pod on its way out must not read as healthy.
func TestTerminatingPodIsNotGreen(t *testing.T) {
	doc := []byte(`{"items":[{"kind":"Pod","metadata":{"name":"db-4","namespace":"pg","uid":"u4",
		"deletionTimestamp":"2026-09-12T11:00:00Z"},"status":{"phase":"Running","containerStatuses":[
		{"name":"database","ready":true,"restartCount":0,"state":{"running":{}}}]}}]}`)
	o := mustOne(t, doc)
	if o.Tone != toneWarn || o.Summary != "Terminating" {
		t.Errorf("tone=%q summary=%q, want a warning that says Terminating", o.Tone, o.Summary)
	}
	if o.Props[0].Key != "Terminating since" {
		t.Errorf("the terminating row should lead: %+v", o.Props)
	}
}

func TestWorkloadReplicaArithmetic(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		tone    string
		summary string
	}{
		{"all ready", `"spec":{"replicas":3},"status":{"replicas":3,"readyReplicas":3,"availableReplicas":3}`, toneOK, "3/3 ready"},
		{"one short", `"spec":{"replicas":3},"status":{"replicas":3,"readyReplicas":2,"availableReplicas":2}`, toneWarn, "2/3 ready"},
		{"none ready", `"spec":{"replicas":3},"status":{"replicas":3,"readyReplicas":0}`, toneBad, "0/3 ready"},
		// Scaled to zero is a thing somebody did on purpose, not an outage.
		{"scaled to zero", `"spec":{"replicas":0},"status":{"replicas":0}`, toneDone, "scaled to 0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := mustOne(t, []byte(`{"items":[{"kind":"StatefulSet","metadata":{"name":"db","namespace":"pg","uid":"s1"},`+c.body+`}]}`))
			if o.Tone != c.tone || o.Summary != c.summary {
				t.Errorf("tone=%q summary=%q, want %q / %q", o.Tone, o.Summary, c.tone, c.summary)
			}
		})
	}
}

func TestClaimPhases(t *testing.T) {
	for phase, want := range map[string]string{"Bound": toneOK, "Pending": toneWarn, "Lost": toneBad} {
		o := mustOne(t, []byte(`{"items":[{"kind":"PersistentVolumeClaim","metadata":{"name":"data","namespace":"pg","uid":"p1"},
			"spec":{"volumeName":"pvc-1"},"status":{"phase":"`+phase+`","capacity":{"storage":"1Gi"}}}]}`))
		if o.Tone != want {
			t.Errorf("%s claim tone = %q, want %q", phase, o.Tone, want)
		}
		if p, _ := prop(o, "Capacity"); phase == "Bound" && p.Value != "1Gi" {
			t.Errorf("Capacity = %q", p.Value)
		}
	}
}

// These clusters run MetalLB, so a LoadBalancer with no address is a real symptom.
func TestLoadBalancerWithoutAnAddressIsAWarning(t *testing.T) {
	pending := mustOne(t, []byte(`{"items":[{"kind":"Service","metadata":{"name":"ha","namespace":"pg","uid":"v1"},
		"spec":{"type":"LoadBalancer","clusterIP":"10.43.0.5","ports":[{"port":5432}]},"status":{"loadBalancer":{}}}]}`))
	if p, _ := prop(pending, "External IP"); p.Value != "pending" || p.Tone != toneWarn {
		t.Errorf("pending LoadBalancer row = %+v", p)
	}
	got := mustOne(t, []byte(`{"items":[{"kind":"Service","metadata":{"name":"ha","namespace":"pg","uid":"v1"},
		"spec":{"type":"LoadBalancer","clusterIP":"10.43.0.5","ports":[{"port":5432}]},
		"status":{"loadBalancer":{"ingress":[{"ip":"172.20.255.246"}]}}}]}`))
	if got.Summary != "172.20.255.246" || got.Tone != toneOK {
		t.Errorf("addressed LoadBalancer: tone=%q summary=%q", got.Tone, got.Summary)
	}
}

func TestNodeReadyAndPressure(t *testing.T) {
	o := mustOne(t, []byte(`{"items":[{"kind":"Node","metadata":{"name":"k3d-s-0","uid":"n1"},"spec":{"unschedulable":true},
		"status":{"nodeInfo":{"kubeletVersion":"v1.33.13+k3s1"},"conditions":[
		{"type":"MemoryPressure","status":"False"},{"type":"DiskPressure","status":"True"},
		{"type":"Ready","status":"False","reason":"KubeletNotReady"}]}}]}`))
	if o.Tone != toneBad || o.Summary != "NotReady, cordoned" {
		t.Errorf("tone=%q summary=%q", o.Tone, o.Summary)
	}
	if p, _ := prop(o, "DiskPressure"); p.Tone != toneBad {
		t.Errorf("DiskPressure row = %+v, want bad", p)
	}
	if _, ok := prop(o, "MemoryPressure"); ok {
		t.Error("a False pressure condition is normal and must not take a row")
	}
}

// Every operator spells its state differently, so the custom-resource reader is a
// vocabulary over whatever status it finds. These three shapes are the real ones.
func TestCustomResourceStates(t *testing.T) {
	pg := mustOne(t, []byte(`{"items":[{"kind":"PerconaPGCluster","metadata":{"name":"k3d-00","namespace":"pg","uid":"c1"},
		"status":{"state":"ready","postgres":{"ready":3,"size":3},"pgbouncer":{"ready":3,"size":3},"observedGeneration":4}}]}`))
	if pg.Tone != toneOK || pg.Summary != "ready" {
		t.Errorf("pg cluster: tone=%q summary=%q", pg.Tone, pg.Summary)
	}
	if p, ok := prop(pg, "postgres.ready"); !ok || p.Value != "3" {
		t.Errorf("nested status field = %+v (ok=%v) — a failover moves this number", p, ok)
	}
	if _, ok := prop(pg, "observedGeneration"); ok {
		t.Error("observedGeneration is bookkeeping, not state")
	}

	pxc := mustOne(t, []byte(`{"items":[{"kind":"PerconaXtraDBCluster","metadata":{"name":"c","namespace":"pxc","uid":"c2"},
		"status":{"state":"error","message":"cannot reconcile"}}]}`))
	if pxc.Tone != toneBad {
		t.Errorf("an operator saying error must be red, got %q", pxc.Tone)
	}

	cnpg := mustOne(t, []byte(`{"items":[{"kind":"Cluster","metadata":{"name":"c","namespace":"cnpg","uid":"c3"},
		"status":{"phase":"Setting up primary","conditions":[{"type":"Ready","status":"False","reason":"WaitingForPrimary"}]}}]}`))
	if cnpg.Tone != toneBad {
		t.Errorf("a Ready=False condition is the object saying it is not working, got %q", cnpg.Tone)
	}
	if p, _ := prop(cnpg, "Ready"); !strings.Contains(p.Value, "WaitingForPrimary") {
		t.Errorf("condition row = %+v", p)
	}
}

// A kind with no rule and no recognisable status still has to produce a card.
func TestUnknownKindStillShowsSomething(t *testing.T) {
	o := mustOne(t, []byte(`{"items":[{"kind":"Widget","metadata":{"name":"w","namespace":"x","uid":"w1"},"status":{"shards":7}}]}`))
	if o.Kind != "Widget" || o.Summary == "" {
		t.Errorf("unknown kind produced %+v", o)
	}
	if p, _ := prop(o, "shards"); p.Value != "7" {
		t.Errorf("scalar status field = %+v", p)
	}
}

// Events explain a red card; they never cause one. A pod that is Running and ready with a
// probe warning against it is still green, and the warning is still visible.
func TestWarningEventsAttachWithoutChangingTheTone(t *testing.T) {
	objs := parseK3DStates(podJSON("db-5", `{"phase":"Running","containerStatuses":[
		{"name":"database","ready":true,"restartCount":0,"state":{"running":{}}}]}`))
	events := []byte(`{"items":[
		{"type":"Warning","reason":"Unhealthy","message":"Liveness probe failed","count":3,"lastTimestamp":"2026-09-12T11:00:00Z",
		 "involvedObject":{"uid":"u-db-5","kind":"Pod","name":"db-5","namespace":"pg"}},
		{"type":"Normal","reason":"Pulled","message":"Container image already present","lastTimestamp":"2026-09-12T11:01:00Z",
		 "involvedObject":{"uid":"u-db-5","kind":"Pod","name":"db-5","namespace":"pg"}}]}`)
	k3dAttachEvents(objs, events, 5)
	o := objs[0]
	if o.Tone != toneOK {
		t.Errorf("tone = %q — an event must not repaint a healthy object", o.Tone)
	}
	if len(o.Events) != 1 || o.Events[0].Reason != "Unhealthy" {
		t.Fatalf("events = %+v, want only the Warning", o.Events)
	}
	if o.Events[0].Count != 3 {
		t.Errorf("count = %d", o.Events[0].Count)
	}
}

// Events are matched by UID, and by address when the event carries no UID.
func TestEventsMatchByNameWhenThereIsNoUID(t *testing.T) {
	objs := parseK3DStates([]byte(`{"items":[{"kind":"Pod","metadata":{"name":"db-6","namespace":"pg"},"status":{"phase":"Running"}}]}`))
	k3dAttachEvents(objs, []byte(`{"items":[{"type":"Warning","reason":"BackOff","message":"Back-off pulling image",
		"involvedObject":{"kind":"Pod","name":"db-6","namespace":"pg"}}]}`), 5)
	if len(objs[0].Events) != 1 {
		t.Errorf("events = %+v", objs[0].Events)
	}
}

func TestEventsAreCappedNewestFirst(t *testing.T) {
	objs := parseK3DStates(podJSON("db-7", `{"phase":"Running"}`))
	var items []string
	for _, ts := range []string{"11:00", "11:05", "11:02", "11:09", "11:07", "11:01"} {
		items = append(items, `{"type":"Warning","reason":"R`+ts+`","message":"m","lastTimestamp":"2026-09-12T`+ts+`:00Z",
			"involvedObject":{"uid":"u-db-7"}}`)
	}
	k3dAttachEvents(objs, []byte(`{"items":[`+strings.Join(items, ",")+`]}`), 3)
	got := objs[0].Events
	if len(got) != 3 || got[0].Reason != "R11:09" || got[2].Reason != "R11:05" {
		t.Errorf("events = %+v, want the three newest", got)
	}
}

// The canvas lays cards out in the order the sample arrives, so the order has to be the
// same every sample or every card moves under the pointer.
func TestStatesSortIsStableAndKindFirst(t *testing.T) {
	doc := []byte(`{"items":[
		{"kind":"Service","metadata":{"name":"svc","namespace":"pg","uid":"1"},"status":{}},
		{"kind":"PerconaPGCluster","metadata":{"name":"cr","namespace":"pg","uid":"2"},"status":{"state":"ready"}},
		{"kind":"Pod","metadata":{"name":"b","namespace":"pg","uid":"3"},"status":{"phase":"Running"}},
		{"kind":"Pod","metadata":{"name":"a","namespace":"kube-system","uid":"4"},"status":{"phase":"Running"}},
		{"kind":"Node","metadata":{"name":"n","uid":"5"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`)
	var got []string
	for _, o := range parseK3DStates(doc) {
		got = append(got, o.Kind+"/"+o.Name)
	}
	want := []string{"Node/n", "Pod/a", "Pod/b", "Service/svc", "PerconaPGCluster/cr"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// A missing UID must still produce a stable identity, because the canvas keys its cards
// (and its tombstones) on it.
func TestObjectsWithoutAUIDGetAStableKey(t *testing.T) {
	o := mustOne(t, []byte(`{"items":[{"kind":"Pod","metadata":{"name":"db","namespace":"pg"},"status":{"phase":"Running"}}]}`))
	if o.UID != "pod/pg/db" {
		t.Errorf("uid = %q", o.UID)
	}
}

// One bad document among several must not lose the good ones — the custom-resource call
// is allowed to fail on a cluster with no operator.
func TestABadDocumentDoesNotLoseTheOthers(t *testing.T) {
	objs := parseK3DStates(podJSON("db-8", `{"phase":"Running"}`), []byte("error: the server doesn't have a resource type"))
	if len(objs) != 1 {
		t.Errorf("got %d objects, want the pod to survive", len(objs))
	}
}

// The response is what the canvas is built from, so it has to be JSON the browser can
// read — in particular Props must never be null, or every card renders empty.
func TestStateObjectsMarshalWithAPropsArray(t *testing.T) {
	objs := parseK3DStates([]byte(`{"items":[{"kind":"Widget","metadata":{"name":"w","namespace":"x","uid":"w1"}}]}`))
	b, err := json.Marshal(objs[0])
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	json.Unmarshal(b, &back)
	if _, ok := back["props"].([]any); !ok {
		t.Errorf("props marshalled as %T, want an array", back["props"])
	}
}

// ---------------------------------------------------------------- logs and YAML

// The command these build is argv, not a shell line, so there is no quoting to get wrong —
// and none to save us either: a "name" of `--all-containers` is a flag kubectl would honour.
// Every value is therefore checked against what a Kubernetes name can be.
func TestLogArgsRejectAnythingThatIsNotAName(t *testing.T) {
	for _, bad := range []struct{ ns, pod, container string }{
		{"--all-namespaces", "db-0", ""},
		{"pg", "-l app=db", ""},
		{"pg", "db-0", "--previous"},
		{"pg", "", ""},
		{"", "db-0", ""},
		{"pg", "db-0; rm -rf /", ""},
		{"pg", "DB-0", ""}, // uppercase is not a Kubernetes name
	} {
		if _, _, err := k3dLogArgs(bad.ns, bad.pod, bad.container, "200", false); err == nil {
			t.Errorf("k3dLogArgs(%q, %q, %q) was accepted", bad.ns, bad.pod, bad.container)
		}
	}
}

func TestLogArgs(t *testing.T) {
	got, n, err := k3dLogArgs("pg", "db-0", "database", "500", true)
	if err != nil {
		t.Fatal(err)
	}
	want := "-n pg logs db-0 --tail 500 --timestamps -c database --previous"
	if strings.Join(got, " ") != want {
		t.Errorf("args = %q\n want %q", strings.Join(got, " "), want)
	}
	// The tail comes back as a number as well as an argument. It is in the response, and
	// reading it out of the argv again is how a package-level `tail` func ended up in a
	// response map — encoding it failed, and every log read became an empty HTTP 200.
	if n != 500 {
		t.Errorf("tail = %d, want 500", n)
	}
	// No container: kubectl picks, which is right for a single-container pod.
	got, _, _ = k3dLogArgs("pg", "db-0", "", "", false)
	if strings.Contains(strings.Join(got, " "), "-c") {
		t.Errorf("args = %v, want no -c", got)
	}
	// The tail is bounded and never absurd: a pane is a tail, not an archive.
	for in, want := range map[string]string{"": "200", "0": "200", "-5": "200", "nonsense": "200", "99999": "5000", "1000": "1000"} {
		got, n, _ := k3dLogArgs("pg", "db-0", "", in, false)
		if got[5] != want || strconv.Itoa(n) != want {
			t.Errorf("tail %q → argv %q / n %d, want %q", in, got[5], n, want)
		}
	}
}

func TestManifestArgs(t *testing.T) {
	got, err := k3dManifestArgs("Pod", "pg", "db-0")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "-n pg get Pod db-0 -o yaml" {
		t.Errorf("args = %v", got)
	}
	// Cluster-scoped: the flag is left off entirely. `-n ""` is not the same thing and
	// fails, which would make every Node's YAML pane an error box.
	got, err = k3dManifestArgs("Node", "", "k3d-server-0")
	if err != nil || strings.Join(got, " ") != "get Node k3d-server-0 -o yaml" {
		t.Errorf("cluster-scoped args = %v (%v)", got, err)
	}
	// A kind is a resource name, and the flag-shaped ones are refused like any other.
	for _, bad := range [][3]string{{"--raw", "pg", "db-0"}, {"Pod", "--all-namespaces", "db-0"}, {"Pod", "pg", "-l app=db"}, {"", "pg", "db-0"}} {
		if _, err := k3dManifestArgs(bad[0], bad[1], bad[2]); err == nil {
			t.Errorf("k3dManifestArgs(%q, %q, %q) was accepted", bad[0], bad[1], bad[2])
		}
	}
}

// The log picker is built from these, and which container it opens on decides whether the
// pane is useful on a six-container Percona pod.
func TestPodStateListsItsContainers(t *testing.T) {
	o := mustOne(t, podJSON("db-9", `{"phase":"Running","initContainerStatuses":[
		{"name":"pg-init","ready":true,"restartCount":0,"state":{"terminated":{"reason":"Completed","exitCode":0}}}],
		"containerStatuses":[
		{"name":"pgbouncer","ready":true,"restartCount":0,"state":{"running":{}}},
		{"name":"database","ready":false,"restartCount":7,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}`))
	if len(o.Containers) != 3 {
		t.Fatalf("containers = %+v", o.Containers)
	}
	if !o.Containers[0].Init || o.Containers[0].Name != "pg-init" {
		t.Errorf("init containers come first and are marked: %+v", o.Containers[0])
	}
	var db k3dStateContainer
	for _, c := range o.Containers {
		if c.Name == "database" {
			db = c
		}
	}
	if db.Tone != toneBad {
		t.Errorf("the crash-looping container's tone = %q — the picker leads with the worst", db.Tone)
	}
	if db.Restarts != 7 {
		t.Errorf("restarts = %d — the browser offers --previous only when something restarted", db.Restarts)
	}
}

// The custom-resource reader met a real PXC cluster and got two things wrong. Both are here
// so they cannot come back.
func TestCustomResourceReaderAgainstARealPXCStatus(t *testing.T) {
	// Verbatim shape from a running PXC cluster (percona-xtradb-cluster-operator 1.20):
	// conditions are a transition LOG with arbitrary status words, and a ready cluster still
	// carries `initializing: True` from ten minutes ago.
	o := mustOne(t, []byte(`{"items":[{"kind":"PerconaXtraDBCluster","metadata":{"name":"k3d-00","namespace":"pxc","uid":"c1"},
		"status":{"state":"ready","host":"172.20.255.246","ready":6,"size":6,"observedGeneration":1,
		"pxc":{"ready":3,"size":3,"status":"ready","image":"percona/percona-xtradb-cluster:8.4.8-8.1"},
		"haproxy":{"ready":3,"size":3,"status":"ready"},
		"conditions":[
			{"type":"tls","status":"enabled","lastTransitionTime":"2026-09-12T14:58:28Z"},
			{"type":"initializing","status":"True","lastTransitionTime":"2026-09-12T14:58:32Z"},
			{"type":"ready","status":"True","lastTransitionTime":"2026-09-12T15:02:22Z"}]}}]}`))

	if o.Tone != toneOK {
		t.Fatalf("a ready cluster came out %q: %+v", o.Tone, o.Props)
	}
	// `{type: tls, status: enabled}` is not a complaint. It is not "True", which is what the
	// first version of this rule keyed on, and it turned a healthy cluster amber.
	if p, ok := prop(o, "tls"); !ok || p.Tone != "" {
		t.Errorf("tls condition = %+v (ok=%v), want a row with no colour", p, ok)
	}
	// A stale True condition is history, not state.
	if _, ok := prop(o, "initializing"); ok {
		t.Error("a True condition should not take a row — the headline state is the answer")
	}
	if p, _ := prop(o, "pxc.status"); p.Tone != toneOK {
		t.Errorf("pxc.status = %+v, want green: a nested status IS a state", p)
	}
	if p, _ := prop(o, "pxc.ready"); p.Value != "3" || p.Tone != "" {
		t.Errorf("pxc.ready = %+v, want an uncoloured number that the canvas can watch change", p)
	}
}

// The other one: a value is only a state when its key says so. This lab's backup bucket is
// called "backup", "backup" is a word in the vocabulary, and a finished backup came out
// amber because of the name of its bucket.
func TestAValueIsOnlyAStateWhenItsKeySaysSo(t *testing.T) {
	o := mustOne(t, []byte(`{"items":[{"kind":"PerconaXtraDBClusterBackup","metadata":{"name":"b1","namespace":"pxc","uid":"b1"},
		"status":{"state":"Succeeded","storageName":"seaweedfs","image":"percona/percona-xtrabackup:8.4.0-5.1",
		"destination":"s3://backup/k3d-00-2026-09-12-15:03:19-full",
		"s3":{"bucket":"backup","region":"us-east-1","endpointUrl":"http://seaweedfs-01.example.net:8333"}}}]}`))

	if o.Tone != toneOK {
		t.Fatalf("a succeeded backup came out %q: %+v", o.Tone, o.Props)
	}
	for _, key := range []string{"s3.bucket", "destination", "storageName", "image"} {
		if p, _ := prop(o, key); p.Tone != "" {
			t.Errorf("%s = %+v — a value that is not a state must not be coloured", key, p)
		}
	}
	if p, _ := prop(o, "State"); p.Tone != toneOK {
		t.Errorf("State = %+v, want the headline coloured", p)
	}
}

// An operator that genuinely reports trouble still has to come out red.
func TestConditionsThatAreRealComplaintsStillLandRed(t *testing.T) {
	o := mustOne(t, []byte(`{"items":[{"kind":"Cluster","metadata":{"name":"c","namespace":"cnpg","uid":"c3"},
		"status":{"phase":"Setting up primary","conditions":[
			{"type":"Ready","status":"False","reason":"WaitingForPrimary"},
			{"type":"ContinuousArchiving","status":"False","reason":"WALArchiveFailing"}]}}]}`))
	if o.Tone != toneBad {
		t.Fatalf("tone = %q, want bad", o.Tone)
	}
	if p, _ := prop(o, "Ready"); p.Tone != toneBad {
		t.Errorf("a Ready=False condition = %+v, want red", p)
	}
	if p, _ := prop(o, "ContinuousArchiving"); p.Tone != toneWarn {
		t.Errorf("a non-ready condition that is False = %+v, want amber", p)
	}
	// And a status word that means failure is red wherever it appears as a state.
	bad := mustOne(t, []byte(`{"items":[{"kind":"PerconaXtraDBCluster","metadata":{"name":"c","namespace":"pxc","uid":"c9"},
		"status":{"state":"error","conditions":[{"type":"ready","status":"error"}]}}]}`))
	if bad.Tone != toneBad {
		t.Errorf("an operator saying error = %q", bad.Tone)
	}
}

// ---------------------------------------------------------------- cluster-dumps

// tarGz builds a pt-k8s-debug-collector-shaped archive out of a map of paths to contents,
// so the archive reader can be exercised without a cluster or a 500 KiB fixture.
func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, name := range sortedKeys(files) {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: "cluster-dump/" + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// The shapes below are the collector's, checked against a real capture of a PXC cluster: a
// List per resource per namespace, every item carrying its own `kind`, pod logs in
// <namespace>/<pod>/logs.txt beside whatever else was pulled off the container's disk.
var dumpFixture = map[string]string{
	"nodes.yaml": `apiVersion: v1
items:
- apiVersion: v1
  kind: Node
  metadata: {name: k3d-server-0}
  status:
    nodeInfo: {kubeletVersion: v1.33.13+k3s1}
    conditions:
    - {type: Ready, status: "True"}
kind: List
`,
	"pxc/pods.yaml": `apiVersion: v1
items:
- apiVersion: v1
  kind: Pod
  metadata: {name: cluster1-pxc-0, namespace: pxc, uid: u1}
  spec: {nodeName: k3d-server-0}
  status:
    phase: Running
    containerStatuses:
    - {name: pxc, ready: false, restartCount: 9, state: {waiting: {reason: CrashLoopBackOff}}}
    - {name: logs, ready: true, restartCount: 0, state: {running: {}}}
- apiVersion: v1
  kind: Pod
  metadata: {name: xb-backup-1, namespace: pxc, uid: u2}
  status:
    phase: Succeeded
    containerStatuses:
    - {name: xtrabackup, ready: false, restartCount: 0, state: {terminated: {reason: Completed, exitCode: 0}}}
kind: List
`,
	"pxc/statefulsets.yaml": `apiVersion: v1
items:
- apiVersion: apps/v1
  kind: StatefulSet
  metadata: {name: cluster1-pxc, namespace: pxc, uid: s1}
  spec: {replicas: 3}
  status: {replicas: 3, readyReplicas: 2}
kind: List
`,
	"pxc/persistentvolumeclaims.yaml": `apiVersion: v1
items: []
kind: List
`,
	"pxc/events.yaml": `apiVersion: v1
items:
- apiVersion: v1
  kind: Event
  type: Warning
  reason: BackOff
  message: Back-off restarting failed container
  count: 14
  lastTimestamp: "2026-09-12T11:00:00Z"
  involvedObject: {uid: u1, kind: Pod, name: cluster1-pxc-0, namespace: pxc}
kind: List
`,
	"pxc/perconaxtradbclusters.pxc.percona.com.yaml": `apiVersion: v1
items:
- apiVersion: pxc.percona.com/v1
  kind: PerconaXtraDBCluster
  metadata: {name: cluster1, namespace: pxc, uid: c1}
  status:
    state: initializing
    pxc: {ready: 2, size: 3, status: initializing}
kind: List
`,
	// The collector's own log of what it could not get. Real captures have entries in this
	// routinely, and a board that quietly shows fewer objects because half the cluster was
	// unreachable is the one thing this page must not do.
	"errors.txt":                                        "kubectl port-forward\nsignal: killed\n\nkubectl logs cluster1-pxc-1\ncontainer not found\n",
	"pxc/cluster1-pxc-0/logs.txt":                       "2026-09-12T11:00:00Z [ERROR] WSREP: failed to open gcomm backend\n",
	"pxc/cluster1-pxc-0/summary.txt":                    "# Percona Toolkit MySQL Summary Report\n",
	"pxc/cluster1-pxc-0/var/lib/mysql/mysqld-error.log": "2026-09-12T10:59:59Z [Note] Shutting down\n",
}

func dumpFiles(t *testing.T) opFiles {
	t.Helper()
	f, err := readOpArchive(tarGz(t, dumpFixture))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// The point of the whole exercise: a capture produces the SAME board a live cluster does.
// Same tones, same rows, same warnings — the collector's YAML and `kubectl get -o json` are
// the same objects in two encodings.
func TestBoardFromACaptureMatchesTheLiveRules(t *testing.T) {
	objs, nss, kinds, warnings := k3dStatesFromArchive(dumpFiles(t))

	byName := map[string]k3dStateObj{}
	for _, o := range objs {
		byName[o.Name] = o
	}
	if len(objs) != 5 {
		t.Fatalf("got %d objects, want 5: %v", len(objs), byName)
	}
	pod := byName["cluster1-pxc-0"]
	if pod.Tone != toneBad {
		t.Errorf("the crash-looping pod came out %q", pod.Tone)
	}
	if p, _ := prop(pod, "pxc"); p.Value != "CrashLoopBackOff" || p.Tone != toneBad {
		t.Errorf("container row = %+v", p)
	}
	// Its containers are there, which is what the log picker needs — even though a capture
	// keeps one log per pod rather than one per container.
	if len(pod.Containers) != 2 || pod.Containers[0].Name != "pxc" {
		t.Errorf("containers = %+v", pod.Containers)
	}
	// Events are per namespace in an archive rather than one cluster-wide list.
	if len(pod.Events) != 1 || pod.Events[0].Count != 14 {
		t.Errorf("events = %+v", pod.Events)
	}
	if byName["xb-backup-1"].Tone != toneDone {
		t.Errorf("a completed backup pod came out %q", byName["xb-backup-1"].Tone)
	}
	if byName["cluster1-pxc"].Tone != toneWarn {
		t.Errorf("a StatefulSet 2/3 ready came out %q", byName["cluster1-pxc"].Tone)
	}
	if byName["cluster1"].Tone != toneWarn || byName["cluster1"].Summary != "initializing" {
		t.Errorf("the custom resource = %+v", byName["cluster1"])
	}
	if byName["k3d-server-0"].Kind != "Node" {
		t.Errorf("the cluster-scoped node is missing: %v", byName)
	}
	if strings.Join(nss, ",") != "pxc" {
		t.Errorf("namespaces = %v", nss)
	}
	if !containsStr(kinds, "PerconaXtraDBCluster") {
		t.Errorf("kinds = %v", kinds)
	}
	// The board says what a capture cannot show rather than looking like a cluster with no
	// Services and nothing happening.
	if len(warnings) == 0 || !strings.Contains(warnings[0], "one instant") {
		t.Errorf("warnings = %v", warnings)
	}
	// ...and what the capture itself failed to collect, which is the difference between "the
	// cluster had nothing there" and "the collector could not read it".
	if len(warnings) < 2 || !strings.Contains(warnings[1], "2 collection error(s)") ||
		!strings.Contains(warnings[1], "kubectl port-forward") {
		t.Errorf("collector errors are not reported: %v", warnings)
	}
}

func TestCollectorErrorsAreCountedAndQuoted(t *testing.T) {
	n, first := k3dArchiveCollectorErrors(opFiles{"errors.txt": []byte("a cmd\nfailed\n\nb cmd\nalso failed\n")})
	if n != 2 || first != "a cmd failed" {
		t.Errorf("n=%d first=%q", n, first)
	}
	// A capture that collected everything says nothing at all.
	if n, _ := k3dArchiveCollectorErrors(opFiles{"errors.txt": []byte("  \n\n ")}); n != 0 {
		t.Errorf("an empty errors.txt reported %d errors", n)
	}
	if n, _ := k3dArchiveCollectorErrors(opFiles{}); n != 0 {
		t.Errorf("a capture with no errors.txt reported %d errors", n)
	}
	// One long entry is quoted to a line rather than pasted into the header — measured in
	// characters, since the ellipsis it ends with is three bytes and one of them.
	n, first = k3dArchiveCollectorErrors(opFiles{"errors.txt": []byte("cmd\n" + strings.Repeat("x", 400))})
	if n != 1 || len([]rune(first)) > 120 {
		t.Errorf("n=%d runes=%d", n, len([]rune(first)))
	}
	// And a multi-byte character on the boundary survives whole rather than as a half-glyph.
	_, first = k3dArchiveCollectorErrors(opFiles{"errors.txt": []byte(strings.Repeat("é", 400))})
	if !strings.HasSuffix(first, "…") || strings.Contains(first, "\ufffd") {
		t.Errorf("trimmed mid-character: %q", first[len(first)-8:])
	}
}

func TestManifestOutOfACapture(t *testing.T) {
	f := dumpFiles(t)
	y, err := k3dArchiveManifest(f, "PerconaXtraDBCluster", "pxc", "cluster1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"kind: PerconaXtraDBCluster", "name: cluster1", "state: initializing"} {
		if !strings.Contains(y, want) {
			t.Errorf("YAML is missing %q:\n%s", want, y)
		}
	}
	// Cluster-scoped objects live in the archive's root file, with no namespace.
	if _, err := k3dArchiveManifest(f, "Node", "", "k3d-server-0"); err != nil {
		t.Errorf("node manifest: %v", err)
	}
	// A name that is not in the archive is a clear miss, not an empty document.
	if _, err := k3dArchiveManifest(f, "Pod", "pxc", "nope"); err == nil {
		t.Error("a missing object should say so")
	}
	// Nor may a kind be answered with another kind's object of the same name.
	if _, err := k3dArchiveManifest(f, "Service", "pxc", "cluster1-pxc-0"); err == nil {
		t.Error("a Pod was returned as a Service")
	}
}

// A capture keeps one log per pod — plus whatever pt-k8s-debug-collector pulled off the
// container's disk, which on a PXC pod is the mysqld error log. That is more than the live
// board can offer, so it is offered.
func TestPodFilesOutOfACapture(t *testing.T) {
	f := dumpFiles(t)
	files := k3dArchivePodFiles(f, "pxc", "cluster1-pxc-0")
	if len(files) != 3 || files[0] != "logs.txt" {
		t.Fatalf("files = %v, want logs.txt first", files)
	}
	if !containsStr(files, "var/lib/mysql/mysqld-error.log") {
		t.Errorf("the collector's extra files are missing: %v", files)
	}
	body, ok := k3dArchiveFile(f, "pxc", "cluster1-pxc-0", "var/lib/mysql/mysqld-error.log")
	if !ok || !strings.Contains(body, "Shutting down") {
		t.Errorf("file read = %q (ok=%v)", body, ok)
	}
	// A pod the capture kept nothing for, and a path trying to leave the pod's directory.
	if len(k3dArchivePodFiles(f, "pxc", "xb-backup-1")) != 0 {
		t.Error("invented files for a pod with none")
	}
	if _, ok := k3dArchiveFile(f, "pxc", "cluster1-pxc-0", "../../nodes.yaml"); ok {
		t.Error("a file name escaped the pod's directory")
	}
}

// An uploaded archive is held against the uploader, briefly, and belongs to nobody else.
func TestUploadedArchivesAreHeldPerUser(t *testing.T) {
	k3dStateUploads.Range(func(k, _ any) bool { k3dStateUploads.Delete(k); return true })
	tok := k3dStateUploadKeep(7, "a.tar.gz", []byte("one"))
	if tok == "" {
		t.Fatal("no token")
	}
	if _, _, ok := k3dStateUploadGet(7, tok); !ok {
		t.Error("the uploader cannot read their own archive")
	}
	if _, _, ok := k3dStateUploadGet(8, tok); ok {
		t.Error("another user read it — an uploaded archive may be a customer's cluster")
	}
	if _, _, ok := k3dStateUploadGet(7, "nonsense"); ok {
		t.Error("a made-up token resolved")
	}
	// Only so many per user: these are whole archives held in memory.
	for i := 0; i < k3dStateUploadPerUser+1; i++ {
		k3dStateUploadKeep(7, "b.tar.gz", []byte("two"))
	}
	n := 0
	k3dStateUploads.Range(func(_, v any) bool {
		if v.(*k3dStateUpload).Owner == 7 {
			n++
		}
		return true
	})
	if n > k3dStateUploadPerUser {
		t.Errorf("%d archives held for one user, cap is %d", n, k3dStateUploadPerUser)
	}
	if _, _, ok := k3dStateUploadGet(7, tok); ok {
		t.Error("the oldest archive should have been evicted")
	}
}

// An archive that is not a cluster-dump must fail as one, not produce an empty board.
func TestARandomTarballIsNotACaptureForTheBoard(t *testing.T) {
	if _, err := readOpArchive(tarGz(t, map[string]string{"notes.txt": "hello"})); err != nil {
		t.Fatalf("a tarball with a file in it reads: %v", err)
	}
	f, _ := readOpArchive(tarGz(t, map[string]string{"notes.txt": "hello"}))
	objs, _, _, warnings := k3dStatesFromArchive(f)
	if len(objs) != 0 {
		t.Errorf("invented %d objects", len(objs))
	}
	if len(warnings) < 2 || !strings.Contains(warnings[1], "no objects") {
		t.Errorf("warnings = %v — an empty board has to say why", warnings)
	}
}

// The archive reader follows pt-k8s-debug-collector's own layout (its paths.go), and two
// layouts are in the wild: a current collector writes one log per CONTAINER and puts
// cluster-scoped resources under cluster-scope/, an older one writes a single logs.txt per
// pod and puts them at the root. Both have to read.
func TestBothCollectorLayoutsRead(t *testing.T) {
	modern := map[string]string{
		"cluster-scope/nodes.yaml": `apiVersion: v1
items:
- kind: Node
  metadata: {name: node-a}
  status: {conditions: [{type: Ready, status: "True"}]}
kind: List
`,
		"pxc/pods.yaml": `apiVersion: v1
items:
- kind: Pod
  metadata: {name: cluster1-pxc-0, namespace: pxc}
  status:
    phase: Running
    containerStatuses:
    - {name: pxc, ready: true, restartCount: 0, state: {running: {}}}
    - {name: logs, ready: true, restartCount: 0, state: {running: {}}}
kind: List
`,
		// One log per container, plus the summary and the backup logs the collector pulls
		// off a PXC container (its resources.go).
		"pxc/cluster1-pxc-0/pxc.log":                              "starting mysqld\n",
		"pxc/cluster1-pxc-0/logs.log":                             "fluent-bit up\n",
		"pxc/cluster1-pxc-0/summary.txt":                          "# Percona Toolkit MySQL Summary Report\n",
		"pxc/cluster1-pxc-0/var/lib/mysql/innobackup.backup.log":  "xtrabackup: completed OK!\n",
		"pxc/cluster1-pxc-0/var/lib/mysql/innobackup.prepare.log": "xtrabackup: prepare OK\n",
		// Secrets in their own directory, which must not be mistaken for a pod's files.
		"pxc/secrets/cluster1-secrets.yaml": `apiVersion: v1
kind: Secret
metadata: {name: cluster1-secrets, namespace: pxc}
type: Opaque
data: {root: cm9vdA==}
`,
	}
	f, err := readOpArchive(tarGz(t, modern))
	if err != nil {
		t.Fatal(err)
	}
	objs, nss, kinds, _ := k3dStatesFromArchive(f)
	byName := map[string]k3dStateObj{}
	for _, o := range objs {
		byName[o.Name] = o
	}
	if _, ok := byName["node-a"]; !ok {
		t.Errorf("a cluster-scope/ node was missed: %v", kinds)
	}
	if _, ok := byName["cluster1-pxc-0"]; !ok {
		t.Error("the pod was missed")
	}
	// A Secret is in the capture, so it is on the board — as an inventory card with a key
	// count and NEVER a value, which is the Secrets editor's job.
	sec, ok := byName["cluster1-secrets"]
	if !ok {
		t.Fatalf("the secret was missed: %v", kinds)
	}
	if sec.Tone != toneOK || sec.Summary != "1 key" {
		t.Errorf("secret card = %+v", sec)
	}
	for _, p := range sec.Props {
		if strings.Contains(p.Value, "root") || strings.Contains(p.Value, "cm9vdA") {
			t.Fatalf("a secret's contents reached the board: %+v", p)
		}
	}
	if strings.Join(nss, ",") != "pxc" {
		t.Errorf("namespaces = %v", nss)
	}

	// The per-container logs are the pod's files, and the log the pane opens on is a log
	// rather than auto.cnf — with the backup logs ranked above the rest of the disk files.
	files := k3dArchivePodFiles(f, "pxc", "cluster1-pxc-0")
	if len(files) != 5 {
		t.Fatalf("pod files = %v", files)
	}
	if !strings.HasSuffix(files[0], ".log") || strings.Contains(files[0], "/") {
		t.Errorf("the pane would open on %q, not a container log", files[0])
	}
	if files[2] != "summary.txt" {
		t.Errorf("summary should follow the logs: %v", files)
	}
	if !strings.Contains(files[3], "innobackup") {
		t.Errorf("the backup logs should lead the disk files: %v", files)
	}
	// A secrets/ directory is not a pod, so its files are not pod files.
	if got := k3dArchivePodFiles(f, "pxc", "secrets"); len(got) != 0 {
		t.Errorf("secrets/ was read as a pod: %v", got)
	}
}

// Nothing in an archive should be invisible: every file is in the tree, classified, and
// readable.
func TestEveryFileInACaptureIsReachable(t *testing.T) {
	f := dumpFiles(t)
	tree := k3dArchiveTree(f)
	if len(tree) != len(dumpFixture) {
		t.Fatalf("tree has %d of %d files", len(tree), len(dumpFixture))
	}
	kinds := map[string]string{}
	for _, e := range tree {
		kinds[e.Path] = e.Kind
	}
	for path, want := range map[string]string{
		"errors.txt":                     "log",
		"pxc/pods.yaml":                  "resource",
		"pxc/events.yaml":                "events",
		"pxc/cluster1-pxc-0/logs.txt":    "podfile",
		"pxc/cluster1-pxc-0/summary.txt": "podfile",
	} {
		if kinds[path] != want {
			t.Errorf("%s classified as %q, want %q", path, kinds[path], want)
		}
	}
	// And any of them reads back.
	if body, ok := k3dArchiveAnyFile(f, "errors.txt"); !ok || !strings.Contains(body, "port-forward") {
		t.Errorf("errors.txt = %q (ok=%v)", body, ok)
	}
	for _, bad := range []string{"", "../etc/passwd", "pxc/../../x", "nope.txt"} {
		if _, ok := k3dArchiveAnyFile(f, bad); ok {
			t.Errorf("%q was served", bad)
		}
	}
}

// A capture contains every resource the API server serves, including kinds with no status at
// all. Those are inventory, not health — a board of amber "no status" cards about Roles is
// what teaches people to ignore amber.
func TestStatuslessKindsAreNeutral(t *testing.T) {
	o := mustOne(t, []byte(`{"items":[{"kind":"ClusterRole","metadata":{"name":"view"},"rules":[]}]}`))
	if o.Tone != toneOK || o.Summary != "—" {
		t.Errorf("a ClusterRole came out %q/%q", o.Tone, o.Summary)
	}
	// A custom resource with no status is the opposite: the operator has not touched it.
	cr := mustOne(t, []byte(`{"items":[{"kind":"PerconaXtraDBCluster","metadata":{"name":"c","namespace":"pxc"}}]}`))
	if cr.Tone != toneWarn || cr.Summary != "no status" {
		t.Errorf("an untouched custom resource came out %q/%q", cr.Tone, cr.Summary)
	}
}

func TestVolumeCronJobAndBudgetRules(t *testing.T) {
	// A Released volume is a claim's outage seen from the other end.
	pv := mustOne(t, []byte(`{"items":[{"kind":"PersistentVolume","metadata":{"name":"pvc-1"},
		"spec":{"capacity":{"storage":"6G"},"storageClassName":"local-path","persistentVolumeReclaimPolicy":"Delete",
		"claimRef":{"namespace":"pxc","name":"datadir-cluster1-pxc-0"}},"status":{"phase":"Released"}}]}`))
	if pv.Tone != toneWarn {
		t.Errorf("a Released volume came out %q", pv.Tone)
	}
	if p, _ := prop(pv, "Claimed by"); p.Value != "pxc/datadir-cluster1-pxc-0" {
		t.Errorf("claim ref = %+v", p)
	}
	if failed := mustOne(t, []byte(`{"items":[{"kind":"PersistentVolume","metadata":{"name":"p"},"status":{"phase":"Failed","reason":"deleter failed"}}]}`)); failed.Tone != toneBad {
		t.Errorf("a Failed volume came out %q", failed.Tone)
	}

	// A suspended schedule is why there has been no backup since Tuesday.
	cj := mustOne(t, []byte(`{"items":[{"kind":"CronJob","metadata":{"name":"backup","namespace":"pxc"},
		"spec":{"schedule":"0 0 * * *","suspend":true},"status":{"lastScheduleTime":"2026-09-10T00:00:00Z"}}]}`))
	if cj.Tone != toneWarn || cj.Summary != "suspended" {
		t.Errorf("a suspended CronJob = %q/%q", cj.Tone, cj.Summary)
	}

	// disruptionsAllowed: 0 is why a rolling restart is sitting there doing nothing.
	pdb := mustOne(t, []byte(`{"items":[{"kind":"PodDisruptionBudget","metadata":{"name":"pxc","namespace":"pxc"},
		"status":{"currentHealthy":3,"desiredHealthy":3,"expectedPods":3,"disruptionsAllowed":0}}]}`))
	if pdb.Tone != toneWarn {
		t.Errorf("a budget allowing no disruption came out %q (%+v)", pdb.Tone, pdb.Props)
	}
	if short := mustOne(t, []byte(`{"items":[{"kind":"PodDisruptionBudget","metadata":{"name":"p","namespace":"x"},
		"status":{"currentHealthy":1,"desiredHealthy":3,"disruptionsAllowed":0}}]}`)); short.Tone != toneBad {
		t.Errorf("a budget below its floor came out %q", short.Tone)
	}
}

// k3dUploadFixture posts the fixture archive the way the page does and returns its token.
func k3dUploadFixture(t *testing.T, app *App, cookie *http.Cookie) string {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	w, err := mw.CreateFormFile("file", "cluster-dump.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	w.Write(tarGz(t, dumpFixture))
	mw.Close()
	r := httptest.NewRequest(http.MethodPost, "/api/k8sstates/upload", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.handleK3DStatesUpload(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("upload = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Upload string `json:"upload"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Upload == "" {
		t.Fatalf("no upload token: %v %s", err, rec.Body.String())
	}
	return out.Upload
}

// A log pane opens on the pod's WORST CONTAINER, because that is the right answer for a live
// cluster and the pane cannot know it is not looking at one. A capture has no containers, so
// that first question names something the archive does not have — and answering it with a 404
// was the bug: the pane never learned that logs.txt, summary.txt and the mysqld error log
// were sitting right there. Every one of these asks a question a real pane asks, and every
// one of them must come back with the file list.
func TestALogPaneOnACaptureAlwaysLearnsWhatTheCaptureKept(t *testing.T) {
	app, cookie := ftdcAuthed(t)
	tok := k3dUploadFixture(t, app, cookie)

	read := func(file string) (int, map[string]any) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet,
			"/api/k8sstates/archive/logs?upload="+tok+"&namespace=pxc&name=cluster1-pxc-0&file="+url.QueryEscape(file), nil)
		r.AddCookie(cookie)
		rec := httptest.NewRecorder()
		app.handleK3DStateArchiveLogs(rec, r)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// "pxc" is a container of that pod, and nothing in the capture is called that.
	code, out := read("pxc")
	if code != http.StatusOK {
		t.Fatalf("a container name got %d %v — the pane is left with no file list at all", code, out)
	}
	files, _ := out["files"].([]any)
	if len(files) != 3 {
		t.Fatalf("files = %v, want the three the collector kept", out["files"])
	}
	if out["file"] != "logs.txt" || !strings.Contains(out["text"].(string), "gcomm") {
		t.Errorf("fell back to %v (%q)", out["file"], out["text"])
	}
	if note, _ := out["note"].(string); !strings.Contains(note, "pxc") || !strings.Contains(note, "logs.txt") {
		t.Errorf("note = %q — it has to say what was asked for and what came back", note)
	}

	// And the files themselves, which is what somebody came for: pt-mysql-summary's output
	// and what the collector pulled off the container's disk.
	for file, want := range map[string]string{
		"summary.txt":                    "Percona Toolkit",
		"var/lib/mysql/mysqld-error.log": "Shutting down",
		"mysqld-error.log":               "Shutting down", // its bare name, as the picker shows it nested
	} {
		code, out := read(file)
		if code != http.StatusOK || !strings.Contains(out["text"].(string), want) {
			t.Errorf("%s = %d %v", file, code, out["text"])
		}
	}
}

// The pick itself, without the HTTP around it. The two forgiving matches are the same
// question in a different dialect, and the last case is the one that must never 404.
func TestPickingAPodsFileForgivesTheLiveBoardsVocabulary(t *testing.T) {
	perContainer := []string{"pxc.log", "logs.log", "summary.txt", "var/lib/mysql/mysqld-error.log"}
	for _, tc := range []struct {
		available []string
		want      string
		file      string
		matched   bool
	}{
		{perContainer, "", "pxc.log", true},                                        // the pane asks for nothing
		{perContainer, "summary.txt", "summary.txt", true},                         // exact
		{perContainer, "pxc", "pxc.log", true},                                     // a container name, per-container layout
		{perContainer, "mysqld-error.log", "var/lib/mysql/mysqld-error.log", true}, // a bare name for a nested file
		{perContainer, "nothing-like-it", "pxc.log", false},                        // answered, not refused
		{[]string{"logs.txt"}, "pxc", "logs.txt", false},                           // the older one-log-per-pod layout
		{nil, "pxc", "", false},                                                    // a pod the capture kept nothing for
	} {
		file, matched := k3dArchivePickPodFile(tc.available, tc.want)
		if file != tc.file || matched != tc.matched {
			t.Errorf("pick(%v, %q) = %q/%v, want %q/%v", tc.available, tc.want, file, matched, tc.file, tc.matched)
		}
	}
}
