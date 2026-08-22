package main

// logsummary_k8sevents.go — the Kubernetes Events of a cluster's namespace, as a source.
//
// The last of the four things this feature kept deferring, and the reason it kept coming
// back: **the answer to "why did that member restart" is not in any log.** A PXC member
// cut off from its cluster does the right thing — goes non-primary and waits — and is
// killed twenty-five seconds later by its liveness probe. Its own log records that as
// `Received SHUTDOWN from user <via user signal>`, which is byte-for-byte what a deliberate
// stop writes; the operator's log says nothing at all. The reason exists in exactly one
// place:
//
//	Warning  Unhealthy  Liveness probe failed: + [[ -n non-Primary ]]…
//	Normal   Killing    Container pxc failed liveness probe, will be restarted
//
// which is an API object, not a file. So this source is not a log at all — it is
// `kubectl get events -o json`, folded into the same records everything else here becomes.
//
// Three things about Events that shape the code, and all three are why they are worth
// reading beside the logs rather than instead of them:
//
//  1. **They expire.** The default TTL is one hour. An incident investigated the next
//     morning has no Events at all, and their absence is not evidence of a quiet night.
//     The collector says so rather than returning an empty source.
//  2. **They are counted, not repeated.** One object carries `count` and a first/last
//     timestamp, which is exactly the shape lsEvent.Repeat already has — so a probe that
//     failed forty times is one row with a span, the same as a folded log record.
//  3. **`type` is only Normal or Warning**, and the interesting ones are split across both:
//     `Killing` is Normal. Severity here therefore comes from the REASON, the same way it
//     comes from meaning everywhere else in this package.

import (
	"encoding/json"
	"strings"
	"time"
)

// pktEngineK8sEvents is the engine name for a source that is not a database and not even a
// log: an API object list. It is spelled distinctly so the sources table can say so.
const pktEngineK8sEvents = "k8sevents"

const lsFlavourK8sEvents = "k8sevents"

