package main

// opsummary.go — Operator Summary: turn a pt-k8s-debug-collector archive into the
// two things a reader actually needs from it, which are workload health and what
// the operator thinks is wrong.
//
// The archive layout below is not documented anywhere. Percona's docs describe the
// flags and say the output is cluster-dump.tar.gz; the source builds its paths
// through PodResourcePath/PodSummaryPath/DumperLogPath helpers. So this was written
// against a real capture — percona-toolkit 3.6.0's collector run against a k3s
// cluster with a PXC CRD and deliberately broken workloads — rather than a guess:
//
//	cluster-dump/nodes.yaml                          cluster-scoped, a v1 List
//	cluster-dump/errors.txt                          the collector's own failures
//	cluster-dump/<namespace>/<resource>.yaml         pods, deployments, statefulsets,
//	                                                 events, persistentvolumeclaims, …
//	cluster-dump/<namespace>/<plural>.<group>.yaml   operator CRs, e.g.
//	                                                 perconaxtradbclusters.pxc.percona.com.yaml
//	cluster-dump/<namespace>/<pod>/logs.txt          one file per pod, all containers
//
// Every *.yaml is a Kubernetes List: `apiVersion: v1` with an `items:` array. The
// collector writes every namespace-scoped resource under every namespace it saw,
// so most files in a real archive are empty lists — the parser skips those rather
// than reporting a hundred empty namespaces.

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// opArchiveLimit caps how much of an uploaded archive is read into memory. A real
// capture of a busy cluster is a few megabytes, but a long-lived cluster with verbose
// pod logs runs to gigabytes, so this is deliberately generous: it is a guard against
// exhausting the process, not a statement about normal sizes. The archive is held
// uncompressed in memory, so this costs RAM roughly one-for-one.
const opArchiveLimit = 4 << 30

// ---------------------------------------------------------------- the model

// opModel is what the Operator Summary page renders. It mirrors the shape Stalk
// Summary settled on — an availability map so the UI can grey out a panel whose
// input the capture did not contain, findings for the detail, and verdicts for
// the handful of judgements worth leading with.
type opModel struct {
	Source     string `json:"source"`
	CapturedAt string `json:"capturedAt,omitempty"`
	Kubernetes string `json:"kubernetes,omitempty"`
	Collector  string `json:"collector,omitempty"`

	Deployment   *opDeployment  `json:"deployment,omitempty"`
	Images       []opImage      `json:"images"`
	Secrets      []opSecret     `json:"secrets"`
	Backups      []opBackup     `json:"backups"`
	Certs        []opCert       `json:"certs"`
	Storage      []opPVC        `json:"storage"`
	Galera       []opGalera     `json:"galera,omitempty"`
	PodSummaries []opPodSummary `json:"podSummaries,omitempty"`
	BackupLogs   []opBackupLog  `json:"backupLogs,omitempty"`
	Schedules    []opSchedule   `json:"schedules,omitempty"`
	Config       []opConfig     `json:"config,omitempty"`
	Rollouts     []opRollout    `json:"rollouts,omitempty"`
	Budgets      []opBudget     `json:"budgets,omitempty"`
	RBAC         []opRBAC       `json:"rbac,omitempty"`
	Nodes        []opNode       `json:"nodes"`
	Namespaces   []opNamespace  `json:"namespaces"`
	Workloads    []opWorkload   `json:"workloads"`
	Pods         []opPod        `json:"pods"`
	CRs          []opCR         `json:"crs"`
	Operators    []opOperator   `json:"operators"`

	Findings []opFinding `json:"findings"`
	Verdicts []opVerdict `json:"verdicts"`

	// Logs is what Log Summary's classifier made of the archive's log-shaped files
	// — Kubernetes Events, the operator's log, each database's error log. Carried
	// as a summary rather than the whole bundle: the events themselves belong on
	// the Log Summary page, which is built to hold a hundred thousand of them.
	Logs *opLogs `json:"logs,omitempty"`

	CollectorErrors []string        `json:"collectorErrors,omitempty"`
	Available       map[string]bool `json:"available"`

	// logInputs is what was handed to lsBuild, kept so a handler can register the
	// same bundle with Log Summary without re-reading the archive. Unexported: it
	// is the raw log text and has no business in the JSON that goes to a browser.
	logInputs []lsInput
}

// opDeployment is the "what am I actually looking at" header: which operator, at
// which version, managing what. Every field is read from the archive rather than
// assumed — the operator's version is the tag on its own Deployment image, which
// is the only place it is stated.
type opDeployment struct {
	Operator     string `json:"operator,omitempty"`     // pxc | psmdb | pg | ps | pgo | cnpg
	OperatorName string `json:"operatorName,omitempty"` // the image repository
	Version      string `json:"version,omitempty"`      // the image tag
	Namespace    string `json:"namespace,omitempty"`
	Pod          string `json:"pod,omitempty"`
	CRVersion    string `json:"crVersion,omitempty"`
	PMM          string `json:"pmm,omitempty"` // the pmm-client image, when one is wired in
	Kubernetes   string `json:"kubernetes,omitempty"`
}

// opImage is one distinct container image and what runs it. A cluster that is
// half-upgraded, or pinned to a stale build, shows up here and nowhere else.
type opImage struct {
	Image string   `json:"image"`
	Repo  string   `json:"repo"`
	Tag   string   `json:"tag"`
	Used  []string `json:"used"` // namespace/pod entries, capped
	Count int      `json:"count"`
}

// opSecret is a secret this deployment *depends on*. The collector deliberately
// does not collect Secret objects, so this is the reference graph rather than any
// content: which secrets the pods mount and the custom resources name. That is the
// useful half anyway — a missing or renamed secret is a classic operator failure,
// and the value would only be a liability in an archive people email around.
type opSecret struct {
	Name string   `json:"name"`
	Kind string   `json:"kind,omitempty"` // tls | credentials | referenced
	Used []string `json:"used"`
}

// opBackup is one backup or restore custom resource, in flight or finished.
type opBackup struct {
	Kind        string `json:"kind"`
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	Cluster     string `json:"cluster,omitempty"`
	State       string `json:"state,omitempty"`
	Storage     string `json:"storage,omitempty"`
	StorageType string `json:"storageType,omitempty"`
	Destination string `json:"destination,omitempty"`
	Image       string `json:"image,omitempty"`
	Started     string `json:"started,omitempty"`
	Completed   string `json:"completed,omitempty"`
	Error       string `json:"error,omitempty"`
	Restore     bool   `json:"restore,omitempty"`
}

// opCert is a TLS secret's certificate metadata. The collector renders each cert
// as openssl text under the secret's name, which is enough to answer the two
// questions worth asking: who issued it, and when does it expire.
type opCert struct {
	Secret     string `json:"secret"`
	Namespace  string `json:"namespace"`
	Entry      string `json:"entry"` // ca.crt | tls.crt | …
	Subject    string `json:"subject,omitempty"`
	Issuer     string `json:"issuer,omitempty"`
	NotAfter   string `json:"notAfter,omitempty"`
	DaysLeft   *int   `json:"daysLeft,omitempty"`
	SelfSigned bool   `json:"selfSigned,omitempty"`
}

// opPVC is one persistent volume claim — the thing that silently decides whether
// a database can start at all.
type opPVC struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	Status       string `json:"status,omitempty"`
	Capacity     string `json:"capacity,omitempty"`
	Requested    string `json:"requested,omitempty"`
	StorageClass string `json:"storageClass,omitempty"`
	Volume       string `json:"volume,omitempty"`
}

// opLogs is the digest of the log bundle: what was read, what it found, and
// enough of the worst events to be worth reading here rather than clicking
// through. Every rule behind it is Log Summary's, not this file's.
type opLogs struct {
	Sources int `json:"sources"`
	Events  int `json:"events"`
	// BundleID is set once these logs are registered with Log Summary, which is
	// what makes the full timeline one click away. Empty when they were not — the
	// digest still stands on its own.
	BundleID string         `json:"bundleId,omitempty"`
	Findings []opLogFinding `json:"findings,omitempty"`
	Worst    []opLogEvent   `json:"worst,omitempty"`
}

type opLogFinding struct {
	Severity string `json:"severity"`
	Title    string `json:"title"`
	Detail   string `json:"detail,omitempty"`
}

type opLogEvent struct {
	Source   string `json:"source,omitempty"`
	Node     string `json:"node,omitempty"`
	Severity string `json:"severity,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Message  string `json:"message"`
	At       string `json:"at,omitempty"`
}

// opGalera is one PXC member's saved cluster state. Together these decide whether
// a stopped cluster can bootstrap and from which member — `seqno: -1` with
// `safe_to_bootstrap: 0` on every node is the "this cluster will not come back on
// its own" case, and nothing else in the archive says so.
type opGalera struct {
	Namespace       string `json:"namespace"`
	Pod             string `json:"pod"`
	UUID            string `json:"uuid,omitempty"`
	Seqno           string `json:"seqno,omitempty"`
	SafeToBootstrap bool   `json:"safeToBootstrap"`
	HasView         bool   `json:"hasView"`
}

// opPodSummary is the database's own view of itself, from the summary the
// collector takes by port-forwarding into the pod. Everything else in the archive
// is Kubernetes' opinion of the database; this is the database's.
type opPodSummary struct {
	Namespace string            `json:"namespace"`
	Pod       string            `json:"pod"`
	Facts     map[string]string `json:"facts"`
}

// opBackupLog is one xtrabackup log lifted off a member — where a backup says why
// it failed, which no custom resource records.
type opBackupLog struct {
	Namespace string   `json:"namespace"`
	Pod       string   `json:"pod"`
	File      string   `json:"file"`
	Bytes     int      `json:"bytes"`
	Errors    []string `json:"errors,omitempty"`
	Tail      []string `json:"tail,omitempty"`
	// CompletedOK is xtrabackup's own success line. A log with no matched errors
	// is not the same as a log that says it worked.
	CompletedOK bool `json:"completedOk,omitempty"`
}

// opSchedule is a scheduled backup. The runs are in Backups; this is whether one
// is meant to happen at all, which is the question after a backup goes missing.
type opSchedule struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Schedule  string `json:"schedule"`
	Suspended bool   `json:"suspended,omitempty"`
	LastRun   string `json:"lastRun,omitempty"`
	Active    int    `json:"active,omitempty"`
}

// opConfig is a rendered configuration the operator generated — "what is it
// actually running" is a constant question and the answer is in a ConfigMap.
type opConfig struct {
	Namespace string   `json:"namespace"`
	Name      string   `json:"name"`
	Keys      []string `json:"keys"`
	Excerpt   string   `json:"excerpt,omitempty"`
}

// opRollout is a workload's revision history: how many ReplicaSets it has and
// whether an old one still holds pods, which is what a stuck rollout looks like.
type opRollout struct {
	Namespace string `json:"namespace"`
	Owner     string `json:"owner"`
	Revisions int    `json:"revisions"`
	Stuck     int    `json:"stuck,omitempty"` // old revisions still holding pods
}

// opBudget is a PodDisruptionBudget. A wrong one blocks node drains and rolling
// upgrades indefinitely, and gives no other sign that it is doing so.
type opBudget struct {
	Namespace      string `json:"namespace"`
	Name           string `json:"name"`
	MinAvailable   string `json:"minAvailable,omitempty"`
	MaxUnavailable string `json:"maxUnavailable,omitempty"`
	Healthy        int    `json:"healthy"`
	Desired        int    `json:"desired"`
	Disruptions    int    `json:"disruptions"`
}

// opRBAC is the operator's own permissions — an operator missing one fails in
// ways whose message never mentions RBAC.
type opRBAC struct {
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"` // Role | ClusterRole
	Name      string `json:"name"`
	Rules     int    `json:"rules"`
	Wildcard  bool   `json:"wildcard,omitempty"`
}

