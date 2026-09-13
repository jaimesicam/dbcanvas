package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// k3dstates.go — Kubernetes States: every object in a K3D cluster reduced to a tone, a
// one-line summary and a short list of properties, sampled on a timer so a canvas can
// watch it change.
//
// The other Kubernetes panels in this app each answer one question — what is the custom
// resource (k3dcrform.go), what are the Secrets (k3dobjects.go), what happened in the
// logs (opsummary.go). This one answers the question you actually ask while a lab is
// running: *what is happening right now, and what is wrong*. That is not a list of YAML.
// It is a handful of numbers per object — ready 5 of 6, restarts 12, phase Unknown,
// Bound, NotReady — and the interesting thing about them is when they CHANGE.
//
// Three rules shaped this file:
//
//  1. THE TONE IS THE OBJECT'S OWN STATUS, NOTHING ELSE. Red means the object says it is
//     broken: a Failed pod, a container in CrashLoopBackOff, a Lost claim, a NotReady
//     node, a custom resource in error. It never means "something nearby looks odd" and
//     it is never inferred from events — a canvas where everything is red on a busy
//     cluster is a canvas nobody looks at. Warning events are counted and shown in the
//     detail instead, where they explain a red rather than causing one.
//
//  2. PROPERTIES ARE TONED INDIVIDUALLY. The card says the pod is bad; the property rows
//     say *which* number is the bad one, because that is what the canvas highlights when
//     it changes. A pod with 12 restarts and a container waiting on ImagePullBackOff has
//     exactly two rows worth looking at, and they should be the two that are coloured.
//
//  3. EVERY RULE IS A PURE FUNCTION OF ONE DECODED OBJECT. No cluster is needed to test
//     "a Succeeded pod is done, not bad" — see k3dstates_test.go. The kubectl half is a
//     dozen lines at the bottom.
//
// Everything runs through the k3s server node's own kubectl, like the rest of the
// Kubernetes work here (see k3d.go's header). One `kubectl get` covers the built-in
// kinds, a second covers events, a third covers the operator's custom resources — three
// execs per sample, whatever the cluster holds.

// k3dStateKinds are the built-in kinds the canvas samples, in the order they are laid out.
//
// The list follows what pt-k8s-debug-collector collects, so that a live cluster and a
// capture of one draw the same board (k3dstatesdump.go). The collector discovers every
// resource the API server serves and writes the non-empty ones; sampling *that* live, every
// few seconds, would be a great deal of kubectl for a great deal of noise — so the live half
// takes everything with a status worth watching and leaves the rest (RBAC, ConfigMaps,
// Secrets, StorageClasses) to the capture, where it costs nothing because it is already in
// the file. Nothing is hidden either way: the board's kind chips show what is there.
//
// ReplicaSets are in the list but hidden by default in the browser — a cluster keeps one
// dead ReplicaSet per revision — which is a display decision rather than a collection one.
var k3dStateKinds = []string{
	"nodes", "pods", "statefulsets", "deployments", "replicasets", "jobs", "cronjobs",
	"persistentvolumeclaims", "persistentvolumes", "services", "poddisruptionbudgets",
}

// k3dStateCRs are the operator's own custom resources, per operator key: the cluster
// object first, then the things you ask it to do. These are the objects a Percona lab is
// actually about, so they are sampled even though they cost a second kubectl — and their
// absence is tolerated, since a cluster whose operator never installed has no such CRD
// and must still show its pods.
var k3dStateCRs = map[string][]string{
	"pxc":   {"perconaxtradbclusters", "perconaxtradbclusterbackups", "perconaxtradbclusterrestores"},
	"ps":    {"perconaservermysqls", "perconaservermysqlbackups", "perconaservermysqlrestores"},
	"psmdb": {"perconaservermongodbs", "perconaservermongodbbackups", "perconaservermongodbrestores"},
	"pg":    {"perconapgclusters", "perconapgbackups", "perconapgrestores"},
	// The two Helm-installed community PostgreSQL operators. CloudNativePG's Cluster is
	// the one object worth watching there; Crunchy's postgrescluster is the same shape as
	// the Percona pg one, which it is upstream of.
	"cloudnative-pg": {"clusters.postgresql.cnpg.io"},
	"crunchy":        {"postgresclusters"},
}

// Tones. Deliberately four and not a severity number: a canvas paints backgrounds, and
// four is what a person can tell apart at a glance across six themes.
const (
	toneOK   = "ok"   // doing what it says it should
	toneWarn = "warn" // on its way somewhere, or short of what it wants
	toneBad  = "bad"  // broken, and says so itself
	toneDone = "done" // finished on purpose — a completed pod or job
)

// k3dToneRank orders tones by how much they want attention. An object's own tone is the
// worst of its properties, which is why an object starts with NO tone rather than with
// toneOK: "done" is less alarming than "ok" and would never win against a default, so a
// completed backup pod would come out green — the one colour it must not be, since a lab
// cluster is mostly completed backup pods and a canvas of false greens is as useless as a
// canvas of false reds.
var k3dToneRank = map[string]int{"": 0, toneDone: 1, toneOK: 2, toneWarn: 3, toneBad: 4}

func k3dWorseTone(a, b string) string {
	if k3dToneRank[b] > k3dToneRank[a] {
		return b
	}
	return a
}

// k3dProp is one property of one object: a label, its current value as text, and how
// worried to be about it. Value is text on purpose — the canvas compares snapshots to
// find what changed, and "3" changing to "2" is the event, not the type.
type k3dProp struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Tone  string `json:"tone,omitempty"`
}

// k3dStateEvent is one Kubernetes warning about an object, as the detail pane shows it.
type k3dStateEvent struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Count   int    `json:"count,omitempty"`
	At      string `json:"at,omitempty"`
}

// k3dStateContainer is one container of a pod, for the log picker. A pod is several logs,
// not one, and which of them you want is nearly always decided by this: the container that
// is not running is the one to read.
type k3dStateContainer struct {
	Name     string `json:"name"`
	Init     bool   `json:"init,omitempty"`
	Tone     string `json:"tone,omitempty"`     // how its own row was toned — the picker leads with the worst
	Restarts int    `json:"restarts,omitempty"` // >0 means --previous has something to show
}

// k3dStateObj is one object on the canvas.
type k3dStateObj struct {
	UID        string              `json:"uid"`
	Kind       string              `json:"kind"`
	Namespace  string              `json:"namespace"`
	Name       string              `json:"name"`
	Tone       string              `json:"tone"`
	Summary    string              `json:"summary"`
	Owner      string              `json:"owner,omitempty"`
	CreatedAt  string              `json:"createdAt,omitempty"`
	Props      []k3dProp           `json:"props"`
	Events     []k3dStateEvent     `json:"events,omitempty"`
	Containers []k3dStateContainer `json:"containers,omitempty"`
}