// lsK8sEventDoc is what `kubectl get events -o json` returns: a List, not a stream.
type lsK8sEventDoc struct {
	Kind  string `json:"kind"`
	Items []struct {
		Type           string `json:"type"`
		Reason         string `json:"reason"`
		Message        string `json:"message"`
		Count          int    `json:"count"`
		FirstTimestamp string `json:"firstTimestamp"`
		LastTimestamp  string `json:"lastTimestamp"`
		EventTime      string `json:"eventTime"`
		InvolvedObject struct {
			Kind      string `json:"kind"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"involvedObject"`
		Source struct {
			Component string `json:"component"`
		} `json:"source"`
	} `json:"items"`
}

// lsSniffK8sEvents recognises the document. It is one JSON object rather than a line
// stream, so nothing else in this package would even parse it.
func lsSniffK8sEvents(data string) bool {
	head := strings.TrimSpace(data)
	if len(head) == 0 || head[0] != '{' {
		return false
	}
	if len(head) > 4096 {
		head = head[:4096]
	}
	return strings.Contains(head, `"involvedObject"`) ||
		(strings.Contains(head, `"kind": "List"`) && strings.Contains(head, `"Event"`))
}

// lsFoldK8sEvents turns the List into records, one per Event.
//
// The line number is the item's index rather than a byte offset: "show this in the file" on
// a pretty-printed JSON document would land on a brace, and the index is the honest answer
// to "which of these is it".
func lsFoldK8sEvents(data []byte) []lsRecord {
	var doc lsK8sEventDoc
	if json.Unmarshal(data, &doc) != nil {
		return nil
	}
	out := make([]lsRecord, 0, len(doc.Items))
	for i, it := range doc.Items {
		obj := it.InvolvedObject.Kind + "/" + it.InvolvedObject.Name
		rec := lsRecord{
			Line: i + 1, Level: strings.ToUpper(it.Type), Subsys: lsSubsysK8sEvent,
			Code: it.Reason, Thread: it.Source.Component, Text: it.Message,
		}
		rec.Body = append(rec.Body, "object: "+obj)
		if it.Source.Component != "" {
			rec.Body = append(rec.Body, "reportedBy: "+it.Source.Component)
		}
		first, last := lsK8sEventTime(it.FirstTimestamp), lsK8sEventTime(it.LastTimestamp)
		if first == 0 {
			first = lsK8sEventTime(it.EventTime)
		}
		if last == 0 {
			last = first
		}
		rec.Time = it.FirstTimestamp
		rec.TS = first
		// The peer is the object the event is about, which is what makes an Events source
		// comparable with the members' own lanes: `Killing` on Pod/cluster1-pxc-2 lines up
		// with cluster1-pxc-2's own log.
		rec.Body = append(rec.Body, "count: "+itoa(it.Count))
		if last > first {
			rec.Body = append(rec.Body, "lastSeen: "+it.LastTimestamp)
		}
		out = append(out, rec)
	}
	return out
}

const lsSubsysK8sEvent = "k8s-event"

func itoa(n int) string {
	if n <= 0 {
		return "1"
	}
	d := ""
	for n > 0 {
		d = string(rune('0'+n%10)) + d
		n /= 10
	}
	return d
}

func lsK8sEventTime(s string) float64 {
	if s == "" {
		return 0
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return float64(t.UnixNano()) / 1e9
	}
	return 0
}

// lsK8sEventRules is the catalogue. Severity comes from the REASON, because Kubernetes
// only has Normal and Warning and puts the most consequential thing it does — killing a
// container — in Normal.
var lsK8sEventRules = []lsRule{
	{
		codes: []string{"Killing"},
		class: lsClassShutdown, sev: lsSevBad, overLevel: true,
		label: "Kubernetes killed a container",
		means: "The reason this exists as a source. A container killed by its liveness probe writes an ordinary shutdown record in its own log — for PXC, `Received SHUTDOWN from user <via user signal>`, which is what a deliberate stop writes — and the operator's log says nothing. This is the only place the cause is recorded, and Kubernetes files it as `Normal`.",
	},
	{
		codes: []string{"Unhealthy"},
		class: lsClassState, sev: lsSevWarn,
		label: "A probe failed",
		means: "A liveness, readiness or startup probe failed, with the probe's own output in the message. Readiness failures take the pod out of its Service; liveness failures get it killed once the failure threshold is reached.",
		enrich: func(r lsRecord, e *lsEvent) {
			switch {
			case strings.Contains(r.Text, "Liveness probe failed"):
				e.Label = "Liveness probe failed"
				e.Sev = lsSevBad
			case strings.Contains(r.Text, "Readiness probe failed"):
				e.Label = "Readiness probe failed"
			case strings.Contains(r.Text, "Startup probe failed"):
				e.Label = "Startup probe failed"
			}
		},
	},
	{
		codes: []string{"BackOff", "CrashLoopBackOff"},
		class: lsClassCrash, sev: lsSevBad,
		label: "Container is crash-looping",
		means: "The container keeps exiting and Kubernetes is backing off between restarts, up to five minutes. Its own log will show as many starts as there were attempts, and nothing about why they are being retried.",
	},
	{
		codes: []string{"FailedScheduling"},
		class: lsClassState, sev: lsSevBad,
		label: "A pod could not be scheduled",
		means: "There is no node this pod can be placed on — anti-affinity, resources, or a volume in the wrong zone. It stays Pending indefinitely and no operator will time it out; a rolling restart that waits for all replicas to be ready therefore waits forever.",
	},
	{
		codes: []string{"Failed", "FailedMount", "FailedAttachVolume", "FailedCreatePodSandBox", "Evicted"},
		class: lsClassState, sev: lsSevBad,
		label: "A pod failed",
		means: "The kubelet could not run the pod — a missing image, a volume it could not attach, or a node that evicted it under pressure.",
	},
	{
		codes: []string{"OOMKilling", "OOMKilled"},
		class: lsClassCrash, sev: lsSevBad,
		label: "A container was killed for using too much memory",
		means: "The cgroup limit was hit. The database's own log usually ends mid-sentence with nothing to say about it, because the process was killed rather than asked to stop.",
	},
	{
		codes: []string{"Preempted", "NodeNotReady", "NodeHasDiskPressure", "NodeHasMemoryPressure"},
		class: lsClassState, sev: lsSevWarn,
		label: "The node had a problem",
		means: "Something happened to the machine rather than to the database. It is the half of an incident that no database log can ever contain.",
	},
	{
		codes: []string{"Created", "Started", "Pulled", "Pulling", "Scheduled", "SuccessfulCreate", "SuccessfulDelete"},
		class: lsClassStartup, sev: lsSevInfo,
		label: "Pod lifecycle",
		means: "The ordinary business of a pod being placed, pulled and started. Useful for dating a restart precisely, which is why it is kept rather than dropped.",
	},
	{
		codes: []string{"LeaderElection"},
		class: lsClassReconcile, sev: lsSevInfo,
		label: "Operator leader election",
		means: "Which operator replica holds the lease. It is about the controller, not the database.",
	},
	{
		codes: []string{"WaitForFirstConsumer", "ExternalProvisioning", "Provisioning", "ProvisioningSucceeded"},
		class: lsClassStorage, sev: lsSevInfo,
		label: "Volume provisioning",
		means: "A PersistentVolumeClaim being satisfied. A claim stuck here is a pod that will never start.",
	},
}

// lsClassifyK8sEvent turns one Event into an event.
//
// The count becomes Repeat and the last-seen time becomes EndTS, so forty probe failures
// are one row with a span — the same shape a folded log record has, and the same reading:
// the span is how long the condition lasted.
func lsClassifyK8sEvent(r lsRecord) (lsEvent, bool) {
	e, keep := lsClassifyOpRecord(r, lsK8sEventRules, nil)
	if !keep {
		return e, keep
	}
	for _, ln := range r.Body {
		if v, ok := strings.CutPrefix(ln, "object: "); ok {
			e.Peer = v
			if _, name, found := strings.Cut(v, "/"); found {
				e.Peer = name
			}
		}
		if v, ok := strings.CutPrefix(ln, "count: "); ok && v != "1" {
			n := 0
			for _, c := range v {
				if c >= '0' && c <= '9' {
					n = n*10 + int(c-'0')
				}
			}
			e.Repeat = n
		}
		if v, ok := strings.CutPrefix(ln, "lastSeen: "); ok {
			if ts := lsK8sEventTime(v); ts > 0 {
				e.EndTS = ts
			}
		}
	}
	// An Event's reason is the stable identifier and its message is free text, so the
	// reason is what a reader filters on.
	if e.Label == "" {
		e.Label = r.Code
	}
	return e, true
}

// lsK8sEventLevelFloor: Kubernetes has two levels and neither is a severity. Warning is
// worth a look; Normal covers everything from a pull to a kill.
func lsResolveK8sEvents(events []lsEvent) {
	for i := range events {
		e := &events[i]
		switch e.Label {
		case "Kubernetes killed a container", "Container is crash-looping",
			"A container was killed for using too much memory":
			e.State = lsStateDown
		}
	}
}