type opNode struct {
	Name          string   `json:"name"`
	Ready         bool     `json:"ready"`
	Kubelet       string   `json:"kubelet,omitempty"`
	OS            string   `json:"os,omitempty"`
	Unschedulable bool     `json:"unschedulable,omitempty"`
	Pressure      []string `json:"pressure,omitempty"`
}

type opNamespace struct {
	Name     string `json:"name"`
	Pods     int    `json:"pods"`
	Problems int    `json:"problems"`
}

// opWorkload is one Deployment or StatefulSet, and whether it has the replicas it
// was asked for. This is the cheapest true statement about a cluster's health.
type opWorkload struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Desired   int    `json:"desired"`
	Ready     int    `json:"ready"`
	Updated   int    `json:"updated,omitempty"`
	Available int    `json:"available,omitempty"`
}

func (w opWorkload) short() bool { return w.Ready < w.Desired }

// opPod is a pod worth mentioning: not Running, not Ready, restarting, or killed.
// A healthy pod never reaches the model — a summary of 400 fine pods is not a
// summary.
type opPod struct {
	Namespace  string        `json:"namespace"`
	Name       string        `json:"name"`
	Phase      string        `json:"phase"`
	Node       string        `json:"node,omitempty"`
	Reason     string        `json:"reason,omitempty"`
	Message    string        `json:"message,omitempty"`
	Restarts   int           `json:"restarts"`
	Containers []opContainer `json:"containers,omitempty"`
	Age        string        `json:"age,omitempty"`
	LogTail    []string      `json:"logTail,omitempty"`
}

type opContainer struct {
	Name         string `json:"name"`
	Ready        bool   `json:"ready"`
	RestartCount int    `json:"restartCount"`
	State        string `json:"state,omitempty"`  // Running | Waiting | Terminated
	Reason       string `json:"reason,omitempty"` // CrashLoopBackOff, OOMKilled, …
	Message      string `json:"message,omitempty"`
	ExitCode     *int   `json:"exitCode,omitempty"`
	LastReason   string `json:"lastReason,omitempty"`
	Image        string `json:"image,omitempty"`
}

// opCR is one operator custom resource and the status it publishes about itself.
// Components is what the Percona operators put under status — pxc/haproxy/proxysql
// for PXC, replsets/mongos for PSMDB, and so on — normalised to ready-of-size.
type opCR struct {
	Kind       string          `json:"kind"`
	Group      string          `json:"group"`
	Namespace  string          `json:"namespace"`
	Name       string          `json:"name"`
	State      string          `json:"state,omitempty"`
	Host       string          `json:"host,omitempty"`
	Version    string          `json:"version,omitempty"`
	Ready      string          `json:"ready,omitempty"`
	Components []opCRComponent `json:"components,omitempty"`
	Conditions []opCRCondition `json:"conditions,omitempty"`
	Message    string          `json:"message,omitempty"`
	// SecretRefs are the secret names this resource's spec asks for, which is how
	// a typo in a CR becomes a pod that will never start.
	SecretRefs []string `json:"secretRefs,omitempty"`
}

type opCRComponent struct {
	Name   string `json:"name"`
	Ready  int    `json:"ready"`
	Size   int    `json:"size"`
	Status string `json:"status,omitempty"`
}

type opCRCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	At      string `json:"at,omitempty"`
}

// opOperator is an operator pod's log, reduced to the lines that matter and the
// repeats collapsed. An operator that cannot reconcile says so hundreds of times;
// the useful output is the distinct message and how often it appeared.
type opOperator struct {
	Namespace string       `json:"namespace"`
	Pod       string       `json:"pod"`
	Kind      string       `json:"kind,omitempty"` // pxc | psmdb | pg | ps | unknown
	Lines     int          `json:"lines"`
	Errors    int          `json:"errors"`
	Groups    []opLogGroup `json:"groups,omitempty"`
}

type opLogGroup struct {
	Level   string `json:"level"`
	Count   int    `json:"count"`
	Message string `json:"message"`
	Last    string `json:"last,omitempty"`
}

type opFinding struct {
	Severity string `json:"severity"` // critical | warning | note
	Title    string `json:"title"`
	Detail   string `json:"detail,omitempty"`
	Where    string `json:"where,omitempty"`
}

type opVerdict struct {
	Key    string `json:"key"`
	Tone   string `json:"tone"` // bad | warn | good
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
}

// ---------------------------------------------------------------- k8s subset

// Only the fields this summary reads are declared. sigs.k8s.io/yaml converts YAML
// to JSON before unmarshalling, so these are plain json tags and match the API's
// own field names.

type k8sList struct {
	Items []json.RawMessage `json:"items"`
}

type k8sMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	Labels            map[string]string `json:"labels"`
	CreationTimestamp string            `json:"creationTimestamp"`
}

type k8sCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	LastTransitionTime string `json:"lastTransitionTime"`
}