// add appends a property and lifts the object's tone to match it. Empty values are
// dropped rather than rendered as a blank row: every kind has fields that only some
// objects carry (a pod that has not been scheduled has no node), and a canvas of
// half-empty rows reads as broken.
func (o *k3dStateObj) add(key, value, tone string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	o.Props = append(o.Props, k3dProp{Key: key, Value: value, Tone: tone})
	o.Tone = k3dWorseTone(o.Tone, tone)
}

// ---------------------------------------------------------------- decoding

// k3dRawItem is the part of any Kubernetes object these rules read. Spec and Status stay
// raw and are decoded per kind, which keeps one narrow struct per rule instead of one
// enormous one that is 90% irrelevant to whichever kind is in front of it.
type k3dRawItem struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name              string            `json:"name"`
		Namespace         string            `json:"namespace"`
		UID               string            `json:"uid"`
		CreationTimestamp string            `json:"creationTimestamp"`
		DeletionTimestamp string            `json:"deletionTimestamp"`
		Labels            map[string]string `json:"labels"`
		OwnerReferences   []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Spec   json.RawMessage `json:"spec"`
	Status json.RawMessage `json:"status"`
	// The whole object as it arrived. Most kinds are read through Spec and Status, but some
	// carry what matters at the top level — a ConfigMap's `data`, a Secret's `type` — and a
	// rule that needs those should not have to be handed the bytes a second time.
	Raw json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps the bytes it decoded from, so Raw is filled on every path that decodes
// an item: the live sample, the archive reader, and a test's fixture alike.
func (it *k3dRawItem) UnmarshalJSON(b []byte) error {
	type item k3dRawItem // no methods, so this does not recurse
	var a item
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*it = k3dRawItem(a)
	it.Raw = append(json.RawMessage(nil), b...)
	return nil
}

type k3dRawList struct {
	Items []k3dRawItem `json:"items"`
}

// parseK3DStates turns one or more `kubectl get … -o json` documents into canvas objects.
// A document that does not parse is skipped rather than failing the sample: the custom
// resource call is allowed to fail on a cluster with no operator, and one bad document
// must not take the pods down with it.
func parseK3DStates(docs ...[]byte) []k3dStateObj {
	var out []k3dStateObj
	for _, doc := range docs {
		if len(doc) == 0 {
			continue
		}
		var list k3dRawList
		if err := json.Unmarshal(doc, &list); err != nil {
			// A single object rather than a List — `kubectl get pod x -o json`.
			var one k3dRawItem
			if json.Unmarshal(doc, &one) != nil || one.Metadata.Name == "" {
				continue
			}
			list.Items = []k3dRawItem{one}
		}
		for _, it := range list.Items {
			if it.Metadata.Name == "" || it.Kind == "Event" {
				continue // Events are attached to objects, they are not objects
			}
			out = append(out, k3dStateOf(it))
		}
	}
	sortK3DStates(out)
	return out
}

// sortK3DStates orders the canvas: by kind in k3dStateKinds order (custom resources last,
// alphabetically), then namespace, then name. Stable across samples on purpose — a card
// that moves because a list came back in a different order is a card you cannot watch.
func sortK3DStates(objs []k3dStateObj) {
	rank := map[string]int{}
	for i, k := range k3dStateKinds {
		rank[k3dKindSingular(k)] = i
	}
	sort.SliceStable(objs, func(i, j int) bool {
		a, b := objs[i], objs[j]
		ra, oka := rank[strings.ToLower(a.Kind)]
		rb, okb := rank[strings.ToLower(b.Kind)]
		if oka != okb {
			return oka // a known built-in kind sorts before a custom resource
		}
		if oka && ra != rb {
			return ra < rb
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
}

// k3dKindSingular maps the plural kubectl resource name to the singular Kind a document
// carries ("statefulsets" → "statefulset"), for ordering only.
func k3dKindSingular(resource string) string {
	return strings.TrimSuffix(resource, "s")
}

// k3dStateOf applies the rule for one object's kind. Everything unknown — a custom
// resource, a kind added later — goes through the generic reader, which is why a cluster
// running an operator this file has never heard of still shows something useful.
func k3dStateOf(it k3dRawItem) k3dStateObj {
	o := k3dStateObj{
		UID: it.Metadata.UID, Kind: it.Kind, Namespace: it.Metadata.Namespace,
		Name: it.Metadata.Name, CreatedAt: it.Metadata.CreationTimestamp,
		// Never nil: `props: null` renders as an empty card in the browser, and a card
		// with no rows is indistinguishable from an object with nothing to say.
		Props: []k3dProp{},
	}
	if o.UID == "" { // a fixture, or an object from an API server that omitted it
		o.UID = strings.ToLower(o.Kind) + "/" + o.Namespace + "/" + o.Name
	}
	for _, ref := range it.Metadata.OwnerReferences {
		o.Owner = ref.Kind + "/" + ref.Name
		break
	}
	switch strings.ToLower(it.Kind) {
	case "pod":
		k3dPodState(&o, it)
	case "statefulset", "deployment", "replicaset":
		k3dWorkloadState(&o, it)
	case "job":
		k3dJobState(&o, it)
	case "persistentvolumeclaim":
		k3dClaimState(&o, it)
	case "persistentvolume":
		k3dVolumeState(&o, it)
	case "cronjob":
		k3dCronJobState(&o, it)
	case "poddisruptionbudget":
		k3dBudgetState(&o, it)
	case "configmap", "secret":
		k3dDataState(&o, it)
	case "service":
		k3dServiceState(&o, it)
	case "node":
		k3dNodeState(&o, it)
	default:
		k3dGenericState(&o, it)
	}
	// Being deleted outranks whatever the object last said about itself: a Running pod
	// with a deletionTimestamp is on its way out, and saying "Running" in green while it
	// disappears is the one thing a watcher must not be told.
	if it.Metadata.DeletionTimestamp != "" {
		o.Summary = "Terminating"
		o.Tone = k3dWorseTone(o.Tone, toneWarn)
		o.Props = append([]k3dProp{{Key: "Terminating since", Value: it.Metadata.DeletionTimestamp, Tone: toneWarn}}, o.Props...)
	}
	if o.Summary == "" {
		o.Summary = "—"
	}
	// Nothing said anything worth colouring — a Service is just a Service.
	if o.Tone == "" {
		o.Tone = toneOK
	}
	return o
}

// ---------------------------------------------------------------- pods

// k3dBadWaitReasons are the container waiting reasons that mean "this is not coming up on
// its own". Anything else — ContainerCreating, PodInitializing — is a pod mid-flight and
// is a warning at most, because every pod passes through those on the way to running.
var k3dBadWaitReasons = map[string]bool{
	"CrashLoopBackOff": true, "ImagePullBackOff": true, "ErrImagePull": true,
	"CreateContainerConfigError": true, "CreateContainerError": true,
	"InvalidImageName": true, "RunContainerError": true, "CCFailedToStart": true,
}

type k3dContainerStatus struct {
	Name         string `json:"name"`
	Ready        bool   `json:"ready"`
	RestartCount int    `json:"restartCount"`
	State        struct {
		Waiting *struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"waiting"`
		Terminated *struct {
			Reason   string `json:"reason"`
			ExitCode int    `json:"exitCode"`
		} `json:"terminated"`
		Running *struct {
			StartedAt string `json:"startedAt"`
		} `json:"running"`
	} `json:"state"`
}

type k3dPodStatus struct {
	Phase             string               `json:"phase"`
	Reason            string               `json:"reason"`
	Message           string               `json:"message"`
	PodIP             string               `json:"podIP"`
	StartTime         string               `json:"startTime"`
	ContainerStatuses []k3dContainerStatus `json:"containerStatuses"`
	InitContainerSt   []k3dContainerStatus `json:"initContainerStatuses"`
	Conditions        []struct {
		Type    string `json:"type"`
		Status  string `json:"status"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"conditions"`
}

// k3dContainerTone is how one container is doing, in the same vocabulary as everything else.
// The log picker sorts by it: on a pod with six containers, the one worth reading is the one
// that is not running.
func k3dContainerTone(c k3dContainerStatus) string {
	switch {
	case c.State.Waiting != nil:
		if k3dBadWaitReasons[c.State.Waiting.Reason] {
			return toneBad
		}
		return toneWarn
	case c.State.Terminated != nil:
		if c.State.Terminated.ExitCode != 0 {
			return toneBad
		}
		return toneDone
	case c.State.Running != nil && !c.Ready:
		return toneWarn
	}
	return toneOK
}

func k3dPodState(o *k3dStateObj, it k3dRawItem) {
	var st k3dPodStatus
	json.Unmarshal(it.Status, &st)
	var spec struct {
		NodeName string `json:"nodeName"`
	}
	json.Unmarshal(it.Spec, &spec)

	ready, restarts := 0, 0
	for _, c := range st.ContainerStatuses {
		if c.Ready {
			ready++
		}
		restarts += c.RestartCount
	}
	total := len(st.ContainerStatuses)
	o.Summary = fmt.Sprintf("%d/%d %s", ready, total, orDefault(st.Phase, "Unknown"))

	// The phase first, because it is the thing that decides the card's colour.
	switch st.Phase {
	case "Running":
		if total > 0 && ready < total {
			o.add("Phase", "Running", toneWarn)
		} else {
			o.add("Phase", "Running", toneOK)
		}
	case "Succeeded":
		o.add("Phase", "Succeeded", toneDone)
		o.Summary = fmt.Sprintf("%d/%d Completed", ready, total)
	case "Pending":
		o.add("Phase", "Pending", toneWarn)
	case "Failed", "Unknown":
		o.add("Phase", st.Phase, toneBad)
	default:
		o.add("Phase", orDefault(st.Phase, "Unknown"), toneWarn)
	}
	if st.Reason != "" {
		o.add("Reason", st.Reason, toneBad)
	}
	readyTone := toneOK
	if total > 0 && ready < total && st.Phase != "Succeeded" {
		readyTone = toneWarn
	}
	if st.Phase == "Succeeded" {
		readyTone = toneDone
	}
	o.add("Ready", fmt.Sprintf("%d/%d", ready, total), readyTone)
	if restarts > 0 {
		// Restarts are a warning, never a red: a pod that crashed an hour ago and has
		// been up since is not broken now, and the container rows below say whether it
		// still is. The number changing is what the canvas highlights.
		o.add("Restarts", strconv.Itoa(restarts), toneWarn)
	}
	o.add("Node", spec.NodeName, "")
	o.add("Pod IP", st.PodIP, "")

	// One row per container that is not simply running, named so the row *is* the answer —
	// and, in the same pass, the container list the log picker is built from.
	for _, c := range st.InitContainerSt {
		o.Containers = append(o.Containers, k3dStateContainer{
			Name: c.Name, Init: true, Tone: k3dContainerTone(c), Restarts: c.RestartCount})
	}
	for _, c := range st.ContainerStatuses {
		o.Containers = append(o.Containers, k3dStateContainer{
			Name: c.Name, Tone: k3dContainerTone(c), Restarts: c.RestartCount})
	}
	for _, c := range append(append([]k3dContainerStatus{}, st.InitContainerSt...), st.ContainerStatuses...) {
		switch {
		case c.State.Waiting != nil:
			r := orDefault(c.State.Waiting.Reason, "Waiting")
			tone := toneWarn
			if k3dBadWaitReasons[r] {
				tone = toneBad
			}
			o.add(c.Name, r, tone)
		case c.State.Terminated != nil:
			t := c.State.Terminated
			tone := toneDone
			if t.ExitCode != 0 {
				tone = toneBad
			}
			o.add(c.Name, fmt.Sprintf("%s (exit %d)", orDefault(t.Reason, "Terminated"), t.ExitCode), tone)
		case c.State.Running != nil && !c.Ready:
			// Running but failing its readiness probe — the state a database pod sits in
			// while it recovers, and the one people watch for.
			o.add(c.Name, "Running, not ready", toneWarn)
		}
	}
	// A pod that is not scheduled says why in its conditions, and "Insufficient memory"
	// is the answer to a question people otherwise spend ten minutes on.
	for _, c := range st.Conditions {
		if c.Type == "PodScheduled" && c.Status != "True" {
			o.add("Unschedulable", strings.TrimSpace(c.Reason+" "+c.Message), toneBad)
		}
	}
}

// ---------------------------------------------------------------- workloads

func k3dWorkloadState(o *k3dStateObj, it k3dRawItem) {
	var spec struct {
		Replicas *int `json:"replicas"`
	}
	json.Unmarshal(it.Spec, &spec)
	var st struct {
		Replicas          int `json:"replicas"`
		ReadyReplicas     int `json:"readyReplicas"`
		AvailableReplicas int `json:"availableReplicas"`
		UpdatedReplicas   int `json:"updatedReplicas"`
		CurrentReplicas   int `json:"currentReplicas"`
	}
	json.Unmarshal(it.Status, &st)

	want := st.Replicas
	if spec.Replicas != nil {
		want = *spec.Replicas
	}
	o.Summary = fmt.Sprintf("%d/%d ready", st.ReadyReplicas, want)

	tone := toneOK
	switch {
	case want == 0:
		tone = toneDone // scaled to zero on purpose — paused, not broken
		o.Summary = "scaled to 0"
	case st.ReadyReplicas == 0:
		tone = toneBad
	case st.ReadyReplicas < want:
		tone = toneWarn
	}
	o.add("Ready", fmt.Sprintf("%d/%d", st.ReadyReplicas, want), tone)
	if st.UpdatedReplicas > 0 && st.UpdatedReplicas < want {
		// Mid-rollout: the interesting number during an operator's rolling restart.
		o.add("Updated", fmt.Sprintf("%d/%d", st.UpdatedReplicas, want), toneWarn)
	}
	if st.AvailableReplicas != st.ReadyReplicas {
		o.add("Available", strconv.Itoa(st.AvailableReplicas), toneWarn)
	}
}

// ---------------------------------------------------------------- jobs

func k3dJobState(o *k3dStateObj, it k3dRawItem) {
	var spec struct {
		Completions *int `json:"completions"`
	}
	json.Unmarshal(it.Spec, &spec)
	var st struct {
		Active         int    `json:"active"`
		Succeeded      int    `json:"succeeded"`
		Failed         int    `json:"failed"`
		StartTime      string `json:"startTime"`
		CompletionTime string `json:"completionTime"`
	}
	json.Unmarshal(it.Status, &st)

	want := 1
	if spec.Completions != nil {
		want = *spec.Completions
	}
	switch {
	case st.Failed > 0:
		o.Summary = fmt.Sprintf("%d failed", st.Failed)
		o.add("Failed", strconv.Itoa(st.Failed), toneBad)
	case st.Succeeded >= want:
		o.Summary = "Complete"
		o.add("Succeeded", fmt.Sprintf("%d/%d", st.Succeeded, want), toneDone)
	default:
		o.Summary = fmt.Sprintf("%d active", st.Active)
		o.add("Active", strconv.Itoa(st.Active), toneWarn)
	}
	o.add("Started", st.StartTime, "")
	o.add("Completed", st.CompletionTime, "")
}

// ---------------------------------------------------------------- claims

func k3dClaimState(o *k3dStateObj, it k3dRawItem) {
	var spec struct {
		StorageClassName *string `json:"storageClassName"`
		VolumeName       string  `json:"volumeName"`
	}
	json.Unmarshal(it.Spec, &spec)
	var st struct {
		Phase    string            `json:"phase"`
		Capacity map[string]string `json:"capacity"`
	}
	json.Unmarshal(it.Status, &st)

	o.Summary = orDefault(st.Phase, "Unknown")
	switch st.Phase {
	case "Bound":
		o.add("Phase", "Bound", toneOK)
	case "Pending":
		// A claim nothing has satisfied is why a StatefulSet is stuck, every time.
		o.add("Phase", "Pending", toneWarn)
	case "Lost":
		o.add("Phase", "Lost", toneBad)
	default:
		o.add("Phase", orDefault(st.Phase, "Unknown"), toneWarn)
	}
	o.add("Capacity", st.Capacity["storage"], "")
	if spec.StorageClassName != nil {
		o.add("Storage class", *spec.StorageClassName, "")
	}
	o.add("Volume", spec.VolumeName, "")
}

// k3dVolumeState reads a PersistentVolume. Its phase is the other half of a claim's: a claim
// that is Pending and a volume that is Released are the same outage seen from two ends, and
// only one of them is visible from the claim.
func k3dVolumeState(o *k3dStateObj, it k3dRawItem) {
	var spec struct {
		Capacity      map[string]string `json:"capacity"`
		StorageClass  string            `json:"storageClassName"`
		ReclaimPolicy string            `json:"persistentVolumeReclaimPolicy"`
		ClaimRef      struct {
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
		} `json:"claimRef"`
	}
	json.Unmarshal(it.Spec, &spec)
	var st struct {
		Phase   string `json:"phase"`
		Reason  string `json:"reason"`
		Message string `json:"message"`
	}
	json.Unmarshal(it.Status, &st)

	o.Summary = orDefault(st.Phase, "Unknown")
	switch st.Phase {
	case "Bound", "Available":
		o.add("Phase", st.Phase, toneOK)
	case "Released":
		// The claim is gone and the data is still here: the state somebody is looking for
		// after a StatefulSet was deleted and recreated.
		o.add("Phase", "Released", toneWarn)
	case "Failed":
		o.add("Phase", "Failed", toneBad)
	default:
		o.add("Phase", orDefault(st.Phase, "Unknown"), toneWarn)
	}
	o.add("Reason", strings.TrimSpace(st.Reason+" "+st.Message), toneBad)
	o.add("Capacity", spec.Capacity["storage"], "")
	o.add("Storage class", spec.StorageClass, "")
	o.add("Reclaim", spec.ReclaimPolicy, "")
	if spec.ClaimRef.Name != "" {
		o.add("Claimed by", spec.ClaimRef.Namespace+"/"+spec.ClaimRef.Name, "")
	}
}

// k3dCronJobState reads a CronJob — the operators' scheduled backups, among other things.
// A suspended schedule is the answer to "why has there been no backup since Tuesday".
func k3dCronJobState(o *k3dStateObj, it k3dRawItem) {
	var spec struct {
		Schedule string `json:"schedule"`
		Suspend  *bool  `json:"suspend"`
	}
	json.Unmarshal(it.Spec, &spec)
	var st struct {
		Active           []any  `json:"active"`
		LastScheduleTime string `json:"lastScheduleTime"`
		LastSuccessful   string `json:"lastSuccessfulTime"`
	}
	json.Unmarshal(it.Status, &st)

	suspended := spec.Suspend != nil && *spec.Suspend
	switch {
	case suspended:
		o.Summary = "suspended"
		o.add("Suspended", "true", toneWarn)
	case len(st.Active) > 0:
		o.Summary = fmt.Sprintf("%d running", len(st.Active))
		o.add("Active", strconv.Itoa(len(st.Active)), toneWarn)
	default:
		o.Summary = orDefault(spec.Schedule, "scheduled")
		o.add("Schedule", spec.Schedule, toneOK)
	}
	o.add("Schedule", spec.Schedule, "")
	o.add("Last scheduled", st.LastScheduleTime, "")
	o.add("Last successful", st.LastSuccessful, "")
}

// k3dBudgetState reads a PodDisruptionBudget. `disruptionsAllowed: 0` is why an operator's
// rolling restart is sitting there doing nothing, and it is invisible from every other card.
func k3dBudgetState(o *k3dStateObj, it k3dRawItem) {
	var st struct {
		CurrentHealthy     int `json:"currentHealthy"`
		DesiredHealthy     int `json:"desiredHealthy"`
		ExpectedPods       int `json:"expectedPods"`
		DisruptionsAllowed int `json:"disruptionsAllowed"`
	}
	json.Unmarshal(it.Status, &st)

	o.Summary = fmt.Sprintf("%d/%d healthy", st.CurrentHealthy, st.DesiredHealthy)
	tone := toneOK
	if st.CurrentHealthy < st.DesiredHealthy {
		tone = toneBad
	}
	o.add("Healthy", fmt.Sprintf("%d/%d", st.CurrentHealthy, st.DesiredHealthy), tone)
	if st.DisruptionsAllowed == 0 {
		o.add("Disruptions allowed", "0", toneWarn)
	} else {
		o.add("Disruptions allowed", strconv.Itoa(st.DisruptionsAllowed), toneOK)
	}
	o.add("Expected pods", strconv.Itoa(st.ExpectedPods), "")
}

// k3dDataState reads a ConfigMap or a Secret — objects a capture contains and a cluster has
// plenty of. They have no status at all, so the card is an inventory entry: how many keys,
// and for a Secret what type it is. NEVER a value, and never even a key name: the Secrets
// editor (k3dobjects.go) is where those are read, with the rules that file argues for.
func k3dDataState(o *k3dStateObj, it k3dRawItem) {
	var raw struct {
		Type       string          `json:"type"`
		Data       map[string]any  `json:"data"`
		BinaryData map[string]any  `json:"binaryData"`
		Immutable  *bool           `json:"immutable"`
		Spec       json.RawMessage `json:"-"`
	}
	// data/type sit at the top level of the object rather than under spec or status, so the
	// raw item's Spec/Status are no use here — decode the object again.
	json.Unmarshal(it.Raw, &raw)
	n := len(raw.Data) + len(raw.BinaryData)
	o.Summary = fmt.Sprintf("%d key%s", n, map[bool]string{true: "", false: "s"}[n == 1])
	o.add("Type", raw.Type, "")
	o.add("Keys", strconv.Itoa(n), "")
	if raw.Immutable != nil && *raw.Immutable {
		o.add("Immutable", "true", "")
	}
}

// ---------------------------------------------------------------- services

func k3dServiceState(o *k3dStateObj, it k3dRawItem) {
	var spec struct {
		Type      string `json:"type"`
		ClusterIP string `json:"clusterIP"`
		Ports     []struct {
			Port     int    `json:"port"`
			NodePort int    `json:"nodePort"`
			Protocol string `json:"protocol"`
		} `json:"ports"`
	}
	json.Unmarshal(it.Spec, &spec)
	var st struct {
		LoadBalancer struct {
			Ingress []struct {
				IP       string `json:"ip"`
				Hostname string `json:"hostname"`
			} `json:"ingress"`
		} `json:"loadBalancer"`
	}
	json.Unmarshal(it.Status, &st)

	o.Summary = orDefault(spec.Type, "ClusterIP")
	o.add("Type", spec.Type, "")
	o.add("Cluster IP", spec.ClusterIP, "")
	if spec.Type == "LoadBalancer" {
		ext := ""
		for _, in := range st.LoadBalancer.Ingress {
			ext = orDefault(in.IP, in.Hostname)
			break
		}
		if ext == "" {
			// MetalLB is installed on these clusters (k3d.go), so a LoadBalancer with no
			// address is a pool that ran out or a speaker that is down — not normal.
			o.add("External IP", "pending", toneWarn)
		} else {
			o.add("External IP", ext, toneOK)
			o.Summary = ext
		}
	}
	var ports []string
	for _, p := range spec.Ports {
		s := strconv.Itoa(p.Port)
		if p.NodePort > 0 {
			s += ":" + strconv.Itoa(p.NodePort)
		}
		ports = append(ports, s)
	}
	o.add("Ports", strings.Join(ports, " "), "")
}

// ---------------------------------------------------------------- nodes

func k3dNodeState(o *k3dStateObj, it k3dRawItem) {
	var spec struct {
		Unschedulable bool `json:"unschedulable"`
	}
	json.Unmarshal(it.Spec, &spec)
	var st struct {
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
		NodeInfo struct {
			KubeletVersion string `json:"kubeletVersion"`
		} `json:"nodeInfo"`
	} // the two halves of a node worth watching: is it ready, and is it under pressure
	json.Unmarshal(it.Status, &st)

	ready := ""
	for _, c := range st.Conditions {
		switch c.Type {
		case "Ready":
			ready = c.Status
			if c.Status == "True" {
				o.add("Ready", "True", toneOK)
			} else {
				o.add("Ready", orDefault(c.Reason, c.Status), toneBad)
			}
		case "MemoryPressure", "DiskPressure", "PIDPressure", "NetworkUnavailable":
			// These are False on a healthy node, so only a True is worth a row — and a
			// node under disk pressure is the answer to "why did the operator evict it".
			if c.Status == "True" {
				o.add(c.Type, "True", toneBad)
			}
		}
	}
	o.Summary = "Ready"
	if ready != "True" {
		o.Summary = "NotReady"
	}
	if spec.Unschedulable {
		o.add("Scheduling", "cordoned", toneWarn)
		o.Summary += ", cordoned"
	}
	o.add("Kubelet", st.NodeInfo.KubeletVersion, "")
}

// ---------------------------------------------------------------- custom resources

// k3dStatelessKinds are the kinds that have no `status` by definition. A capture contains
// plenty of them (pt-k8s-debug-collector writes every resource the API server serves), and
// they are inventory rather than health: the card says what the object is, in one line, and
// nothing about it is alarming.
var k3dStatelessKinds = map[string]bool{
	"configmap": true, "secret": true, "serviceaccount": true, "storageclass": true,
	"role": true, "rolebinding": true, "clusterrole": true, "clusterrolebinding": true,
	"priorityclass": true, "runtimeclass": true, "ingressclass": true, "endpoints": true,
	"endpointslice": true, "limitrange": true, "resourcequota": true, "podtemplate": true,
	"controllerrevision": true, "customresourcedefinition": true, "componentstatus": true,
	"volumeattachment": true, "csidriver": true, "csinode": true, "networkpolicy": true,
	"mutatingwebhookconfiguration": true, "validatingwebhookconfiguration": true,
	"apiservice": true, "lease": true, "namespace": true, "event": true,
}

// k3dStateWords maps the words a status field uses to a tone. Every operator spells its
// state differently and none of them document it as an enum, so this is a vocabulary
// rather than a schema: what is not in it is shown without a colour, which is the honest
// answer for a word this file has never seen.
var k3dStateWords = map[string]string{
	"ready": toneOK, "running": toneOK, "succeeded": toneOK, "bound": toneOK,
	"applied": toneOK, "complete": toneDone, "completed": toneDone, "succeeded_": toneDone,
	"initializing": toneWarn, "starting": toneWarn, "pending": toneWarn, "creating": toneWarn,
	"paused": toneWarn, "stopping": toneWarn, "restoring": toneWarn, "backup": toneWarn,
	"unknown": toneWarn, "waiting": toneWarn, "requested": toneWarn,
	"error": toneBad, "failed": toneBad, "failing": toneBad, "crashed": toneBad, "degraded": toneBad,
}

// k3dWordTone tones a status word, case-insensitively. "" when the word means nothing to
// us — the row is still shown, just uncoloured.
func k3dWordTone(s string) string {
	return k3dStateWords[strings.ToLower(strings.TrimSpace(s))]
}

// k3dIsStateKey reports whether a status field's NAME says its value is a state, which is
// the only thing that earns a value a colour.
//
// This exists because the obvious rule — tone any value that looks like a state word — is
// wrong on a real cluster, and was: a PerconaXtraDBClusterBackup carries `s3.bucket`, the
// bucket in this lab is called "backup", "backup" is in the vocabulary, and a finished
// backup came out amber because of the name of its bucket. A word only means a state when
// it is the value of something that holds a state.
func k3dIsStateKey(key string) bool {
	k := strings.ToLower(key)
	if i := strings.LastIndex(k, "."); i >= 0 {
		k = k[i+1:]
	}
	switch k {
	case "state", "status", "phase":
		return true
	}
	return false
}

// k3dGenericState reads any object whose kind has no rule of its own — every custom
// resource, and any built-in kind added to the sample later.
//
// It reads the status the way a person does: find the word that says what state it is in
// (`state`, `status`, `phase`), then the scalars beside it, then any condition that is not
// True. That covers all four Percona operators (`status.state` is "ready" / "initializing"
// / "error"), CloudNativePG (`status.phase`), and Crunchy's postgresclusters (conditions),
// without a schema for any of them.
func k3dGenericState(o *k3dStateObj, it k3dRawItem) {
	var st map[string]any
	if json.Unmarshal(it.Status, &st); st == nil {
		// No status at all. For a custom resource that is worth saying — the operator has
		// not touched it yet. For the many kinds that simply do not have one (a Role, a
		// StorageClass, a ServiceAccount — all of which a cluster-dump contains), it is
		// normal, and a board of amber cards saying "no status" about objects that never
		// have one would be noise that teaches people to ignore amber.
		if k3dStatelessKinds[strings.ToLower(it.Kind)] {
			o.Summary = "—"
			return
		}
		o.Summary = "no status"
		o.Tone = toneWarn
		return
	}
	// The headline word.
	for _, key := range []string{"state", "status", "phase"} {
		s, ok := st[key].(string)
		if !ok || s == "" {
			continue
		}
		o.Summary = s
		tone := k3dWordTone(s)
		if tone == "" {
			tone = toneOK
		}
		o.add(strings.ToUpper(key[:1])+key[1:], s, tone)
		break
	}
	// The scalars beside it, in a stable order. Nested objects are flattened one level —
	// a Percona cluster's status is `{postgres: {ready: 3, size: 3}, pgbouncer: {…}}`, and
	// "postgres.ready 3" is exactly the row somebody watches during a failover.
	for _, k := range sortedAnyKeys(st) {
		switch k {
		case "state", "status", "phase", "conditions", "observedGeneration", "managedFields":
			continue
		}
		tone := func(key, val string) string {
			if k3dIsStateKey(key) {
				return k3dWordTone(val)
			}
			return ""
		}
		switch v := st[k].(type) {
		case string, float64, bool:
			o.add(k, k3dScalarString(v), tone(k, k3dScalarString(v)))
		case map[string]any:
			for _, k2 := range sortedAnyKeys(v) {
				switch v2 := v[k2].(type) {
				case string, float64, bool:
					o.add(k+"."+k2, k3dScalarString(v2), tone(k2, k3dScalarString(v2)))
				}
			}
		}
	}
	// Conditions, which need care. The textbook Kubernetes condition is True/False/Unknown
	// and "not True" means unhappy — but the Percona operators use the same list as a
	// TRANSITION LOG with arbitrary words in `status`, and both halves of that broke the
	// first version of this rule on a live cluster: `{type: tls, status: enabled}` came out
	// amber because "enabled" is not "True", and a stale `{type: initializing, status:
	// True}` sits there for the life of a ready cluster.
	//
	// So: True is skipped (the headline state is the current answer, not a log of how it got
	// there), False is judged by what the condition is about, and anything else is judged by
	// the word — which for a word this file does not know means no colour at all.
	if conds, ok := st["conditions"].([]any); ok {
		for _, c := range conds {
			m, ok := c.(map[string]any)
			if !ok {
				continue
			}
			typ, _ := m["type"].(string)
			status, _ := m["status"].(string)
			reason, _ := m["reason"].(string)
			if typ == "" || status == "True" {
				continue
			}
			tone := k3dWordTone(status)
			if status == "False" {
				tone = toneWarn
				// A ready-ish condition that is False is the object saying it is not working.
				if lt := strings.ToLower(typ); strings.Contains(lt, "ready") || strings.Contains(lt, "available") || strings.Contains(lt, "healthy") {
					tone = toneBad
				}
			}
			o.add(typ, strings.TrimSpace(status+" "+reason), tone)
		}
	}
	if o.Summary == "" {
		o.Summary = fmt.Sprintf("%d fields", len(o.Props))
	}
}

func k3dScalarString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

func sortedAnyKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- events

// k3dAttachEvents hangs each object's recent Warning events off it, newest first.
//
// Warnings only, and capped: a busy cluster produces thousands of Normal events that say
// nothing a status field does not already say, and the detail pane is for the handful that
// explain a red card — "Back-off pulling image", "0/1 nodes are available", "Liveness probe
// failed". They never change a tone (see rule 1 at the top of this file).
func k3dAttachEvents(objs []k3dStateObj, doc []byte, max int) {
	if len(doc) == 0 {
		return
	}
	var list struct {
		Items []struct {
			Type           string `json:"type"`
			Reason         string `json:"reason"`
			Message        string `json:"message"`
			Count          int    `json:"count"`
			LastTimestamp  string `json:"lastTimestamp"`
			EventTime      string `json:"eventTime"`
			InvolvedObject struct {
				UID       string `json:"uid"`
				Kind      string `json:"kind"`
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"involvedObject"`
		} `json:"items"`
	}
	if json.Unmarshal(doc, &list) != nil {
		return
	}
	byUID := map[string][]k3dStateEvent{}
	byName := map[string][]k3dStateEvent{}
	for _, e := range list.Items {
		if e.Type != "Warning" {
			continue
		}
		ev := k3dStateEvent{Reason: e.Reason, Message: e.Message, Count: e.Count,
			At: orDefault(e.LastTimestamp, e.EventTime)}
		if e.InvolvedObject.UID != "" {
			byUID[e.InvolvedObject.UID] = append(byUID[e.InvolvedObject.UID], ev)
		}
		key := strings.ToLower(e.InvolvedObject.Kind) + "/" + e.InvolvedObject.Namespace + "/" + e.InvolvedObject.Name
		byName[key] = append(byName[key], ev)
	}
	newest := func(evs []k3dStateEvent) []k3dStateEvent {
		sort.SliceStable(evs, func(i, j int) bool { return evs[i].At > evs[j].At })
		if len(evs) > max {
			evs = evs[:max]
		}
		return evs
	}
	for i := range objs {
		o := &objs[i]
		if evs := byUID[o.UID]; len(evs) > 0 {
			o.Events = newest(evs)
			continue
		}
		// A UID the events do not carry (or a fixture without one): fall back to the
		// object's address, which is what `kubectl describe` matches on anyway.
		key := strings.ToLower(o.Kind) + "/" + o.Namespace + "/" + o.Name
		if evs := byName[key]; len(evs) > 0 {
			o.Events = newest(evs)
		}
	}
}

// ---------------------------------------------------------------- handlers

// k3dStateMaxEvents is how many warnings one object keeps. Five is a detail pane, not a
// log: the log is `kubectl -n ns describe`, and the console next door opens it.
const k3dStateMaxEvents = 5

// handleK3DStates samples one cluster: every built-in kind on the canvas, the operator's
// custom resources when it has any, and the warning events that explain them.
//
// Three kubectl calls, and only the first is required. A cluster with no operator has no
// custom resources to list and the call fails; a cluster whose event TTL has expired has
// no events. Neither is a reason to show nothing, so both are reported in `warnings` and
// the sample goes out with what it has.
func (a *App) handleK3DStates(w http.ResponseWriter, r *http.Request) {
	_, _, dep, ok := a.k3dRBACContext(w, r)
	if !ok {
		return
	}
	var cfg k3dConfig
	json.Unmarshal(dep.Config, &cfg)

	ctx := r.Context()
	core, err := a.kubectl(ctx, dep.ContainerID, "get", strings.Join(k3dStateKinds, ","), "-A", "-o", "json")
	if err != nil {
		writeErr(w, http.StatusBadGateway, "read the cluster: "+lastLines(err.Error(), 200))
		return
	}
	docs := [][]byte{[]byte(core)}
	var warnings []string

	if crs := k3dStateCRs[cfg.Operator]; len(crs) > 0 {
		out, err := a.kubectl(ctx, dep.ContainerID, "get", strings.Join(crs, ","), "-A", "-o", "json")
		if err != nil {
			warnings = append(warnings, "custom resources: "+lastLines(err.Error(), 120))
		} else {
			docs = append(docs, []byte(out))
		}
	}
	objs := parseK3DStates(docs...)

	if out, err := a.kubectl(ctx, dep.ContainerID, "get", "events", "-A", "-o", "json"); err != nil {
		warnings = append(warnings, "events: "+lastLines(err.Error(), 120))
	} else {
		k3dAttachEvents(objs, []byte(out), k3dStateMaxEvents)
	}

	namespaces := map[string]bool{}
	kinds := map[string]bool{}
	for _, o := range objs {
		if o.Namespace != "" {
			namespaces[o.Namespace] = true
		}
		kinds[o.Kind] = true
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"objects":          objs,
		"namespaces":       sortedBoolKeys(namespaces),
		"kinds":            sortedBoolKeys(kinds),
		"clusterNamespace": cfg.Namespace,
		"operator":         cfg.Operator,
		"capturedAt":       time.Now().UTC().Format(time.RFC3339),
		"warnings":         warnings,
	})
}

func sortedBoolKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// k3dStateTarget is one cluster the canvas can watch.
type k3dStateTarget struct {
	StackID   int64  `json:"stackId"`
	StackName string `json:"stackName"`
	FrameID   string `json:"frameId"`
	Label     string `json:"label"`
	Operator  string `json:"operator,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

// handleK3DStateTargets lists every running K3D cluster the caller can see, so the page
// opens with a picker rather than asking somebody to find a stack id.
//
// Unlike the debugger's target list next door (k3ddebugapi.go), there is no gate: any
// running cluster can be watched, because watching it is a read of its own API server and
// needs nothing to have been deployed specially.
func (a *App) handleK3DStateTargets(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	out := []k3dStateTarget{}
	stacks, _ := a.store.ListStacks(u.ID, u.Role == RoleAdmin)
	for _, s := range stacks {
		st, err := a.store.GetStack(s.ID)
		if err != nil {
			continue
		}
		var doc designDoc
		if json.Unmarshal(st.Design, &doc) != nil {
			continue
		}
		for _, f := range doc.Frames {
			if f.Type != "k3d" {
				continue
			}
			_, dep, err := a.k3dFrameAndServer(st, doc, f.ID)
			if err != nil {
				continue // not running: nothing to watch
			}
			var cfg k3dConfig
			json.Unmarshal(dep.Config, &cfg)
			out = append(out, k3dStateTarget{
				StackID: st.ID, StackName: st.Name, FrameID: f.ID,
				Label: orDefault(f.Label, f.ID), Operator: cfg.Operator, Namespace: cfg.Namespace,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StackName != out[j].StackName {
			return out[i].StackName < out[j].StackName
		}
		return out[i].Label < out[j].Label
	})
	writeJSON(w, http.StatusOK, map[string]any{"targets": out})
}

// ---------------------------------------------------------------- logs and YAML

// A pinned pane is either an object's log or its manifest, and both are one kubectl away.
// They are here rather than in a general "run kubectl" endpoint for the reason the rest of
// this app avoids one: every argument is checked against what it is allowed to be, and the
// two shapes below are all the canvas asks for.
//
// Checking matters more than it looks. These are argv, not a shell, so there is no quoting
// to get wrong — but an unchecked name of "--all-containers" or "-l app=x" is not a name at
// all, it is a flag, and kubectl would honour it. Every value is matched against a
// Kubernetes name (or, for a kind, a resource name) before it goes anywhere near the command.
var (
	// A Kubernetes object or container name. k3dDNSSubdomain (k3dpods.go) covers the first
	// two; container names are the same shape.
	k3dStateKindRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9.-]{0,62}$`)
)

// k3dLogArgs builds the `kubectl logs` command for one container, checking every value on
// the way. Pure, so the checking is testable: the case that matters is a "name" like
// `--all-containers` or `-l app=x`, which is not a name at all but a flag kubectl would
// honour — argv means there is no quoting to get wrong, and no quoting to save you either.
//
// It returns the tail it settled on as well as the argv, so the response can say how many
// lines were asked for. That is not decoration: the handler used to read a package-level
// `tail` — a helper function in vagrant.go — because the local had been refactored away and
// the name still resolved. It compiled, and `json.Encode` then failed on a func value, so
// every log read came back as HTTP 200 with an empty body and the pane said "No log lines"
// about a pod that was talking. Returning the value removes the name that could be captured.
func k3dLogArgs(namespace, pod, container, tailParam string, previous bool) ([]string, int, error) {
	if !k3dDNSSubdomain.MatchString(namespace) || !k3dDNSSubdomain.MatchString(pod) {
		return nil, 0, fmt.Errorf("namespace and name must be Kubernetes names")
	}
	if container != "" && !k3dDNSSubdomain.MatchString(container) {
		return nil, 0, fmt.Errorf("container must be a Kubernetes name")
	}
	n := k3dStateTailDefault
	if v, err := strconv.Atoi(strings.TrimSpace(tailParam)); err == nil && v > 0 {
		n = min(v, k3dStateTailMax)
	}
	args := []string{"-n", namespace, "logs", pod, "--tail", strconv.Itoa(n), "--timestamps"}
	if container != "" {
		args = append(args, "-c", container)
	}
	if previous {
		args = append(args, "--previous")
	}
	return args, n, nil
}

// k3dManifestArgs builds the `kubectl get -o yaml` command for one object. An empty
// namespace is the cluster-scoped case (a Node) and means the flag is left off entirely —
// `-n ""` is not the same thing and would fail.
func k3dManifestArgs(kind, namespace, name string) ([]string, error) {
	if !k3dStateKindRe.MatchString(kind) {
		return nil, fmt.Errorf("kind must be a Kubernetes kind")
	}
	if !k3dDNSSubdomain.MatchString(name) {
		return nil, fmt.Errorf("name must be a Kubernetes name")
	}
	args := []string{"get", kind, name, "-o", "yaml"}
	if namespace == "" {
		return args, nil
	}
	if !k3dDNSSubdomain.MatchString(namespace) {
		return nil, fmt.Errorf("namespace must be a Kubernetes name")
	}
	return append([]string{"-n", namespace}, args...), nil
}

// k3dStateLogCap is the most log text one request returns. A crash-looping database can
// produce megabytes a minute and the pane is a tail, not an archive: the console on the
// cluster's node is where the whole thing lives.
const (
	k3dStateLogCap      = 512 << 10 // 512 KiB of text
	k3dStateTailDefault = 200
	k3dStateTailMax     = 5000
)

// handleK3DStateLogs tails one container of one pod.
//
// `previous` is the option that earns this endpoint its place: a container in
// CrashLoopBackOff has nothing useful in its current log (it has barely started), and
// everything useful in the log of the run that died. That is one checkbox here and a thing
// people forget `kubectl logs` can do.
func (a *App) handleK3DStateLogs(w http.ResponseWriter, r *http.Request) {
	_, _, dep, ok := a.k3dRBACContext(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	ns := strings.TrimSpace(q.Get("namespace"))
	pod := strings.TrimSpace(q.Get("name"))
	container := strings.TrimSpace(q.Get("container"))
	args, nLines, err := k3dLogArgs(ns, pod, container, q.Get("tail"), q.Get("previous") == "1")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// NOT a.kubectl: it returns stdout alone on success, and `kubectl logs` says the most
	// important things on stderr WITH A ZERO EXIT. Verified against a real crash-looping
	// pod: `--previous` on a container whose dead instance containerd has already collected
	// prints "unable to retrieve container logs for containerd://…" on stderr and exits 0,
	// so a.kubectl would hand back an empty string and the pane would say "No log lines" —
	// which is not what happened. `kubectl logs` on a multi-container pod with no -c does
	// the same with "Defaulted container …". Both are worth showing, neither is an error,
	// so stdout is the log and stderr is a note beside it.
	out, note, code, err := a.kubectlOut(r.Context(), dep.ContainerID, args...)
	if err != nil || code != 0 {
		// Still not a server error: "previous terminated container not found" is the normal
		// answer for a container that has never restarted, and the pane should say so
		// rather than show an error box.
		msg := strings.TrimSpace(note + "\n" + out)
		if err != nil && msg == "" {
			msg = err.Error()
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"namespace": ns, "name": pod, "container": container,
			"text": "", "error": lastLines(msg, 300),
		})
		return
	}
	truncated := false
	if len(out) > k3dStateLogCap {
		// Keep the END: a tail that drops its newest lines is not a tail.
		out = out[len(out)-k3dStateLogCap:]
		if i := strings.IndexByte(out, '\n'); i >= 0 {
			out = out[i+1:]
		}
		truncated = true
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"namespace": ns, "name": pod, "container": container,
		"text": out, "truncated": truncated, "tail": nLines,
		"note":     lastLines(strings.TrimSpace(note), 300),
		"previous": q.Get("previous") == "1",
		"readAt":   time.Now().UTC().Format(time.RFC3339),
	})
}

// kubectlOut runs kubectl and keeps stdout and stderr apart, which a.kubectl (k3d.go) does
// not: it returns stdout on success and the two concatenated on failure. That is right for
// a command whose output is data and whose stderr is a failure — and wrong for `kubectl
// logs`, where stderr carries notes about a perfectly successful read.
func (a *App) kubectlOut(ctx context.Context, serverID string, args ...string) (stdout, stderr string, code int, err error) {
	res, err := a.engCtx(ctx).Exec(ctx, serverID, append([]string{"kubectl"}, args...),
		[]string{"KUBECONFIG=" + k3dKubeconfig})
	if err != nil {
		return "", "", 0, err
	}
	return res.Stdout, res.Stderr, res.Code, nil
}

// handleK3DStateManifest returns one object as YAML — what `kubectl get … -o yaml` prints,
// which since Kubernetes 1.21 leaves managedFields out, so it is the object rather than a
// page of apply bookkeeping.
//
// Read-only on purpose. The canvas is a monitor; the custom resource editor (k3dcrform.go)
// and the Secrets editor (k3dobjects.go) are where a cluster is changed, and both do a
// server-side dry run first. A YAML text box that applied would be neither.
func (a *App) handleK3DStateManifest(w http.ResponseWriter, r *http.Request) {
	_, _, dep, ok := a.k3dRBACContext(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	kind := strings.TrimSpace(q.Get("kind"))
	ns := strings.TrimSpace(q.Get("namespace"))
	name := strings.TrimSpace(q.Get("name"))
	args, err := k3dManifestArgs(kind, ns, name)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := a.kubectl(r.Context(), dep.ContainerID, args...)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "read "+kind+"/"+name+": "+lastLines(err.Error(), 300))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind": kind, "namespace": ns, "name": name, "yaml": out,
		"readAt": time.Now().UTC().Format(time.RFC3339),
	})
}