type k8sContainerState struct {
	Waiting *struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"waiting"`
	Running *struct {
		StartedAt string `json:"startedAt"`
	} `json:"running"`
	Terminated *struct {
		Reason     string `json:"reason"`
		Message    string `json:"message"`
		ExitCode   int    `json:"exitCode"`
		FinishedAt string `json:"finishedAt"`
	} `json:"terminated"`
}

type k8sContainerStatus struct {
	Name         string            `json:"name"`
	Ready        bool              `json:"ready"`
	RestartCount int               `json:"restartCount"`
	Image        string            `json:"image"`
	State        k8sContainerState `json:"state"`
	LastState    k8sContainerState `json:"lastState"`
}

type k8sContainerSpec struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

type k8sVolume struct {
	Name   string `json:"name"`
	Secret *struct {
		SecretName string `json:"secretName"`
	} `json:"secret"`
	PersistentVolumeClaim *struct {
		ClaimName string `json:"claimName"`
	} `json:"persistentVolumeClaim"`
}

type k8sPod struct {
	Metadata k8sMeta `json:"metadata"`
	Spec     struct {
		NodeName       string             `json:"nodeName"`
		Containers     []k8sContainerSpec `json:"containers"`
		InitContainers []k8sContainerSpec `json:"initContainers"`
		Volumes        []k8sVolume        `json:"volumes"`
	} `json:"spec"`
	Status struct {
		Phase                 string               `json:"phase"`
		Reason                string               `json:"reason"`
		Message               string               `json:"message"`
		Conditions            []k8sCondition       `json:"conditions"`
		ContainerStatuses     []k8sContainerStatus `json:"containerStatuses"`
		InitContainerStatuses []k8sContainerStatus `json:"initContainerStatuses"`
	} `json:"status"`
}

type k8sWorkload struct {
	Kind     string  `json:"kind"`
	Metadata k8sMeta `json:"metadata"`
	Spec     struct {
		Replicas *int `json:"replicas"`
	} `json:"spec"`
	Status struct {
		Replicas          int `json:"replicas"`
		ReadyReplicas     int `json:"readyReplicas"`
		UpdatedReplicas   int `json:"updatedReplicas"`
		AvailableReplicas int `json:"availableReplicas"`
	} `json:"status"`
}

type k8sPVC struct {
	Metadata k8sMeta `json:"metadata"`
	Spec     struct {
		StorageClassName *string `json:"storageClassName"`
		VolumeName       string  `json:"volumeName"`
		Resources        struct {
			Requests map[string]string `json:"requests"`
		} `json:"resources"`
	} `json:"spec"`
	Status struct {
		Phase    string            `json:"phase"`
		Capacity map[string]string `json:"capacity"`
	} `json:"status"`
}

type k8sNode struct {
	Metadata k8sMeta `json:"metadata"`
	Spec     struct {
		Unschedulable bool `json:"unschedulable"`
	} `json:"spec"`
	Status struct {
		Conditions []k8sCondition `json:"conditions"`
		NodeInfo   struct {
			KubeletVersion string `json:"kubeletVersion"`
			OSImage        string `json:"osImage"`
		} `json:"nodeInfo"`
	} `json:"status"`
}

// ---------------------------------------------------------------- reading

// opFiles is one archive, flattened: path inside cluster-dump/ -> content.
type opFiles map[string][]byte

// readOpArchive un-tars a gzipped collector archive. Paths are kept relative to
// the cluster-dump/ root the collector writes, so a re-tarred archive that lost
// that prefix still parses.
func readOpArchive(gzData []byte) (opFiles, error) {
	zr, err := gzip.NewReader(strings.NewReader(string(gzData)))
	if err != nil {
		return nil, fmt.Errorf("not a gzipped archive: %w", err)
	}
	defer zr.Close()

	files := opFiles{}
	tr := tar.NewReader(zr)
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if total += h.Size; total > opArchiveLimit {
			return nil, fmt.Errorf("archive is larger than %d bytes uncompressed", int64(opArchiveLimit))
		}
		data, err := io.ReadAll(io.LimitReader(tr, opArchiveLimit))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", h.Name, err)
		}
		name := path.Clean(h.Name)
		// Every path is under cluster-dump/; drop it so lookups are by namespace.
		if i := strings.Index(name, "cluster-dump/"); i >= 0 {
			name = name[i+len("cluster-dump/"):]
		}
		if name == "" || name == "." {
			continue
		}
		files[name] = data
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("archive is empty — is this a pt-k8s-debug-collector cluster-dump?")
	}
	return files, nil
}

// items unmarshals one resource file into its List items. A missing file, an empty
// list and a parse failure are all "nothing here": the collector writes a file per
// resource per namespace whether or not anything exists, so most are empty and an
// error on one must never sink the whole summary.
func (f opFiles) items(name string) []json.RawMessage {
	data, ok := f[name]
	if !ok || len(data) == 0 {
		return nil
	}
	var l k8sList
	if err := yaml.Unmarshal(data, &l); err != nil {
		return nil
	}
	return l.Items
}

// namespaces returns the namespace directories in the archive, sorted. The
// collector names them by directory, so this is the set of top-level dirs that
// hold a pods.yaml.
func (f opFiles) namespaces() []string {
	seen := map[string]bool{}
	for name := range f {
		dir, file := path.Split(name)
		if file != "pods.yaml" {
			continue
		}
		if ns := strings.Trim(dir, "/"); ns != "" && !strings.Contains(ns, "/") {
			seen[ns] = true
		}
	}
	out := make([]string, 0, len(seen))
	for ns := range seen {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// crFiles returns the operator custom-resource dumps in a namespace: the collector
// names them <plural>.<group>.yaml, which is what distinguishes them from the
// fixed set of core resources it always writes.
func (f opFiles) crFiles(ns string) []string {
	var out []string
	prefix := ns + "/"
	for name := range f {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		base := strings.TrimPrefix(name, prefix)
		if strings.Contains(base, "/") || !strings.HasSuffix(base, ".yaml") {
			continue
		}
		stem := strings.TrimSuffix(base, ".yaml")
		// A CR dump is <plural>.<group>; a core resource is a bare plural.
		if strings.Contains(stem, ".") {
			out = append(out, base)
		}
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- parsing

// parseOpDump is the entry point: an archive in, a rendered model out.
func parseOpDump(gzData []byte, source string) (*opModel, error) {
	files, err := readOpArchive(gzData)
	if err != nil {
		return nil, err
	}
	m := &opModel{Source: source, Available: map[string]bool{}}

	if errs := strings.TrimSpace(string(files["errors.txt"])); errs != "" {
		for _, ln := range strings.Split(errs, "\n") {
			if ln = strings.TrimSpace(ln); ln != "" {
				m.CollectorErrors = append(m.CollectorErrors, ln)
			}
		}
	}
	parseOpNodes(m, files)
	parseOpWorkloads(m, files)
	parseOpPods(m, files)
	parseOpCRs(m, files)
	parseOpOperatorLogs(m, files)
	parseOpImages(m, files)
	parseOpCerts(m, files)
	parseOpSecrets(m, files)
	parseOpBackups(m, files)
	parseOpStorage(m, files)
	parseOpDeployment(m, files)
	parseOpGalera(m, files)
	parseOpPodSummaries(m, files)
	parseOpBackupLogs(m, files)
	parseOpSchedules(m, files)
	parseOpConfig(m, files)
	parseOpRollouts(m, files)
	parseOpBudgets(m, files)
	parseOpRBAC(m, files)
	m.logInputs = opLogInputs(files)
	if len(m.logInputs) > 0 {
		m.Logs = opDigestLogs(lsBuild(m.logInputs))
	}

	m.Available["nodes"] = len(m.Nodes) > 0
	m.Available["workloads"] = len(m.Workloads) > 0
	m.Available["pods"] = len(m.Pods) > 0
	m.Available["crs"] = len(m.CRs) > 0
	m.Available["operators"] = len(m.Operators) > 0
	m.Available["images"] = len(m.Images) > 0
	m.Available["secrets"] = len(m.Secrets) > 0
	m.Available["backups"] = len(m.Backups) > 0
	m.Available["certs"] = len(m.Certs) > 0
	m.Available["storage"] = len(m.Storage) > 0
	m.Available["deployment"] = m.Deployment != nil
	m.Available["logs"] = m.Logs != nil
	m.Available["galera"] = len(m.Galera) > 0
	m.Available["podSummaries"] = len(m.PodSummaries) > 0
	m.Available["backupLogs"] = len(m.BackupLogs) > 0
	m.Available["schedules"] = len(m.Schedules) > 0
	m.Available["config"] = len(m.Config) > 0

	// Added before computeOpFindings, which appends and then sorts — so this lands
	// with the other notes rather than at the end of the list.
	noteMissingServerLogs(m, files)
	computeOpFindings(m)
	computeOpVerdicts(m)
	return m, nil
}

func parseOpNodes(m *opModel, f opFiles) {
	for _, raw := range f.items("nodes.yaml") {
		var n k8sNode
		if json.Unmarshal(raw, &n) != nil {
			continue
		}
		node := opNode{
			Name:          n.Metadata.Name,
			Kubelet:       n.Status.NodeInfo.KubeletVersion,
			OS:            n.Status.NodeInfo.OSImage,
			Unschedulable: n.Spec.Unschedulable,
		}
		for _, c := range n.Status.Conditions {
			switch {
			case c.Type == "Ready":
				node.Ready = c.Status == "True"
			// Every other node condition is a pressure//problem flag that is bad
			// when True — MemoryPressure, DiskPressure, PIDPressure, NetworkUnavailable.
			case c.Status == "True":
				node.Pressure = append(node.Pressure, c.Type)
			}
		}
		if m.Kubernetes == "" {
			m.Kubernetes = node.Kubelet
		}
		m.Nodes = append(m.Nodes, node)
	}
	sort.Slice(m.Nodes, func(i, j int) bool { return m.Nodes[i].Name < m.Nodes[j].Name })
}

func parseOpWorkloads(m *opModel, f opFiles) {
	for _, ns := range f.namespaces() {
		for kind, file := range map[string]string{"Deployment": "deployments.yaml", "StatefulSet": "statefulsets.yaml"} {
			for _, raw := range f.items(ns + "/" + file) {
				var w k8sWorkload
				if json.Unmarshal(raw, &w) != nil {
					continue
				}
				desired := w.Status.Replicas
				if w.Spec.Replicas != nil {
					desired = *w.Spec.Replicas
				}
				m.Workloads = append(m.Workloads, opWorkload{
					Kind: kind, Namespace: ns, Name: w.Metadata.Name,
					Desired: desired, Ready: w.Status.ReadyReplicas,
					Updated: w.Status.UpdatedReplicas, Available: w.Status.AvailableReplicas,
				})
			}
		}
	}
	// Short workloads first, then by namespace/name: the reason to open this page
	// is the thing that is not running.
	sort.Slice(m.Workloads, func(i, j int) bool {
		a, b := m.Workloads[i], m.Workloads[j]
		if a.short() != b.short() {
			return a.short()
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
}

// podProblem reduces a pod's status to a reason, or "" when nothing is wrong. The
// order matters: a container's waiting reason (CrashLoopBackOff, ImagePullBackOff)
// is more specific than the pod phase, and an OOMKill in lastState is the thing a
// reader most wants surfaced even while the container is Running again.
func podProblem(p k8sPod) (reason, message string) {
	for _, cs := range append(append([]k8sContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" && w.Reason != "PodInitializing" {
			return w.Reason, w.Message
		}
		if t := cs.LastState.Terminated; t != nil && t.Reason == "OOMKilled" {
			return "OOMKilled", fmt.Sprintf("container %s was OOM-killed (exit %d)", cs.Name, t.ExitCode)
		}
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
			return "Error", fmt.Sprintf("container %s exited %d (%s)", cs.Name, t.ExitCode, t.Reason)
		}
	}
	// A crash-looping pod caught in the moment between restarts is Running and
	// Ready and looks perfectly healthy — the capture is a single instant, and it
	// lands wherever it lands. The restart count and whatever killed the container
	// last are the evidence that it is not healthy, so they outrank the phase.
	// Found by parsing a real archive: of two identically-broken pods, one was
	// caught mid-crash and reported, the other was caught running and was not.
	for _, cs := range append(append([]k8sContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		if cs.RestartCount > 0 && cs.LastState.Terminated != nil {
			t := cs.LastState.Terminated
			return "Restarting", fmt.Sprintf("container %s has restarted %d time(s); last exit %d (%s)",
				cs.Name, cs.RestartCount, t.ExitCode, firstNonEmpty(t.Reason, "no reason given"))
		}
	}
	switch p.Status.Phase {
	case "Running", "Succeeded":
		// Running but not Ready is still a problem — a failing readiness probe
		// keeps a pod out of its Service while everything here looks fine.
		for _, c := range p.Status.Conditions {
			if c.Type == "Ready" && c.Status != "True" {
				return firstNonEmpty(c.Reason, "NotReady"), c.Message
			}
		}
		return "", ""
	case "Pending":
		for _, c := range p.Status.Conditions {
			if c.Type == "PodScheduled" && c.Status != "True" {
				return firstNonEmpty(c.Reason, "Pending"), c.Message
			}
		}
		return "Pending", p.Status.Message
	default:
		return firstNonEmpty(p.Status.Reason, p.Status.Phase), p.Status.Message
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func parseOpPods(m *opModel, f opFiles) {
	for _, ns := range f.namespaces() {
		items := f.items(ns + "/pods.yaml")
		nsRow := opNamespace{Name: ns, Pods: len(items)}
		for _, raw := range items {
			var p k8sPod
			if json.Unmarshal(raw, &p) != nil {
				continue
			}
			reason, message := podProblem(p)
			if reason == "" {
				continue
			}
			nsRow.Problems++
			row := opPod{
				Namespace: ns, Name: p.Metadata.Name, Phase: p.Status.Phase,
				Node: p.Spec.NodeName, Reason: reason, Message: strings.TrimSpace(message),
				Age: sinceStamp(p.Metadata.CreationTimestamp),
			}
			for _, cs := range append(append([]k8sContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
				c := opContainer{Name: cs.Name, Ready: cs.Ready, RestartCount: cs.RestartCount, Image: cs.Image}
				row.Restarts += cs.RestartCount
				switch {
				case cs.State.Waiting != nil:
					c.State, c.Reason, c.Message = "Waiting", cs.State.Waiting.Reason, cs.State.Waiting.Message
				case cs.State.Terminated != nil:
					code := cs.State.Terminated.ExitCode
					c.State, c.Reason, c.ExitCode = "Terminated", cs.State.Terminated.Reason, &code
				case cs.State.Running != nil:
					c.State = "Running"
				}
				if cs.LastState.Terminated != nil {
					c.LastReason = cs.LastState.Terminated.Reason
				}
				row.Containers = append(row.Containers, c)
			}
			// The pod's own log is in the archive; the tail is usually the answer.
			if log, ok := f[ns+"/"+p.Metadata.Name+"/logs.txt"]; ok {
				row.LogTail = tailLines(string(log), 12)
			}
			m.Pods = append(m.Pods, row)
		}
		if nsRow.Pods > 0 {
			m.Namespaces = append(m.Namespaces, nsRow)
		}
	}
	sort.Slice(m.Pods, func(i, j int) bool {
		a, b := m.Pods[i], m.Pods[j]
		if (a.Restarts > 0) != (b.Restarts > 0) {
			return a.Restarts > b.Restarts
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
}

func tailLines(s string, n int) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	out := []string{}
	for _, ln := range lines {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// sinceStamp renders an RFC3339 creation stamp as an age. Best effort — an
// unparseable stamp simply has no age rather than breaking the row.
func sinceStamp(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// ---------------------------------------------------------------- custom resources

// opCRComponentKeys are the status sub-objects the Percona operators publish a
// ready/size pair under. The list is deliberately explicit: status also carries
// scalars and conditions, and walking every map key would turn those into
// components with zero of zero.
var opCRComponentKeys = []string{
	"pxc", "haproxy", "proxysql", "logcollector", "backup", // PXC
	"mysql", "orchestrator", "router", // PS
	"mongos", "configsvr", "replsets", // PSMDB
	"postgresql", "pgbouncer", "pgbackrest", // PG
}

// crStatus is the slice of a Percona CR's status this reads. Everything is
// optional: the operators disagree on which of these they publish, and a CR that
// has only just been created has almost none of them.
type crStatus struct {
	State      string                     `json:"state"`
	Status     string                     `json:"status"`
	Host       string                     `json:"host"`
	Size       int                        `json:"size"`
	Ready      int                        `json:"ready"`
	Message    string                     `json:"message"`
	Conditions []k8sCondition             `json:"conditions"`
	Raw        map[string]json.RawMessage `json:"-"`
}

type crComponentStatus struct {
	Size   int    `json:"size"`
	Ready  int    `json:"ready"`
	Status string `json:"status"`
}

func parseOpCRs(m *opModel, f opFiles) {
	for _, ns := range f.namespaces() {
		for _, file := range f.crFiles(ns) {
			// <plural>.<group>.yaml — split on the first dot.
			stem := strings.TrimSuffix(file, ".yaml")
			plural, group, _ := strings.Cut(stem, ".")
			// Backup and restore resources are custom resources too, but they have
			// their own panel and their own status shape. Listing them here as
			// well reported one backup as two things.
			if strings.Contains(plural, "backup") || strings.Contains(plural, "restore") {
				continue
			}
			for _, raw := range f.items(ns + "/" + file) {
				var head struct {
					Kind     string          `json:"kind"`
					Metadata k8sMeta         `json:"metadata"`
					Spec     json.RawMessage `json:"spec"`
					Status   json.RawMessage `json:"status"`
				}
				if json.Unmarshal(raw, &head) != nil {
					continue
				}
				cr := opCR{
					Kind: firstNonEmpty(head.Kind, plural), Group: group,
					Namespace: ns, Name: head.Metadata.Name,
				}
				var spec struct {
					CRVersion string `json:"crVersion"`
				}
				_ = json.Unmarshal(head.Spec, &spec)
				cr.Version = spec.CRVersion
				cr.SecretRefs = crSecretRefs(head.Spec)

				if len(head.Status) > 0 {
					var st crStatus
					_ = json.Unmarshal(head.Status, &st)
					cr.State = firstNonEmpty(st.State, st.Status)
					cr.Host, cr.Message = st.Host, st.Message
					if st.Size > 0 {
						cr.Ready = fmt.Sprintf("%d/%d", st.Ready, st.Size)
					}
					for _, c := range st.Conditions {
						cr.Conditions = append(cr.Conditions, opCRCondition{
							Type: c.Type, Status: c.Status, Reason: c.Reason,
							Message: c.Message, At: c.LastTransitionTime,
						})
					}
					var byKey map[string]json.RawMessage
					if json.Unmarshal(head.Status, &byKey) == nil {
						for _, key := range opCRComponentKeys {
							rawc, ok := byKey[key]
							if !ok {
								continue
							}
							cr.Components = append(cr.Components, crComponentsFrom(key, rawc)...)
						}
					}
				}
				m.CRs = append(m.CRs, cr)
			}
		}
	}
	sort.Slice(m.CRs, func(i, j int) bool {
		a, b := m.CRs[i], m.CRs[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
}

// ---------------------------------------------------------------- operator logs

// opOperatorKinds maps a hint in the pod name to the operator it is. The Percona
// operator deployments are named for their product, which is the only clue the
// archive carries once the CRs are separated out.
var opOperatorKinds = map[string]string{
	"percona-xtradb-cluster-operator": "pxc",
	"percona-server-mongodb-operator": "psmdb",
	"percona-postgresql-operator":     "pg",
	"percona-server-mysql-operator":   "ps",
	"pgo":                             "pgo",
	"cloudnative-pg":                  "cnpg",
}

// opLogLevel classifies a log line. The Percona operators log JSON through
// zap ({"level":"error",...}); the community ones log text with a level word.
// Both are recognised, and anything else is not an error line.
func opLogLevel(line string) (level, message string) {
	if strings.HasPrefix(strings.TrimSpace(line), "{") {
		var rec struct {
			Level string `json:"level"`
			Msg   string `json:"msg"`
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Level != "" {
			return strings.ToLower(rec.Level), firstNonEmpty(rec.Error, rec.Msg)
		}
	}
	lower := strings.ToLower(line)
	switch {
	case strings.Contains(lower, "error") || strings.Contains(lower, "fatal") || strings.Contains(lower, "panic"):
		return "error", strings.TrimSpace(line)
	case strings.Contains(lower, "warn"):
		return "warn", strings.TrimSpace(line)
	}
	return "", ""
}

// opLogNoise strips the parts of a message that differ between otherwise identical
// lines — timestamps, UIDs, durations — so the same failure repeated 400 times
// collapses to one group with a count instead of 400 rows.
func opLogNoise(msg string) string {
	var b strings.Builder
	for _, field := range strings.Fields(msg) {
		if len(field) > 24 && strings.Count(field, "-") >= 4 {
			b.WriteString("<uid> ")
			continue
		}
		if _, err := strconv.ParseFloat(strings.TrimRight(field, "sm)"), 64); err == nil && len(field) > 1 {
			b.WriteString("<n> ")
			continue
		}
		b.WriteString(field + " ")
	}
	return strings.TrimSpace(b.String())
}

func parseOpOperatorLogs(m *opModel, f opFiles) {
	for name, data := range f {
		if !strings.HasSuffix(name, "/logs.txt") {
			continue
		}
		parts := strings.Split(strings.TrimSuffix(name, "/logs.txt"), "/")
		if len(parts) != 2 {
			continue
		}
		ns, pod := parts[0], parts[1]
		kind := ""
		for hint, k := range opOperatorKinds {
			if strings.Contains(pod, hint) {
				kind = k
				break
			}
		}
		// Only operator pods get log analysis. Every other pod's log is already
		// carried as a tail on its own row, and parsing a database's error log
		// here would drown the panel that is supposed to be about the operator.
		if kind == "" && !strings.Contains(pod, "-operator") {
			continue
		}
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		op := opOperator{Namespace: ns, Pod: pod, Kind: firstNonEmpty(kind, "unknown"), Lines: len(lines)}
		groups := map[string]*opLogGroup{}
		var order []string
		for _, ln := range lines {
			level, msg := opLogLevel(ln)
			if level != "error" && level != "warn" {
				continue
			}
			if level == "error" {
				op.Errors++
			}
			key := level + "\x00" + opLogNoise(msg)
			g, ok := groups[key]
			if !ok {
				g = &opLogGroup{Level: level, Message: truncate(msg, 300)}
				groups[key] = g
				order = append(order, key)
			}
			g.Count++
			g.Last = truncate(strings.TrimSpace(ln), 300)
		}
		for _, k := range order {
			op.Groups = append(op.Groups, *groups[k])
		}
		sort.SliceStable(op.Groups, func(i, j int) bool {
			a, b := op.Groups[i], op.Groups[j]
			if (a.Level == "error") != (b.Level == "error") {
				return a.Level == "error"
			}
			return a.Count > b.Count
		})
		if len(op.Groups) > 25 {
			op.Groups = op.Groups[:25]
		}
		m.Operators = append(m.Operators, op)
	}
	sort.Slice(m.Operators, func(i, j int) bool { return m.Operators[i].Pod < m.Operators[j].Pod })
}

// ---------------------------------------------------------------- findings

// computeOpFindings turns the parsed model into the list a reader scans. Severity
// is about what it means, not how loud it is: a pod that cannot be scheduled and a
// pod that is crash-looping are both critical, a restart count is a warning, and a
// collector that failed to read something is a note.
func computeOpFindings(m *opModel) {
	add := func(sev, title, detail, where string) {
		m.Findings = append(m.Findings, opFinding{Severity: sev, Title: title, Detail: detail, Where: where})
	}

	for _, n := range m.Nodes {
		if !n.Ready {
			add("critical", "Node not Ready", "The kubelet is not reporting Ready; every pod on it is at risk.", n.Name)
		}
		if n.Unschedulable {
			add("warning", "Node cordoned", "Marked unschedulable, so nothing new will land here.", n.Name)
		}
		for _, p := range n.Pressure {
			add("warning", "Node under "+p, "The kubelet is reporting "+p+", which evicts or blocks pods.", n.Name)
		}
	}

	for _, w := range m.Workloads {
		if !w.short() {
			continue
		}
		add("critical",
			fmt.Sprintf("%s %s has %d of %d replicas ready", w.Kind, w.Name, w.Ready, w.Desired),
			"The controller has not reached its desired replica count.",
			w.Namespace+"/"+w.Name)
	}

	for _, p := range m.Pods {
		sev := "warning"
		switch p.Reason {
		case "CrashLoopBackOff", "OOMKilled", "ImagePullBackOff", "ErrImagePull",
			"CreateContainerConfigError", "CreateContainerError", "Unschedulable", "Evicted":
			sev = "critical"
		}
		detail := p.Message
		if detail == "" && p.Restarts > 0 {
			detail = fmt.Sprintf("%d restart(s) across its containers.", p.Restarts)
		}
		add(sev, fmt.Sprintf("Pod %s is %s", p.Name, p.Reason), detail, p.Namespace+"/"+p.Name)
	}

	for _, cr := range m.CRs {
		state := strings.ToLower(cr.State)
		if state != "" && state != "ready" {
			add("critical", fmt.Sprintf("%s %s is %s", cr.Kind, cr.Name, cr.State),
				firstNonEmpty(cr.Message, "The operator has not brought this resource to ready."),
				cr.Namespace+"/"+cr.Name)
		}
		for _, c := range cr.Conditions {
			if c.Status == "True" && (c.Type == "Error" || strings.EqualFold(c.Reason, "ErrorReconcile")) {
				add("critical", fmt.Sprintf("%s %s reports %s", cr.Kind, cr.Name, firstNonEmpty(c.Reason, c.Type)),
					c.Message, cr.Namespace+"/"+cr.Name)
			}
		}
		for _, comp := range cr.Components {
			if comp.Size > 0 && comp.Ready < comp.Size {
				add("warning",
					fmt.Sprintf("%s %s: %s is %d/%d", cr.Kind, cr.Name, comp.Name, comp.Ready, comp.Size),
					"The operator's own status says this component is short.",
					cr.Namespace+"/"+cr.Name)
			}
		}
	}

	for _, op := range m.Operators {
		for _, g := range op.Groups {
			if g.Level != "error" {
				continue
			}
			add("critical",
				fmt.Sprintf("Operator error x%d", g.Count),
				g.Message,
				op.Namespace+"/"+op.Pod)
			break // the top group is the headline; the rest are in the panel
		}
	}

	// Backups and restores. A failed one is the loudest thing in an archive that
	// was very often taken *because* a backup failed.
	for _, b := range m.Backups {
		what := "Backup"
		if b.Restore {
			what = "Restore"
		}
		switch {
		case backupFailed(b.State):
			add("critical", fmt.Sprintf("%s %s failed", what, b.Name),
				firstNonEmpty(b.Error, "The operator reported state "+b.State+"."),
				b.Namespace+"/"+b.Name)
		case backupRunning(b.State):
			add("note", fmt.Sprintf("%s %s is %s", what, b.Name, b.State),
				strings.TrimSpace(b.Destination+" "+b.Storage),
				b.Namespace+"/"+b.Name)
		}
	}

	// A backup whose custom resource says "Running" while the pods doing the work
	// are in Error is the case worth catching: the CR is reporting the *job*, which
	// the operator keeps retrying, so it stays Running indefinitely while every
	// attempt fails. Found live — a PXC backup sat at Running for ten minutes while
	// its xb- pods failed with "xbcloud: Probe failed" against an unreachable S3
	// endpoint. Reading the CR alone would have called that healthy.
	for _, b := range m.Backups {
		if !backupRunning(b.State) {
			continue
		}
		var failing []string
		for _, p := range m.Pods {
			if p.Namespace != b.Namespace || !strings.Contains(p.Name, b.Name) {
				continue
			}
			if p.Reason == "Error" || p.Reason == "CrashLoopBackOff" || p.Restarts > 0 {
				failing = append(failing, p.Name)
			}
		}
		if len(failing) > 0 {
			add("critical",
				fmt.Sprintf("Backup %s says %s, but %d of its pods have failed", b.Name, b.State, len(failing)),
				"The operator retries the job, so the resource stays in a running state while every attempt fails. Pods: "+
					strings.Join(failing, ", "),
				b.Namespace+"/"+b.Name)
		}
	}

	// Certificates. The deadline is real and nothing else in the archive mentions it.
	for _, c := range m.Certs {
		if c.DaysLeft == nil {
			continue
		}
		switch {
		case *c.DaysLeft < 0:
			add("critical", fmt.Sprintf("Certificate %s/%s has expired", c.Secret, c.Entry),
				"Expired "+c.NotAfter+".", c.Namespace+"/"+c.Secret)
		case *c.DaysLeft <= 30:
			add("warning", fmt.Sprintf("Certificate %s/%s expires in %d days", c.Secret, c.Entry, *c.DaysLeft),
				"Not after "+c.NotAfter+".", c.Namespace+"/"+c.Secret)
		}
	}

	// Storage. A claim that never bound is why a database never started.
	for _, v := range m.Storage {
		if v.Status != "" && v.Status != "Bound" {
			add("critical", fmt.Sprintf("PersistentVolumeClaim %s is %s", v.Name, v.Status),
				"Requested "+v.Requested+" from storage class "+firstNonEmpty(v.StorageClass, "(default)")+".",
				v.Namespace+"/"+v.Name)
		}
	}

	// Galera. The one question a stopped PXC cluster turns on.
	if len(m.Galera) > 0 {
		safe := 0
		for _, g := range m.Galera {
			if g.SafeToBootstrap {
				safe++
			}
		}
		if safe == 0 {
			add("warning", "No PXC member is marked safe to bootstrap",
				"Every member's grastate.dat has safe_to_bootstrap: 0. If the cluster is fully stopped it will not start on its own — the member with the highest seqno has to be bootstrapped deliberately.",
				"galera")
		}
		for _, g := range m.Galera {
			if g.Seqno == "-1" && !g.SafeToBootstrap {
				add("note", fmt.Sprintf("%s has no recorded seqno", g.Pod),
					"grastate.dat shows seqno: -1, which means the member did not shut down cleanly or is currently running. Its position is only recoverable with mysqld --wsrep-recover.",
					g.Namespace+"/"+g.Pod)
			}
		}
	}

	// A backup log carrying errors, whatever the custom resource says.
	for _, l := range m.BackupLogs {
		if len(l.Errors) == 0 {
			continue
		}
		add("critical", fmt.Sprintf("%s on %s reports errors", l.File, l.Pod),
			strings.Join(l.Errors, "\n"), l.Namespace+"/"+l.Pod)
	}

	// A suspended schedule is a backup that will never run, and says nothing.
	for _, sc := range m.Schedules {
		if sc.Suspended {
			add("warning", fmt.Sprintf("Scheduled job %s is suspended", sc.Name),
				"Schedule "+sc.Schedule+" will not fire while it is suspended.", sc.Namespace+"/"+sc.Name)
		}
	}

	// A disruption budget that allows nothing blocks drains and rolling upgrades.
	for _, b := range m.Budgets {
		if b.Disruptions == 0 && b.Healthy < b.Desired {
			add("warning", fmt.Sprintf("PodDisruptionBudget %s allows no disruptions", b.Name),
				fmt.Sprintf("%d healthy of %d desired. A node drain or rolling upgrade will block on this.", b.Healthy, b.Desired),
				b.Namespace+"/"+b.Name)
		}
	}

	// An old ReplicaSet still holding pods is a rollout that never finished.
	for _, r := range m.Rollouts {
		if r.Stuck > 0 {
			add("warning", fmt.Sprintf("%s has %d stale revision(s) still holding pods", r.Owner, r.Stuck),
				"A scaled-to-zero ReplicaSet with running pods is a rollout that did not complete.",
				r.Namespace+"/"+r.Owner)
		}
	}

	// Log Summary's own findings, promoted into this page's list. Its classifiers
	// know things this file never will — a Galera state transfer that never
	// completed, a PostgreSQL checkpoint storm — and re-deriving them here would be
	// a second, worse copy of rules that already exist.
	if m.Logs != nil {
		for _, f := range m.Logs.Findings {
			sev := "warning"
			switch strings.ToLower(f.Severity) {
			case "crit", "critical", "error":
				sev = "critical"
			case "info", "note":
				sev = "note"
			}
			add(sev, f.Title, f.Detail, "logs")
		}
	}

	// A capture taken while the cluster was still coming up is the case that looks
	// most like a bug in this page and is not one. `kubectl logs --all-containers`
	// fails outright if ANY container in the pod is still PodInitializing, so a
	// capture taken during a rollout can come back with almost no logs — and the
	// database's own log files are not on disk yet either. Found exactly that way:
	// a 176 KB archive with three pod logs and no mysqld-error.log at all, beside a
	// 379 KB one of the same cluster minutes later with everything. Counting the
	// collector's own failures and saying so up front is the difference between
	// "this page did not read my logs" and "this capture does not contain them".
	initializing := 0
	for _, e := range m.CollectorErrors {
		if strings.Contains(e, "PodInitializing") || strings.Contains(e, "ContainerCreating") {
			initializing++
		}
	}
	if initializing > 0 {
		add("warning", fmt.Sprintf("This capture was taken while %d pod(s) were still starting", initializing),
			"kubectl logs fails for the whole pod if any container in it is still initializing, so those pods contributed no log at all — and a database that has not started yet has written no log file to collect either. Take another capture once the cluster is settled.",
			"capture")
	}

	for _, e := range m.CollectorErrors {
		add("note", "The collector could not read something", e, "")
	}

	// Critical first, then warnings, then notes — stable within each so the
	// ordering the parsers chose (shortest workload, most restarts) survives.
	rank := map[string]int{"critical": 0, "warning": 1, "note": 2}
	sort.SliceStable(m.Findings, func(i, j int) bool {
		return rank[m.Findings[i].Severity] < rank[m.Findings[j].Severity]
	})
}

// computeOpVerdicts answers the handful of questions a reader opens the page with,
// in the order they would ask them. Each is always present — "good" is an answer,
// and a page that silently omits the question reads as if it was not checked.
func computeOpVerdicts(m *opModel) {
	// 1. Is the cluster itself healthy?
	notReady := 0
	for _, n := range m.Nodes {
		if !n.Ready {
			notReady++
		}
	}
	switch {
	case len(m.Nodes) == 0:
		m.Verdicts = append(m.Verdicts, opVerdict{Key: "nodes", Tone: "warn", Title: "No node data",
			Detail: "The capture has no nodes.yaml — it may have been taken with a kubeconfig that cannot list nodes."})
	case notReady > 0:
		m.Verdicts = append(m.Verdicts, opVerdict{Key: "nodes", Tone: "bad",
			Title:  fmt.Sprintf("%d of %d nodes are not Ready", notReady, len(m.Nodes)),
			Detail: "Fix the cluster before reading anything else here: pods cannot be healthy on a node that is not."})
	default:
		m.Verdicts = append(m.Verdicts, opVerdict{Key: "nodes", Tone: "good",
			Title:  fmt.Sprintf("All %d nodes Ready", len(m.Nodes)),
			Detail: "Kubernetes " + m.Kubernetes})
	}

	// 2. Is everything that should be running, running?
	short, shortNames := 0, []string{}
	for _, w := range m.Workloads {
		if w.short() {
			short++
			if len(shortNames) < 4 {
				shortNames = append(shortNames, fmt.Sprintf("%s (%d/%d)", w.Name, w.Ready, w.Desired))
			}
		}
	}
	if short > 0 {
		m.Verdicts = append(m.Verdicts, opVerdict{Key: "workloads", Tone: "bad",
			Title:  fmt.Sprintf("%d workloads are short of replicas", short),
			Detail: strings.Join(shortNames, " · ")})
	} else if len(m.Workloads) > 0 {
		m.Verdicts = append(m.Verdicts, opVerdict{Key: "workloads", Tone: "good",
			Title:  fmt.Sprintf("All %d workloads at full replicas", len(m.Workloads)),
			Detail: "Every Deployment and StatefulSet has the replicas it asked for."})
	}

	// 3. What is wrong with the pods, in one line?
	byReason := map[string]int{}
	for _, p := range m.Pods {
		byReason[p.Reason]++
	}
	if len(m.Pods) > 0 {
		parts := make([]string, 0, len(byReason))
		for r, n := range byReason {
			parts = append(parts, fmt.Sprintf("%s x%d", r, n))
		}
		sort.Strings(parts)
		m.Verdicts = append(m.Verdicts, opVerdict{Key: "pods", Tone: "bad",
			Title:  fmt.Sprintf("%d pods need attention", len(m.Pods)),
			Detail: strings.Join(parts, " · ")})
	} else {
		m.Verdicts = append(m.Verdicts, opVerdict{Key: "pods", Tone: "good",
			Title: "No unhealthy pods", Detail: "Every pod in the capture is Running and Ready."})
	}

	// 4. Did the backups work? Asked before the operator question because a backup
	// is the thing whose failure is discovered latest and costs most.
	if len(m.Backups) > 0 {
		failed, running := 0, 0
		for _, b := range m.Backups {
			if backupFailed(b.State) {
				failed++
			} else if backupRunning(b.State) {
				running++
			}
		}
		switch {
		case failed > 0:
			m.Verdicts = append(m.Verdicts, opVerdict{Key: "backups", Tone: "bad",
				Title:  fmt.Sprintf("%d of %d backups failed", failed, len(m.Backups)),
				Detail: "A backup that fails silently is discovered when it is needed."})
		case running > 0:
			m.Verdicts = append(m.Verdicts, opVerdict{Key: "backups", Tone: "warn",
				Title:  fmt.Sprintf("%d backup(s) in flight", running),
				Detail: "This capture caught them mid-run, so their outcome is not in it."})
		default:
			m.Verdicts = append(m.Verdicts, opVerdict{Key: "backups", Tone: "good",
				Title:  fmt.Sprintf("All %d backups succeeded", len(m.Backups)),
				Detail: "No backup or restore in this capture is in a failed state."})
		}
	}

	// 5. Is TLS about to expire? Nothing else in the archive carries a deadline.
	if len(m.Certs) > 0 {
		soonest, expired, soon := 1<<30, 0, 0
		for _, c := range m.Certs {
			if c.DaysLeft == nil {
				continue
			}
			if *c.DaysLeft < soonest {
				soonest = *c.DaysLeft
			}
			if *c.DaysLeft < 0 {
				expired++
			} else if *c.DaysLeft <= 30 {
				soon++
			}
		}
		switch {
		case expired > 0:
			m.Verdicts = append(m.Verdicts, opVerdict{Key: "certs", Tone: "bad",
				Title: fmt.Sprintf("%d certificate(s) have expired", expired), Detail: "TLS between the cluster's own components will already be failing."})
		case soon > 0:
			m.Verdicts = append(m.Verdicts, opVerdict{Key: "certs", Tone: "warn",
				Title: fmt.Sprintf("TLS expires in %d days", soonest), Detail: fmt.Sprintf("%d certificate(s) inside 30 days.", soon)})
		case soonest < 1<<30:
			m.Verdicts = append(m.Verdicts, opVerdict{Key: "certs", Tone: "good",
				Title: fmt.Sprintf("TLS valid for %d more days", soonest), Detail: fmt.Sprintf("%d certificate(s) read from the capture.", len(m.Certs))})
		}
	}

	// 6. What does the operator itself say?
	crBad := 0
	for _, cr := range m.CRs {
		if s := strings.ToLower(cr.State); s != "" && s != "ready" {
			crBad++
		}
	}
	opErrors := 0
	for _, op := range m.Operators {
		opErrors += op.Errors
	}
	switch {
	case len(m.CRs) == 0 && len(m.Operators) == 0:
		m.Verdicts = append(m.Verdicts, opVerdict{Key: "operator", Tone: "warn", Title: "No operator found",
			Detail: "The capture holds no Percona custom resources and no operator pod log. Run the capture with a -resource that matches the cluster."})
	case crBad > 0 || opErrors > 0:
		detail := []string{}
		if crBad > 0 {
			detail = append(detail, fmt.Sprintf("%d custom resource(s) not ready", crBad))
		}
		if opErrors > 0 {
			detail = append(detail, fmt.Sprintf("%d error lines in the operator log", opErrors))
		}
		m.Verdicts = append(m.Verdicts, opVerdict{Key: "operator", Tone: "bad",
			Title: "The operator is not reconciling cleanly", Detail: strings.Join(detail, " · ")})
	default:
		m.Verdicts = append(m.Verdicts, opVerdict{Key: "operator", Tone: "good",
			Title:  fmt.Sprintf("%d custom resource(s) ready", len(m.CRs)),
			Detail: "No errors in the operator log."})
	}
}

// opSystemNamespaces are the cluster's own plumbing. Pods and workloads in them
// still count for health — a broken coredns is a broken cluster — but the images
// and secrets panels are about *this deployment*, and listing local-path-provisioner
// and metallb beside the database images buries the thing the reader came for.
var opSystemNamespaces = map[string]bool{
	"kube-system": true, "kube-public": true, "kube-node-lease": true,
	"metallb-system": true, "local-path-storage": true, "kube-flannel": true,
}

func isSystemNamespace(ns string) bool { return opSystemNamespaces[ns] }

// ---------------------------------------------------------------- deployment

// opOperatorImageKinds maps an operator image repository to the short kind. The
// repository is the honest source: a Deployment can be renamed, but the image it
// runs cannot lie about which operator it is.
var opOperatorImageKinds = map[string]string{
	"percona-xtradb-cluster-operator": "pxc",
	"percona-server-mongodb-operator": "psmdb",
	"percona-postgresql-operator":     "pg",
	"percona-server-mysql-operator":   "ps",
	"postgres-operator":               "pgo",
	"cloudnative-pg":                  "cnpg",
}

// splitImage pulls a repository and tag apart. A digest-pinned image keeps the
// digest as its "tag", which is the truthful answer to "what version is this".
func splitImage(image string) (repo, tag string) {
	if at := strings.LastIndex(image, "@"); at >= 0 {
		return image[:at], image[at+1:]
	}
	slash := strings.LastIndex(image, "/")
	if colon := strings.LastIndex(image, ":"); colon > slash {
		return image[:colon], image[colon+1:]
	}
	return image, ""
}

// parseOpDeployment identifies the operator and the version it runs. Read from
// the operator Deployment's image tag, because that is the only place in the whole
// archive the operator's version is actually stated — the CR carries crVersion,
// which is the *API* version it was written for and can differ.
func parseOpDeployment(m *opModel, f opFiles) {
	dep := &opDeployment{Kubernetes: m.Kubernetes}
	for _, ns := range f.namespaces() {
		if isSystemNamespace(ns) {
			continue
		}
		for _, raw := range f.items(ns + "/deployments.yaml") {
			var d struct {
				Metadata k8sMeta `json:"metadata"`
				Spec     struct {
					Template struct {
						Spec struct {
							Containers []k8sContainerSpec `json:"containers"`
						} `json:"spec"`
					} `json:"template"`
				} `json:"spec"`
			}
			if json.Unmarshal(raw, &d) != nil {
				continue
			}
			for _, c := range d.Spec.Template.Spec.Containers {
				repo, tag := splitImage(c.Image)
				for hint, kind := range opOperatorImageKinds {
					if strings.Contains(repo, hint) {
						dep.Operator, dep.OperatorName, dep.Version = kind, repo, tag
						dep.Namespace = ns
						dep.Pod = d.Metadata.Name
					}
				}
			}
		}
	}
	for _, cr := range m.CRs {
		if cr.Version != "" && dep.CRVersion == "" {
			dep.CRVersion = cr.Version
		}
	}
	// PMM is wired in as a sidecar image rather than announced anywhere, so its
	// presence is inferred from the image actually running.
	for _, img := range m.Images {
		if strings.Contains(img.Repo, "pmm-client") {
			dep.PMM = img.Image
		}
	}
	if dep.Operator != "" || dep.CRVersion != "" || dep.PMM != "" {
		m.Deployment = dep
	}
}

// parseOpImages collects every distinct container image in use and who runs it.
// Init containers count: on the Percona operators the init container is what
// copies the operator's own binaries into the database pod, so a mismatched one
// is a real and otherwise invisible problem.
func parseOpImages(m *opModel, f opFiles) {
	type agg struct {
		users []string
		count int
	}
	byImage := map[string]*agg{}
	for _, ns := range f.namespaces() {
		if isSystemNamespace(ns) {
			continue
		}
		for _, raw := range f.items(ns + "/pods.yaml") {
			var p k8sPod
			if json.Unmarshal(raw, &p) != nil {
				continue
			}
			for _, c := range append(append([]k8sContainerSpec{}, p.Spec.InitContainers...), p.Spec.Containers...) {
				if c.Image == "" {
					continue
				}
				a, ok := byImage[c.Image]
				if !ok {
					a = &agg{}
					byImage[c.Image] = a
				}
				a.count++
				where := ns + "/" + p.Metadata.Name
				if len(a.users) < 6 && !containsStr(a.users, where) {
					a.users = append(a.users, where)
				}
			}
		}
	}
	for image, a := range byImage {
		repo, tag := splitImage(image)
		m.Images = append(m.Images, opImage{Image: image, Repo: repo, Tag: tag, Used: a.users, Count: a.count})
	}
	sort.Slice(m.Images, func(i, j int) bool { return m.Images[i].Image < m.Images[j].Image })
}

func containsStr(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// parseOpSecrets builds the secret *reference* graph: what the pods mount and what
// the custom resources name. The collector never dumps Secret objects, so there is
// no content here and there should not be — a missing or renamed secret is the
// failure worth catching, and the value would only be a liability in an archive
// that gets emailed around.
func parseOpSecrets(m *opModel, f opFiles) {
	type agg struct {
		kind  string
		users []string
	}
	seen := map[string]*agg{}
	note := func(name, kind, where string) {
		if name == "" {
			return
		}
		a, ok := seen[name]
		if !ok {
			a = &agg{kind: kind}
			seen[name] = a
		}
		if a.kind == "referenced" && kind != "referenced" {
			a.kind = kind
		}
		if where != "" && len(a.users) < 6 && !containsStr(a.users, where) {
			a.users = append(a.users, where)
		}
	}
	for _, ns := range f.namespaces() {
		if isSystemNamespace(ns) {
			continue
		}
		for _, raw := range f.items(ns + "/pods.yaml") {
			var p k8sPod
			if json.Unmarshal(raw, &p) != nil {
				continue
			}
			for _, v := range p.Spec.Volumes {
				if v.Secret != nil {
					note(v.Secret.SecretName, "mounted", ns+"/"+p.Metadata.Name)
				}
			}
		}
	}
	// The names a custom resource gives its secrets, which is how a typo in the CR
	// becomes a pod that will never start.
	for _, cr := range m.CRs {
		for _, name := range cr.SecretRefs {
			note(name, "referenced", cr.Namespace+"/"+cr.Name)
		}
	}
	// A cert file in the archive is named for its secret, so anything with parsed
	// certificate metadata is a TLS secret whatever else claimed it.
	for _, c := range m.Certs {
		note(c.Secret, "tls", "")
	}
	for name, a := range seen {
		m.Secrets = append(m.Secrets, opSecret{Name: name, Kind: a.kind, Used: a.users})
	}
	sort.Slice(m.Secrets, func(i, j int) bool { return m.Secrets[i].Name < m.Secrets[j].Name })
}

// crSecretNameKeys are the spec fields the Percona operators use to name a secret.
// Matched case-insensitively on the key, so this survives an operator adding
// another one without being taught about it.
var crSecretNameKeys = []string{
	"secretsname", "sslsecretname", "sslinternalsecretname", "vaultsecretname",
	"logcollectorsecretname", "credentialssecret", "secretname", "customusersecret",
	"encryptionkeysecretname", "passwordsecretname",
}

// crSecretRefs walks a custom resource's spec for anything that names a secret.
// The spec shapes differ per operator and change between versions, so this walks
// the decoded JSON rather than declaring a struct per operator — a field this does
// not know about is simply not reported, where a struct would have to be revised
// every release.
func crSecretRefs(spec json.RawMessage) []string {
	var root any
	if len(spec) == 0 || json.Unmarshal(spec, &root) != nil {
		return nil
	}
	seen := map[string]bool{}
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				if s, ok := child.(string); ok && s != "" {
					lk := strings.ToLower(k)
					for _, want := range crSecretNameKeys {
						if lk == want {
							seen[s] = true
						}
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(root)
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------- backups

// parseOpBackups reads the backup and restore custom resources. Their status
// shapes differ per operator, so this decodes the union of the fields they use
// rather than one struct per operator — verified against a live PXC backup
// (destination, image, s3.bucket, storageName, state) and a live PSMDB backup
// that failed (state, error).
func parseOpBackups(m *opModel, f opFiles) {
	for _, ns := range f.namespaces() {
		for _, file := range f.crFiles(ns) {
			stem := strings.TrimSuffix(file, ".yaml")
			plural, _, _ := strings.Cut(stem, ".")
			isRestore := strings.Contains(plural, "restore")
			if !isRestore && !strings.Contains(plural, "backup") {
				continue
			}
			for _, raw := range f.items(ns + "/" + file) {
				var item struct {
					Kind     string  `json:"kind"`
					Metadata k8sMeta `json:"metadata"`
					Spec     struct {
						ClusterName string `json:"clusterName"`
						PXCCluster  string `json:"pxcCluster"`
						PGCluster   string `json:"pgCluster"`
						StorageName string `json:"storageName"`
						BackupName  string `json:"backupName"`
					} `json:"spec"`
					Status struct {
						State          string `json:"state"`
						Destination    string `json:"destination"`
						StorageName    string `json:"storageName"`
						StorageType    string `json:"storage_type"`
						Image          string `json:"image"`
						Error          string `json:"error"`
						Completed      string `json:"completed"`
						LastTransition string `json:"lastTransition"`
					} `json:"status"`
				}
				if json.Unmarshal(raw, &item) != nil {
					continue
				}
				b := opBackup{
					Kind:        firstNonEmpty(item.Kind, plural),
					Namespace:   ns,
					Name:        item.Metadata.Name,
					Cluster:     firstNonEmpty(item.Spec.ClusterName, item.Spec.PXCCluster, item.Spec.PGCluster),
					State:       item.Status.State,
					Storage:     firstNonEmpty(item.Status.StorageName, item.Spec.StorageName),
					StorageType: item.Status.StorageType,
					Destination: item.Status.Destination,
					Image:       item.Status.Image,
					Started:     item.Metadata.CreationTimestamp,
					Completed:   firstNonEmpty(item.Status.Completed, item.Status.LastTransition),
					Error:       item.Status.Error,
					Restore:     isRestore,
				}
				m.Backups = append(m.Backups, b)
			}
		}
	}
	// Newest first: the run you are asking about is almost always the last one.
	sort.Slice(m.Backups, func(i, j int) bool { return m.Backups[i].Started > m.Backups[j].Started })
}

// backupFailed reports whether a backup/restore state means it did not work. The
// operators disagree on spelling, so this matches on substance.
func backupFailed(state string) bool {
	s := strings.ToLower(state)
	return s == "error" || s == "failed" || strings.Contains(s, "fail")
}

func backupRunning(state string) bool {
	s := strings.ToLower(state)
	return s == "running" || s == "starting" || s == "requested" || s == "waiting"
}

// ---------------------------------------------------------------- certificates

// certEntryRe matches the bare key line the collector writes before each
// certificate in a TLS secret's file — "ca.crt", "tls.crt".
var certEntryRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

var (
	certNotAfterRe = regexp.MustCompile(`Not After\s*:\s*(.+)`)
	certSubjectRe  = regexp.MustCompile(`^\s*Subject:\s*(.+)$`)
	certIssuerRe   = regexp.MustCompile(`^\s*Issuer:\s*(.+)$`)
)

// parseOpCerts reads the certificate metadata the collector renders for each TLS
// secret. The file is named for the secret and holds one openssl text block per
// key in it, so this is the only place in the archive that answers "when does this
// cluster's TLS expire" — a question with a hard deadline attached.
func parseOpCerts(m *opModel, f opFiles) {
	for name, data := range f {
		parts := strings.Split(name, "/")
		// <namespace>/<secret> — a bare file, not a resource dump and not a pod dir.
		if len(parts) != 2 || strings.Contains(parts[1], ".yaml") || parts[1] == "errors.txt" {
			continue
		}
		text := string(data)
		if !strings.Contains(text, "Certificate:") {
			continue
		}
		ns, secret := parts[0], parts[1]
		entry := ""
		var cur *opCert
		flush := func() {
			if cur != nil && cur.NotAfter != "" {
				m.Certs = append(m.Certs, *cur)
			}
			cur = nil
		}
		for _, line := range strings.Split(text, "\n") {
			trimmed := strings.TrimSpace(line)
			// A bare, unindented token is the next key in the secret.
			if line == trimmed && certEntryRe.MatchString(trimmed) && !strings.HasPrefix(trimmed, "Certificate") {
				flush()
				entry = trimmed
				continue
			}
			if strings.HasPrefix(trimmed, "Certificate:") {
				flush()
				cur = &opCert{Secret: secret, Namespace: ns, Entry: entry}
				continue
			}
			if cur == nil {
				continue
			}
			if mt := certSubjectRe.FindStringSubmatch(line); mt != nil && cur.Subject == "" {
				cur.Subject = strings.TrimSpace(mt[1])
			}
			if mt := certIssuerRe.FindStringSubmatch(line); mt != nil && cur.Issuer == "" {
				cur.Issuer = strings.TrimSpace(mt[1])
			}
			if mt := certNotAfterRe.FindStringSubmatch(line); mt != nil && cur.NotAfter == "" {
				cur.NotAfter = strings.TrimSpace(mt[1])
				if t, err := time.Parse("Jan _2 15:04:05 2006 MST", cur.NotAfter); err == nil {
					d := int(time.Until(t).Hours() / 24)
					cur.DaysLeft = &d
				}
			}
		}
		flush()
	}
	for i := range m.Certs {
		m.Certs[i].SelfSigned = m.Certs[i].Issuer == m.Certs[i].Subject ||
			strings.Contains(strings.ToLower(m.Certs[i].Issuer), "root ca")
	}
	sort.Slice(m.Certs, func(i, j int) bool {
		a, b := m.Certs[i], m.Certs[j]
		// Soonest to expire first — this panel exists for that one number.
		if a.DaysLeft != nil && b.DaysLeft != nil && *a.DaysLeft != *b.DaysLeft {
			return *a.DaysLeft < *b.DaysLeft
		}
		return a.Secret+a.Entry < b.Secret+b.Entry
	})
}

// ---------------------------------------------------------------- storage

func parseOpStorage(m *opModel, f opFiles) {
	for _, ns := range f.namespaces() {
		for _, raw := range f.items(ns + "/persistentvolumeclaims.yaml") {
			var c k8sPVC
			if json.Unmarshal(raw, &c) != nil {
				continue
			}
			pvc := opPVC{
				Namespace: ns, Name: c.Metadata.Name,
				Status:    c.Status.Phase,
				Capacity:  c.Status.Capacity["storage"],
				Requested: c.Spec.Resources.Requests["storage"],
				Volume:    c.Spec.VolumeName,
			}
			if c.Spec.StorageClassName != nil {
				pvc.StorageClass = *c.Spec.StorageClassName
			}
			m.Storage = append(m.Storage, pvc)
		}
	}
	sort.Slice(m.Storage, func(i, j int) bool {
		a, b := m.Storage[i], m.Storage[j]
		// Anything not Bound first: a Pending claim is why a database will not start.
		if (a.Status == "Bound") != (b.Status == "Bound") {
			return a.Status != "Bound"
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
}

// ---------------------------------------------------------------- galera state

// parseOpGalera reads the two files that decide whether a stopped PXC cluster can
// come back: grastate.dat, which carries each member's last committed seqno and
// its safe_to_bootstrap flag, and gvwstate.dat, which exists only while a member
// holds a view. Nothing else in the archive answers "can this cluster bootstrap",
// and getting it wrong means bootstrapping from the wrong node and losing writes.
func parseOpGalera(m *opModel, f opFiles) {
	for name, data := range f {
		if path.Base(name) != "grastate.dat" {
			continue
		}
		parts := strings.Split(name, "/")
		if len(parts) < 3 {
			continue
		}
		g := opGalera{Namespace: parts[0], Pod: parts[1]}
		for _, line := range strings.Split(string(data), "\n") {
			k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok {
				continue
			}
			v = strings.TrimSpace(v)
			switch strings.TrimSpace(k) {
			case "uuid":
				g.UUID = v
			case "seqno":
				g.Seqno = v
			case "safe_to_bootstrap":
				g.SafeToBootstrap = v == "1"
			}
		}
		// gvwstate.dat is written while a member is in a primary view and removed
		// on a clean shutdown, so its presence is itself information.
		g.HasView = len(f[path.Dir(name)+"/gvwstate.dat"]) > 0
		m.Galera = append(m.Galera, g)
	}
	sort.Slice(m.Galera, func(i, j int) bool { return m.Galera[i].Pod < m.Galera[j].Pod })
}

// ---------------------------------------------------------------- pod summaries

// opSummaryFields are the lines worth lifting out of a pt-mysql-summary /
// pt-mongodb-summary / pg_gather report. The reports are long and mostly of
// interest inside Stalk Summary; what belongs here is the handful of facts that
// identify what is actually running.
var opSummaryFields = []string{
	"Version", "Started", "Databases", "Datadir", "Processes", "Replication",
	"SSL", "Uptime", "Storage Engines",
}

// parseOpPodSummaries reads the per-pod database summary the collector produces by
// port-forwarding into each database pod. It is the only view of the *database*
// in the whole archive — everything else is Kubernetes' opinion of it.
func parseOpPodSummaries(m *opModel, f opFiles) {
	for name, data := range f {
		if path.Base(name) != "summary.txt" {
			continue
		}
		parts := strings.Split(name, "/")
		if len(parts) < 3 || isSystemNamespace(parts[0]) {
			continue
		}
		s := opPodSummary{Namespace: parts[0], Pod: parts[1], Facts: map[string]string{}}
		for _, line := range strings.Split(string(data), "\n") {
			k, v, ok := strings.Cut(line, "|")
			if !ok {
				continue
			}
			key, val := strings.TrimSpace(k), strings.TrimSpace(v)
			for _, want := range opSummaryFields {
				if key == want && val != "" && s.Facts[key] == "" {
					s.Facts[key] = truncate(val, 200)
				}
			}
		}
		// wsrep_cluster_size and friends are the PXC-specific half, written as a
		// bare two-column table rather than the pipe-separated block above.
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && strings.HasPrefix(fields[0], "wsrep_") {
				switch fields[0] {
				case "wsrep_cluster_size", "wsrep_cluster_status", "wsrep_local_state_comment",
					"wsrep_last_committed", "wsrep_last_applied", "wsrep_protocol_version":
					s.Facts[fields[0]] = fields[1]
				}
			}
		}
		if len(s.Facts) > 0 {
			m.PodSummaries = append(m.PodSummaries, s)
		}
	}
	sort.Slice(m.PodSummaries, func(i, j int) bool { return m.PodSummaries[i].Pod < m.PodSummaries[j].Pod })
}

// ---------------------------------------------------------------- backup logs

// parseOpBackupLogs reads the xtrabackup logs the collector lifts out of each PXC
// member. This is where a backup actually says why it failed — the finding that
// prompted this panel was `xbcloud: Probe failed. Please check your credentials
// and endpoint settings`, which appears here and in no custom resource.
// opBackupLogFailRe matches a line that is genuinely a failure, not a line that
// merely contains the word. The first cut matched "error" anywhere, which flagged
// every `performance_schema/events_errors_su_152.sdi` xtrabackup streamed — a
// table name — in a log that ended `completed OK!`. A panel that cries wolf on a
// successful backup is worse than no panel.
var opBackupLogFailRe = regexp.MustCompile(`(?i)\[ERROR\]|\bFATAL\b|xbcloud: (?:Probe )?[Ff]ailed|Probe failed|error: http request failed|Couldn't connect to server|failed to (?:connect|upload|read|write)|Access Denied|NoSuchBucket|InvalidAccessKeyId`)

// opBackupLogCollectorNoise matches the collector's own failure to read a file
// that does not exist. innobackup.prepare.log and innobackup.move.log are only
// written during a restore, so on a cluster that has never restored the collector
// tars nothing and records `tar: … Cannot stat`. That is the collector missing a
// file, not the database failing — reporting it would be a standing false alarm.
var opBackupLogCollectorNoise = regexp.MustCompile(`^tar: .*(Cannot stat|Exiting with failure status)`)

// parseOpBackupLogs reads the xtrabackup logs the collector lifts off each PXC
// member. This is where a backup says why it failed — the finding that prompted
// the panel was `xbcloud: Probe failed. Please check your credentials and endpoint
// settings`, which appears here and in no custom resource.
func parseOpBackupLogs(m *opModel, f opFiles) {
	for name, data := range f {
		base := path.Base(name)
		if !strings.HasPrefix(base, "innobackup.") || !strings.HasSuffix(base, ".log") {
			continue
		}
		parts := strings.Split(name, "/")
		if len(parts) < 3 || len(data) == 0 {
			continue
		}
		text := string(data)
		lines := strings.Split(strings.TrimRight(text, "\n"), "\n")

		// A file holding nothing but the collector's own tar complaint is a file
		// that was never written, and belongs in neither this panel nor the
		// findings — it says nothing about the cluster.
		real := 0
		for _, ln := range lines {
			if strings.TrimSpace(ln) != "" && !opBackupLogCollectorNoise.MatchString(strings.TrimSpace(ln)) {
				real++
			}
		}
		if real == 0 {
			continue
		}

		l := opBackupLog{Namespace: parts[0], Pod: parts[1], File: base, Bytes: len(data)}
		l.CompletedOK = strings.Contains(text, "completed OK!")
		for _, line := range lines {
			if opBackupLogFailRe.MatchString(line) && !opBackupLogCollectorNoise.MatchString(strings.TrimSpace(line)) {
				l.Errors = append(l.Errors, truncate(strings.TrimSpace(line), 240))
			}
		}
		if len(l.Errors) > 8 {
			l.Errors = l.Errors[len(l.Errors)-8:]
		}
		l.Tail = tailLines(text, 6)
		m.BackupLogs = append(m.BackupLogs, l)
	}
	sort.Slice(m.BackupLogs, func(i, j int) bool {
		a, b := m.BackupLogs[i], m.BackupLogs[j]
		if (len(a.Errors) > 0) != (len(b.Errors) > 0) {
			return len(a.Errors) > 0
		}
		return a.Pod+a.File < b.Pod+b.File
	})
}

// ------------------------------------------------- schedules, config, rollouts

func parseOpSchedules(m *opModel, f opFiles) {
	for _, ns := range f.namespaces() {
		if isSystemNamespace(ns) {
			continue
		}
		for _, raw := range f.items(ns + "/cronjobs.yaml") {
			var c struct {
				Metadata k8sMeta `json:"metadata"`
				Spec     struct {
					Schedule string `json:"schedule"`
					Suspend  *bool  `json:"suspend"`
				} `json:"spec"`
				Status struct {
					LastScheduleTime string            `json:"lastScheduleTime"`
					Active           []json.RawMessage `json:"active"`
				} `json:"status"`
			}
			if json.Unmarshal(raw, &c) != nil {
				continue
			}
			s := opSchedule{
				Namespace: ns, Name: c.Metadata.Name, Schedule: c.Spec.Schedule,
				LastRun: c.Status.LastScheduleTime, Active: len(c.Status.Active),
			}
			if c.Spec.Suspend != nil {
				s.Suspended = *c.Spec.Suspend
			}
			m.Schedules = append(m.Schedules, s)
		}
	}
	sort.Slice(m.Schedules, func(i, j int) bool { return m.Schedules[i].Name < m.Schedules[j].Name })
}

// opConfigInteresting keeps the panel to configuration a reader would act on. A
// namespace holds dozens of ConfigMaps and most are Kubernetes' own bookkeeping.
func opConfigInteresting(name string, keys []string) bool {
	for _, k := range keys {
		switch {
		case strings.HasSuffix(k, ".cnf"), strings.HasSuffix(k, ".conf"),
			strings.HasSuffix(k, ".yaml"), strings.HasSuffix(k, ".yml"),
			strings.HasSuffix(k, ".ini"), strings.Contains(k, "mongod"), strings.Contains(k, "postgres"):
			return true
		}
	}
	return strings.Contains(name, "config") || strings.Contains(name, "cnf")
}

func parseOpConfig(m *opModel, f opFiles) {
	for _, ns := range f.namespaces() {
		if isSystemNamespace(ns) {
			continue
		}
		for _, raw := range f.items(ns + "/configmaps.yaml") {
			var c struct {
				Metadata k8sMeta           `json:"metadata"`
				Data     map[string]string `json:"data"`
			}
			if json.Unmarshal(raw, &c) != nil || len(c.Data) == 0 {
				continue
			}
			keys := make([]string, 0, len(c.Data))
			for k := range c.Data {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if !opConfigInteresting(c.Metadata.Name, keys) {
				continue
			}
			cfg := opConfig{Namespace: ns, Name: c.Metadata.Name, Keys: keys}
			// One excerpt, from the largest key: enough to recognise the config
			// without turning the panel into a file browser.
			best := ""
			for _, k := range keys {
				if len(c.Data[k]) > len(best) {
					best = c.Data[k]
				}
			}
			cfg.Excerpt = truncate(strings.TrimSpace(best), 1200)
			m.Config = append(m.Config, cfg)
		}
	}
	sort.Slice(m.Config, func(i, j int) bool { return m.Config[i].Name < m.Config[j].Name })
}

func parseOpRollouts(m *opModel, f opFiles) {
	type agg struct{ revisions, stuck int }
	byOwner := map[string]*agg{}
	for _, ns := range f.namespaces() {
		if isSystemNamespace(ns) {
			continue
		}
		for _, raw := range f.items(ns + "/replicasets.yaml") {
			var rs struct {
				Metadata struct {
					Name            string `json:"name"`
					Namespace       string `json:"namespace"`
					OwnerReferences []struct {
						Kind string `json:"kind"`
						Name string `json:"name"`
					} `json:"ownerReferences"`
				} `json:"metadata"`
				Spec struct {
					Replicas *int `json:"replicas"`
				} `json:"spec"`
				Status struct {
					Replicas int `json:"replicas"`
				} `json:"status"`
			}
			if json.Unmarshal(raw, &rs) != nil {
				continue
			}
			owner := rs.Metadata.Name
			for _, o := range rs.Metadata.OwnerReferences {
				if o.Kind == "Deployment" {
					owner = o.Name
				}
			}
			key := ns + "/" + owner
			a, ok := byOwner[key]
			if !ok {
				a = &agg{}
				byOwner[key] = a
			}
			a.revisions++
			// An old revision that still holds pods is what a stuck rollout is.
			if rs.Spec.Replicas != nil && *rs.Spec.Replicas == 0 && rs.Status.Replicas > 0 {
				a.stuck++
			}
		}
	}
	for key, a := range byOwner {
		ns, owner, _ := strings.Cut(key, "/")
		m.Rollouts = append(m.Rollouts, opRollout{Namespace: ns, Owner: owner, Revisions: a.revisions, Stuck: a.stuck})
	}
	sort.Slice(m.Rollouts, func(i, j int) bool { return m.Rollouts[i].Owner < m.Rollouts[j].Owner })
}

func parseOpBudgets(m *opModel, f opFiles) {
	for _, ns := range f.namespaces() {
		if isSystemNamespace(ns) {
			continue
		}
		for _, raw := range f.items(ns + "/poddisruptionbudgets.yaml") {
			var p struct {
				Metadata k8sMeta `json:"metadata"`
				Spec     struct {
					MinAvailable   json.RawMessage `json:"minAvailable"`
					MaxUnavailable json.RawMessage `json:"maxUnavailable"`
				} `json:"spec"`
				Status struct {
					CurrentHealthy     int `json:"currentHealthy"`
					DesiredHealthy     int `json:"desiredHealthy"`
					DisruptionsAllowed int `json:"disruptionsAllowed"`
				} `json:"status"`
			}
			if json.Unmarshal(raw, &p) != nil {
				continue
			}
			m.Budgets = append(m.Budgets, opBudget{
				Namespace: ns, Name: p.Metadata.Name,
				MinAvailable:   strings.Trim(string(p.Spec.MinAvailable), `"`),
				MaxUnavailable: strings.Trim(string(p.Spec.MaxUnavailable), `"`),
				Healthy:        p.Status.CurrentHealthy, Desired: p.Status.DesiredHealthy,
				Disruptions: p.Status.DisruptionsAllowed,
			})
		}
	}
	sort.Slice(m.Budgets, func(i, j int) bool { return m.Budgets[i].Name < m.Budgets[j].Name })
}

func parseOpRBAC(m *opModel, f opFiles) {
	for _, ns := range f.namespaces() {
		if isSystemNamespace(ns) {
			continue
		}
		for kind, file := range map[string]string{"Role": "roles.yaml", "ClusterRole": "clusterroles.yaml"} {
			for _, raw := range f.items(ns + "/" + file) {
				var r struct {
					Metadata k8sMeta `json:"metadata"`
					Rules    []struct {
						APIGroups []string `json:"apiGroups"`
						Resources []string `json:"resources"`
						Verbs     []string `json:"verbs"`
					} `json:"rules"`
				}
				if json.Unmarshal(raw, &r) != nil {
					continue
				}
				// Only the operator's own roles: a namespace's full RBAC is noise,
				// and it is the operator's permissions that explain its failures.
				if !strings.Contains(r.Metadata.Name, "percona") && !strings.Contains(r.Metadata.Name, "operator") {
					continue
				}
				entry := opRBAC{Namespace: ns, Kind: kind, Name: r.Metadata.Name, Rules: len(r.Rules)}
				for _, rule := range r.Rules {
					if containsStr(rule.Verbs, "*") || containsStr(rule.Resources, "*") {
						entry.Wildcard = true
					}
				}
				m.RBAC = append(m.RBAC, entry)
			}
		}
	}
	sort.Slice(m.RBAC, func(i, j int) bool { return m.RBAC[i].Name < m.RBAC[j].Name })
}

// crComponentsFrom reads one status sub-object into components. The operators use
// two shapes for the same idea and both have to work:
//
//	pxc:     status.pxc      = {size, ready, status}            — one component
//	psmdb:   status.replsets = {rs0: {size, ready, status, …}}  — a map of them
//
// Verified against live 1.20.0 and 1.23.0 deployments. Reading only the first
// shape is why a PSMDB cluster reported no components at all while its replica set
// sat at 3/3 — the panel was empty and said nothing was wrong with it.
func crComponentsFrom(key string, raw json.RawMessage) []opCRComponent {
	var flat crComponentStatus
	if json.Unmarshal(raw, &flat) == nil && (flat.Size > 0 || flat.Ready > 0 || flat.Status != "") {
		return []opCRComponent{{Name: key, Ready: flat.Ready, Size: flat.Size, Status: flat.Status}}
	}
	var nested map[string]crComponentStatus
	if json.Unmarshal(raw, &nested) != nil {
		return nil
	}
	names := make([]string, 0, len(nested))
	for name := range nested {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []opCRComponent
	for _, name := range names {
		cs := nested[name]
		if cs.Size == 0 && cs.Ready == 0 && cs.Status == "" {
			continue
		}
		out = append(out, opCRComponent{Name: key + "/" + name, Ready: cs.Ready, Size: cs.Size, Status: cs.Status})
	}
	return out
}

// ------------------------------------------------- what the capture is missing

// opServerLogFiles are the database server logs pt-k8s-debug-collector copies off
// a pod's filesystem. The list is short because the collector's is: it fetches
// these for the MySQL family and nothing equivalent for MongoDB or PostgreSQL.
var opServerLogFiles = []string{"mysqld-error.log", "mysqld.post.processing.log"}

// noteMissingServerLogs says out loud what a capture does not contain.
//
// A pod's logs.txt is `kubectl logs <pod> --all-containers` — every container's
// stdout concatenated, unprefixed. That is not the same as the database's log:
// PSMDB routes mongod's output to /data/db/logs/mongod.log when the logcollector
// sidecar is enabled, so its stdout contributes almost nothing and the file is
// dominated by PBM and logrotate. The only reason a PXC capture has a real server
// log is that the collector copies /var/lib/mysql/*.log off the pod filesystem
// separately — there is no equivalent for the other engines.
//
// Left implicit this reads as "the logs were fine". It is not a finding about the
// cluster, so it is a note — but a summary that quietly analyses the wrong file is
// worse than one that says which file it could not get.
func noteMissingServerLogs(m *opModel, f opFiles) {
	for _, ns := range f.namespaces() {
		if isSystemNamespace(ns) {
			continue
		}
		for _, raw := range f.items(ns + "/pods.yaml") {
			var p k8sPod
			if json.Unmarshal(raw, &p) != nil || len(p.Spec.Containers) < 2 {
				continue
			}
			// Only pods that look like a database: an operator pod has one
			// container and its stdout *is* its log.
			engine := ""
			for _, c := range p.Spec.Containers {
				switch c.Name {
				case "mongod":
					engine = "mongod"
				case "database":
					engine = "postgres"
				case "pxc", "mysql":
					engine = "mysql"
				}
			}
			if engine == "" || engine == "mysql" {
				continue // the MySQL family's server log is collected off disk
			}
			have := false
			for _, want := range opServerLogFiles {
				if len(f[ns+"/"+p.Metadata.Name+"/"+want]) > 0 {
					have = true
				}
			}
			if have {
				continue
			}
			names := make([]string, 0, len(p.Spec.Containers))
			for _, c := range p.Spec.Containers {
				names = append(names, c.Name)
			}
			m.Findings = append(m.Findings, opFinding{
				Severity: "note",
				Title:    fmt.Sprintf("No %s server log for %s in this capture", engine, p.Metadata.Name),
				Detail: "pt-k8s-debug-collector copies the MySQL family's server logs off the pod filesystem but takes only one container's stdout for other engines — and not predictably the first. This pod runs " +
					strings.Join(names, ", ") + ", so whatever its logs.txt holds, it is not the " + engine + " log.",
				Where: ns + "/" + p.Metadata.Name,
			})
		}
	}
}
