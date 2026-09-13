package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"sigs.k8s.io/yaml"
)

// k3dstatesdump.go — the Kubernetes States board, built from a pt-k8s-debug-collector
// cluster-dump instead of from a live cluster.
//
// The board and the archive turn out to want exactly the same thing. `kubectl get -o json`
// and the collector's `<namespace>/<resource>.yaml` are the same objects in two encodings —
// the files are Kubernetes Lists whose items carry their own `kind`, and sigs.k8s.io/yaml
// (already here for Operator Summary) converts them to JSON on the way in. So every rule in
// k3dstates.go applies unchanged: the same pods come out red for the same reasons, with the
// same toned rows, and the same warning events hang off them.
//
// What an archive cannot do, and the page says so rather than pretending:
//
//   - IT IS ONE INSTANT. Nothing is "changing" and nothing has "disappeared", because there
//     is no next sample. Load a second capture of the same cluster with Compare on and the
//     board answers both questions at once — that is what the model was already doing
//     between two live samples, and two captures are two samples a long way apart.
//   - THERE ARE NO SERVICES. The collector does not write a services.yaml, so that column is
//     simply absent. Everything else the board draws is in the archive.
//   - A POD HAS ONE LOG, NOT ONE PER CONTAINER. The collector writes `<pod>/logs.txt`, which
//     is the pod's default container. It also writes what it found on disk — a PXC pod's
//     `var/lib/mysql/mysqld-error.log`, its `summary.txt` — and those are offered beside it,
//     which is more than the live board can do.
//
// Two sources, one path: a capture kept by Diagnostics (k8sdiag.go) is addressed by its id,
// and an archive uploaded from a host is held in memory for the session that uploaded it —
// see k3dStateUploads for why it is not written to disk.

// k3dArchiveLayout is what one cluster-dump contains, worked out by reading the collector
// (percona-toolkit/src/go/pt-k8s-debug-collector) rather than by guessing from one example.
// The paths come from its paths.go, and the two layouts below are both in the wild:
//
//	errors.txt                              what the collector itself failed to collect
//	cluster-scope/<resource>.yaml           cluster-scoped resources (newer collectors)
//	<resource>.yaml                         ...at the root instead (older ones)
//	<ns>/<resource>.yaml                    every namespaced resource that had items
//	<ns>/secrets/<name>.yaml                secrets (newer)
//	<ns>/<secret-name>                      a TLS secret's certificate, `openssl x509 -text`
//	<ns>/<pod>/<container>.log              one log PER CONTAINER (newer collectors)
//	<ns>/<pod>/logs.txt                     ...one log per pod instead (older ones)
//	<ns>/<pod>/summary.txt                  pt-mysql-summary · pg_gather · pt-mongodb-summary
//	<ns>/<pod>/var/lib/mysql/…              PXC: mysqld-error.log, the innobackup.*.log
//	                                        backup logs, grastate.dat, gvwstate.dat, auto.cnf
//	<ns>/<pod>/pg_log/… · pgbackrest_log/…  PG: server logs and pgBackRest's own logs
//	<ns>/<pod>/pgbackrest-info.log          PG: `pgbackrest info` at capture time
//	<ns>/<pod>/patronictl-list.log          PG: `patronictl list` at capture time
//
// Nothing here is a fixed list of interesting kinds. The collector discovers every resource
// the API server serves and writes the ones that had items, so the board reads every *.yaml
// it finds and makes a card of every object in it — which is how a capture of an operator
// this code has never seen still comes out complete.
type k3dArchiveLayout struct {
	Resources  []string            // every resource List in the archive
	Events     []string            // the event Lists among them
	Pods       map[string][]string // namespace → pod names (from each namespace's pods.yaml)
	PodFiles   map[string][]string // "<ns>/<pod>" → files kept for that pod
	OtherFiles []string            // everything else: errors.txt, certificates, strays
	Namespaces []string
}

// k3dArchiveItems reads one file's objects. Most of the archive is Kubernetes Lists, which
// opFiles.items already handles — but not all of it: the collector writes each secret as a
// SINGLE object at <ns>/secrets/<name>.yaml (its paths.go), and a List reader returns nothing
// for those. Missing them would mean a capture whose secrets are simply not on the board.
func k3dArchiveItems(f opFiles, file string) []json.RawMessage {
	if items := f.items(file); len(items) > 0 {
		return items
	}
	data := f[file]
	if len(data) == 0 {
		return nil
	}
	j, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil
	}
	var probe struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if json.Unmarshal(j, &probe) != nil || probe.Metadata.Name == "" || probe.Kind == "List" {
		return nil // an empty List, or something that is not an object at all
	}
	return []json.RawMessage{j}
}

// k3dReadArchiveLayout walks the archive once and says what is in it.
func k3dReadArchiveLayout(f opFiles) k3dArchiveLayout {
	l := k3dArchiveLayout{Pods: map[string][]string{}, PodFiles: map[string][]string{}}

	// The pod directories are named after pods, so the pod lists have to be read first —
	// that is what tells `<ns>/k3d-00-pxc-0/…` (a pod's files) from `<ns>/secrets/…`.
	nsSeen := map[string]bool{}
	podOf := map[string]bool{} // "<ns>/<pod>"
	for name := range f {
		dir, base := path.Split(name)
		ns := strings.Trim(dir, "/")
		if base != "pods.yaml" || ns == "" || strings.Contains(ns, "/") {
			continue
		}
		nsSeen[ns] = true
		for _, raw := range k3dArchiveItems(f, name) {
			var it k3dRawItem
			if json.Unmarshal(raw, &it) == nil && it.Metadata.Name != "" {
				l.Pods[ns] = append(l.Pods[ns], it.Metadata.Name)
				podOf[ns+"/"+it.Metadata.Name] = true
			}
		}
	}

	for name := range f {
		switch {
		case strings.HasSuffix(name, ".yaml") && !k3dArchiveUnderPod(name, podOf):
			l.Resources = append(l.Resources, name)
			if base := path.Base(name); base == "events.yaml" || strings.HasPrefix(base, "events.") {
				l.Events = append(l.Events, name)
			}
			if ns := path.Dir(name); ns != "." && !strings.Contains(ns, "/") && ns != "cluster-scope" {
				nsSeen[ns] = true
			}
		default:
			if key := k3dArchivePodOf(name, podOf); key != "" {
				l.PodFiles[key] = append(l.PodFiles[key], strings.TrimPrefix(name, key+"/"))
				continue
			}
			l.OtherFiles = append(l.OtherFiles, name)
		}
	}
	for ns := range nsSeen {
		l.Namespaces = append(l.Namespaces, ns)
	}
	sort.Strings(l.Resources)
	sort.Strings(l.Events)
	sort.Strings(l.OtherFiles)
	sort.Strings(l.Namespaces)
	for k := range l.PodFiles {
		sort.Slice(l.PodFiles[k], func(i, j int) bool { return k3dPodFileLess(l.PodFiles[k][i], l.PodFiles[k][j]) })
	}
	for ns := range l.Pods {
		sort.Strings(l.Pods[ns])
	}
	return l
}

// k3dArchivePodOf returns the "<ns>/<pod>" a file belongs to, or "" when it belongs to none.
func k3dArchivePodOf(name string, pods map[string]bool) string {
	parts := strings.Split(name, "/")
	if len(parts) < 3 {
		return ""
	}
	key := parts[0] + "/" + parts[1]
	if pods[key] {
		return key
	}
	return ""
}

func k3dArchiveUnderPod(name string, pods map[string]bool) bool {
	return k3dArchivePodOf(name, pods) != ""
}

// k3dPodFileLess orders a pod's kept files the way somebody opens them: its log first
// (whichever layout the collector used), then the summary, then everything it pulled off
// disk — the backup logs, the Galera state files, pgBackRest's own log.
func k3dPodFileLess(a, b string) bool {
	rank := func(s string) int {
		switch {
		case s == "logs.txt":
			return 0
		case !strings.Contains(s, "/") && strings.HasSuffix(s, ".log") && !strings.Contains(s, "-"):
			return 1 // <container>.log, the newer per-container layout
		case s == "summary.txt":
			return 2
		case strings.Contains(s, "backup") || strings.Contains(s, "backrest"):
			return 3 // the backup logs, which is what people come here for
		default:
			return 4
		}
	}
	if ra, rb := rank(a), rank(b); ra != rb {
		return ra < rb
	}
	return a < b
}

// k3dResourceKind guesses the Kind for a resource file whose items do not carry one (the
// collector strips little, but an older one or a hand-made archive might). "pods.yaml" → Pod,
// "perconaxtradbclusters.pxc.percona.com.yaml" → Perconaxtradbcluster. Only a fallback: the
// item's own kind wins whenever it is there.
func k3dResourceKind(file string) string {
	base := strings.TrimSuffix(path.Base(file), ".yaml")
	if i := strings.Index(base, "."); i > 0 {
		base = base[:i]
	}
	base = strings.TrimSuffix(base, "s")
	if base == "" {
		return ""
	}
	return strings.ToUpper(base[:1]) + base[1:]
}

// k3dStatesFromArchive builds one sample out of a cluster-dump: the same objects, the same
// tones, the same properties the live board shows.
func k3dStatesFromArchive(f opFiles) (objs []k3dStateObj, namespaces, kinds, warnings []string) {
	l := k3dReadArchiveLayout(f)
	events := map[string]bool{}
	for _, e := range l.Events {
		events[e] = true
	}

	for _, file := range l.Resources {
		if events[file] {
			continue // events are what a card says about itself, not a card
		}
		fallback := k3dResourceKind(file)
		for _, raw := range k3dArchiveItems(f, file) {
			var it k3dRawItem
			if json.Unmarshal(raw, &it) != nil || it.Metadata.Name == "" {
				continue
			}
			if it.Kind == "" {
				it.Kind = fallback
			}
			// A capture strips creationTimestamp, uid and resourceVersion from every object
			// (see the collector's exportResource), so the namespace has to come from where
			// the file sits when the object no longer says.
			if it.Metadata.Namespace == "" {
				if ns := path.Dir(file); ns != "." && ns != "cluster-scope" && !strings.Contains(ns, "/") {
					it.Metadata.Namespace = ns
				}
			}
			objs = append(objs, k3dStateOf(it))
		}
	}
	sortK3DStates(objs)

	// Events are per namespace here rather than one cluster-wide list, so they are gathered
	// into one document of the shape k3dAttachEvents already reads.
	var evItems []json.RawMessage
	for _, file := range l.Events {
		evItems = append(evItems, k3dArchiveItems(f, file)...)
	}
	if len(evItems) > 0 {
		if doc, err := json.Marshal(map[string]any{"items": evItems}); err == nil {
			k3dAttachEvents(objs, doc, k3dStateMaxEvents)
		}
	}

	nsSeen, kindSeen := map[string]bool{}, map[string]bool{}
	for _, o := range objs {
		if o.Namespace != "" {
			nsSeen[o.Namespace] = true
		}
		kindSeen[o.Kind] = true
	}
	// Say what an archive cannot show, once, where the board shows the other warnings. A
	// column that is missing because nothing collected it looks exactly like a column that
	// is missing because the cluster has none.
	warnings = append(warnings, "a cluster-dump is one instant: nothing here is changing, and nothing has disappeared")
	// And say what the CAPTURE ITSELF could not collect. errors.txt is the collector's own
	// log of what failed while it ran, and a board that quietly shows fewer objects because
	// half the cluster was unreachable is exactly the lie the warning above exists to avoid.
	// Real captures have entries in it routinely ("kubectl port-forward / signal: killed"),
	// so it is reported as one line with a count rather than pasted in full.
	if n, first := k3dArchiveCollectorErrors(f); n > 0 {
		warnings = append(warnings, fmt.Sprintf("the capture itself reported %d collection error(s) — first: %s", n, first))
	}
	if len(objs) == 0 {
		warnings = append(warnings, "no objects in this archive — is it a pt-k8s-debug-collector cluster-dump?")
	}
	return objs, sortedBoolKeys(nsSeen), sortedBoolKeys(kindSeen), warnings
}

// k3dArchiveCollectorErrors reads errors.txt — what pt-k8s-debug-collector could not get.
// Its entries are paragraphs of "<command>\n<failure>", so the count is paragraphs and the
// first line of the first one is what the board quotes.
func k3dArchiveCollectorErrors(f opFiles) (int, string) {
	body := strings.TrimSpace(string(f["errors.txt"]))
	if body == "" {
		return 0, ""
	}
	var entries []string
	for _, block := range strings.Split(body, "\n\n") {
		if b := strings.TrimSpace(block); b != "" {
			entries = append(entries, strings.Join(strings.Fields(strings.ReplaceAll(b, "\n", " ")), " "))
		}
	}
	if len(entries) == 0 {
		return 0, ""
	}
	// Trimmed by RUNES, not bytes: a collector error can carry a container name or a path
	// with anything in it, and cutting a multi-byte character in half puts a replacement
	// glyph in the middle of the board's header.
	first := []rune(entries[0])
	if len(first) > 120 {
		first = append(first[:119], '…')
	}
	return len(entries), string(first)
}

// k3dArchiveManifest finds one object in the archive and re-emits it as YAML, so the board's
// YAML pane reads the same whether it is looking at a cluster or a capture of one.
func k3dArchiveManifest(f opFiles, kind, namespace, name string) (string, error) {
	want := strings.ToLower(kind)
	l := k3dReadArchiveLayout(f)
	// Every resource file that could hold it: the ones in its namespace, plus the
	// cluster-scoped files for an object that has no namespace. Searching by content rather
	// than by a table of file names is what lets an object of any kind be found — including
	// an operator's, whose file is named after a CRD this code has never heard of.
	files := make([]string, 0, len(l.Resources))
	for _, file := range l.Resources {
		dir := path.Dir(file)
		clusterScoped := dir == "." || dir == "cluster-scope"
		if (namespace == "" && clusterScoped) || (namespace != "" && dir == namespace) {
			files = append(files, file)
		}
	}
	for _, file := range files {
		for _, raw := range k3dArchiveItems(f, file) {
			var it k3dRawItem
			if json.Unmarshal(raw, &it) != nil {
				continue
			}
			if it.Metadata.Name != name || (it.Kind != "" && strings.ToLower(it.Kind) != want) {
				continue
			}
			out, err := yaml.JSONToYAML(raw)
			if err != nil {
				return "", fmt.Errorf("re-encode %s/%s: %w", kind, name, err)
			}
			return string(out), nil
		}
	}
	return "", fmt.Errorf("%s %q is not in this archive", kind, name)
}

// k3dArchivePodFiles lists what the collector kept for one pod, in the order somebody opens
// them (k3dPodFileLess). On a PXC pod that is its log — one file per container on a current
// collector, one for the whole pod on an older one — then pt-mysql-summary's output, then
// what it pulled off the container's disk: the mysqld error log, the innobackup.*.log backup
// logs, grastate.dat. On a PG pod it is pg_log/, pgbackrest_log/, `pgbackrest info` and
// `patronictl list`. All of it is a great deal more than `kubectl logs` would have given.
func k3dArchivePodFiles(f opFiles, namespace, pod string) []string {
	// Through the layout rather than by prefix: a directory under a namespace is only a
	// pod's if a pod of that name is in the namespace's pods.yaml. `<ns>/secrets/` is a
	// directory too, and its files belong to nobody's Logs pane.
	return k3dReadArchiveLayout(f).PodFiles[namespace+"/"+pod]
}

// k3dArchivePickPodFile chooses which of a pod's kept files to show, and says whether the
// caller's request is what it chose.
//
// The reason it is forgiving is a real dead end: the log pane opens on the pod's WORST
// CONTAINER, because that is the right answer for a live cluster and the pane has no way to
// know it is not looking at one. A capture has no containers — it has files — so the pane's
// first question is "give me `pxc`" about an archive that holds `logs.txt`, `summary.txt`
// and `var/lib/mysql/mysqld-error.log`. Answering that with a 404 loses the one thing the
// pane needed: the list of what IS there. So a request that matches nothing is answered with
// the pod's first file and a note saying so, and the file list rides along either way.
//
// Between exact and nothing there are two matches worth making, both of them the same
// question asked in a different dialect:
//
//	pxc          → pxc.log       a container name, and this collector kept one log per container
//	mysqld-error.log → var/lib/mysql/mysqld-error.log   a bare name for a file kept under a path
func k3dArchivePickPodFile(available []string, want string) (string, bool) {
	if len(available) == 0 {
		return "", false
	}
	if want == "" {
		return available[0], true
	}
	for _, f := range available {
		if f == want {
			return f, true
		}
	}
	for _, f := range available {
		if f == want+".log" {
			return f, true
		}
	}
	for _, f := range available {
		if strings.HasSuffix(f, "/"+want) {
			return f, true
		}
	}
	return available[0], false
}

// k3dShortWant keeps a query parameter quotable in a note: trimmed by runes, so a long or
// multi-byte name comes back as text rather than as half a character.
func k3dShortWant(s string) string {
	r := []rune(s)
	if len(r) > 60 {
		return string(append(r[:59], '…'))
	}
	return string(r)
}

// k3dArchiveTree is every file in the archive with its size — the answer to "is there
// anything in this capture the board is not showing me", which should always be "no".
type k3dArchiveEntry struct {
	Path  string `json:"path"`
	Size  int    `json:"size"`
	Kind  string `json:"kind"`  // resource | events | podfile | cert | log | other
	Owner string `json:"owner"` // the pod or namespace it belongs to, when it belongs to one
}

func k3dArchiveTree(f opFiles) []k3dArchiveEntry {
	l := k3dReadArchiveLayout(f)
	events := map[string]bool{}
	for _, e := range l.Events {
		events[e] = true
	}
	pods := map[string]bool{}
	for ns, names := range l.Pods {
		for _, n := range names {
			pods[ns+"/"+n] = true
		}
	}
	out := make([]k3dArchiveEntry, 0, len(f))
	for name, data := range f {
		e := k3dArchiveEntry{Path: name, Size: len(data), Kind: "other"}
		switch {
		case events[name]:
			e.Kind = "events"
		case strings.HasSuffix(name, ".yaml"):
			e.Kind = "resource"
		}
		if owner := k3dArchivePodOf(name, pods); owner != "" {
			e.Kind, e.Owner = "podfile", owner
		} else if dir := path.Dir(name); dir != "." && !strings.Contains(dir, "/") {
			e.Owner = dir
			// A namespace-level file that is not YAML is a certificate: the collector runs
			// `openssl x509 -noout -text` over every TLS secret it finds and writes the
			// result under the secret's own name (its secrets.go).
			if e.Kind == "other" {
				e.Kind = "cert"
			}
		} else if e.Kind == "other" && strings.HasSuffix(name, ".txt") {
			e.Kind = "log"
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// k3dArchiveAnyFile reads one file by its path in the archive. The lookup is a map of cleaned
// paths, so a path that tries to leave the archive simply is not a key — but it is refused
// before the lookup all the same.
func k3dArchiveAnyFile(f opFiles, name string) (string, bool) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "/")
	if name == "" || strings.Contains(name, "..") {
		return "", false
	}
	data, ok := f[path.Clean(name)]
	if !ok {
		return "", false
	}
	return string(data), true
}

// k3dArchiveFile reads one of them. The path is rebuilt from checked parts rather than taken
// from the caller: a file name of "../../etc/passwd" addresses nothing in the map (the keys
// are cleaned archive paths), but it should not reach the lookup at all.
func k3dArchiveFile(f opFiles, namespace, pod, file string) (string, bool) {
	if strings.Contains(file, "..") {
		return "", false
	}
	name := path.Clean(namespace + "/" + pod + "/" + file)
	data, ok := f[name]
	if !ok {
		return "", false
	}
	return string(data), true
}

// ---------------------------------------------------------------- uploaded archives

// k3dStateUpload is an archive somebody dropped on the page, held for as long as they are
// likely to be reading it.
//
// In memory, not on disk, and deliberately: a kept capture belongs to a stack, which is what
// decides who may read it (k8sdiag.go's ownsDumpStack). An uploaded archive belongs to
// nobody — it may be a customer's cluster that this installation has never seen — so there is
// no stack to hang permission from, and writing it to the dumps directory would make it look
// like a capture of something here. Holding it against the uploader's own user id for an hour
// is the honest lifetime: long enough to click through the board, short enough that a support
// engineer's archive is not still on the server tomorrow.
type k3dStateUpload struct {
	Owner int64
	Name  string
	Data  []byte
	At    time.Time
}

const (
	k3dStateUploadTTL     = time.Hour
	k3dStateUploadPerUser = 3
	k3dStateUploadMax     = 512 << 20 // one upload, uncompressed guard is opArchiveLimit
)

// k3dStateUploads: token → *k3dStateUpload.
var k3dStateUploads sync.Map

// k3dStateUploadKeep stores an archive and returns its token, evicting what has expired and
// whatever the uploader has too many of. A page that is left open past the hour asks for a
// log and is told the archive is gone; that is a better failure than an installation that
// slowly fills its disk with other people's clusters.
func k3dStateUploadKeep(owner int64, name string, data []byte) string {
	now := time.Now()
	var mine []struct {
		tok string
		at  time.Time
	}
	k3dStateUploads.Range(func(k, v any) bool {
		u := v.(*k3dStateUpload)
		if now.Sub(u.At) > k3dStateUploadTTL {
			k3dStateUploads.Delete(k)
			return true
		}
		if u.Owner == owner {
			mine = append(mine, struct {
				tok string
				at  time.Time
			}{k.(string), u.At})
		}
		return true
	})
	sort.Slice(mine, func(i, j int) bool { return mine[i].at.Before(mine[j].at) })
	for len(mine) >= k3dStateUploadPerUser {
		k3dStateUploads.Delete(mine[0].tok)
		mine = mine[1:]
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		// Without a token nothing can address the archive, so the honest answer is "not
		// held" rather than a guessable id.
		return ""
	}
	tok := hex.EncodeToString(raw)
	k3dStateUploads.Store(tok, &k3dStateUpload{Owner: owner, Name: name, Data: data, At: now})
	return tok
}

// k3dStateUploadGet returns an archive if the caller is the one who uploaded it and it has
// not expired.
func k3dStateUploadGet(owner int64, tok string) ([]byte, string, bool) {
	v, ok := k3dStateUploads.Load(tok)
	if !ok {
		return nil, "", false
	}
	u := v.(*k3dStateUpload)
	if u.Owner != owner || time.Since(u.At) > k3dStateUploadTTL {
		return nil, "", false
	}
	return u.Data, u.Name, true
}

// ---------------------------------------------------------------- handlers

// k3dStateArchiveOf resolves the archive a request is about: `?dump=<id>` for a capture kept
// by Diagnostics, `?upload=<token>` for one held from an upload. Writes the error itself.
func (a *App) k3dStateArchiveOf(w http.ResponseWriter, r *http.Request) (opFiles, string, bool) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return nil, "", false
	}
	var data []byte
	label := ""
	switch {
	case strings.TrimSpace(r.URL.Query().Get("upload")) != "":
		var got bool
		data, label, got = k3dStateUploadGet(u.ID, strings.TrimSpace(r.URL.Query().Get("upload")))
		if !got {
			writeErr(w, http.StatusGone, "that uploaded archive is no longer held — upload it again")
			return nil, "", false
		}
	default:
		d, raw, got := a.loadOwnedDumpID(w, r, strings.TrimSpace(r.URL.Query().Get("dump")))
		if !got {
			return nil, "", false
		}
		data, label = raw, d.Cluster+" · "+d.CapturedAt
	}
	files, err := readOpArchive(data)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return nil, "", false
	}
	return files, label, true
}

// loadOwnedDumpID is loadOwnedDump (k8sdiag.go) addressed by a query parameter rather than a
// path value, which is what lets one endpoint serve both sources.
func (a *App) loadOwnedDumpID(w http.ResponseWriter, r *http.Request, id string) (k8sDump, []byte, bool) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return k8sDump{}, nil, false
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad capture id")
		return k8sDump{}, nil, false
	}
	d, err := a.store.K8sDump(n)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such capture")
		return k8sDump{}, nil, false
	}
	if !a.ownsDumpStack(u, d) {
		writeErr(w, http.StatusForbidden, "not your capture")
		return k8sDump{}, nil, false
	}
	data, err := os.ReadFile(d.Path)
	if err != nil {
		writeErr(w, http.StatusGone, "the capture file is missing from this installation")
		return k8sDump{}, nil, false
	}
	return d, data, true
}

// k3dStateArchiveSample writes the board's sample for one archive.
func k3dStateArchiveSample(w http.ResponseWriter, f opFiles, label, capturedAt string) {
	objs, nss, kinds, warnings := k3dStatesFromArchive(f)
	writeJSON(w, http.StatusOK, map[string]any{
		"objects": objs, "namespaces": nss, "kinds": kinds,
		"capturedAt": capturedAt, "warnings": warnings,
		"source": "archive", "label": label,
	})
}

func (a *App) handleK3DStatesFromDump(w http.ResponseWriter, r *http.Request) {
	d, data, ok := a.loadOwnedDump(w, r)
	if !ok {
		return
	}
	files, err := readOpArchive(data)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	k3dStateArchiveSample(w, files, d.Cluster, d.CapturedAt)
}

func (a *App) handleK3DStatesUpload(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if err := r.ParseMultipartForm(96 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid upload")
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "no file provided")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, k3dStateUploadMax))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read upload: "+err.Error())
		return
	}
	files, err := readOpArchive(data)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	name := "uploaded archive"
	if hdr != nil && hdr.Filename != "" {
		name = hdr.Filename
	}
	// Held only after it has parsed: an upload that is not a cluster-dump never occupies
	// anything.
	tok := k3dStateUploadKeep(u.ID, name, data)
	objs, nss, kinds, warnings := k3dStatesFromArchive(files)
	writeJSON(w, http.StatusOK, map[string]any{
		"objects": objs, "namespaces": nss, "kinds": kinds,
		"capturedAt": "", "warnings": warnings,
		"source": "archive", "label": name, "upload": tok,
	})
}

// handleK3DStateArchiveManifest is the YAML pane, reading from an archive.
func (a *App) handleK3DStateArchiveManifest(w http.ResponseWriter, r *http.Request) {
	files, _, ok := a.k3dStateArchiveOf(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	kind, ns, name := strings.TrimSpace(q.Get("kind")), strings.TrimSpace(q.Get("namespace")), strings.TrimSpace(q.Get("name"))
	if _, err := k3dManifestArgs(kind, ns, name); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	out, err := k3dArchiveManifest(files, kind, ns, name)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"kind": kind, "namespace": ns, "name": name, "yaml": out})
}

// handleK3DStateArchiveFiles is the capture's own file tree: every file in the archive, what
// it is, and what it belongs to — plus one of them when `path` names it. It exists so that
// the answer to "is there anything in this capture the board is not showing me" is no.
func (a *App) handleK3DStateArchiveFiles(w http.ResponseWriter, r *http.Request) {
	files, label, ok := a.k3dStateArchiveOf(w, r)
	if !ok {
		return
	}
	if want := strings.TrimSpace(r.URL.Query().Get("path")); want != "" {
		text, got := k3dArchiveAnyFile(files, want)
		if !got {
			writeErr(w, http.StatusNotFound, "no such file in this capture")
			return
		}
		truncated := false
		if len(text) > k3dStateLogCap {
			text = text[len(text)-k3dStateLogCap:]
			if i := strings.IndexByte(text, '\n'); i >= 0 {
				text = text[i+1:]
			}
			truncated = true
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": want, "text": text, "truncated": truncated})
		return
	}
	n, first := k3dArchiveCollectorErrors(files)
	writeJSON(w, http.StatusOK, map[string]any{
		"label": label, "files": k3dArchiveTree(files),
		"collectorErrors": n, "collectorFirstError": first,
	})
}

// handleK3DStateArchiveLogs is the log pane, reading from an archive. Its "containers" are
// the files the collector kept for that pod — logs.txt plus whatever it pulled off disk —
// because a capture has one log per pod and not one per container.
func (a *App) handleK3DStateArchiveLogs(w http.ResponseWriter, r *http.Request) {
	files, _, ok := a.k3dStateArchiveOf(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	ns, pod, file := strings.TrimSpace(q.Get("namespace")), strings.TrimSpace(q.Get("name")), strings.TrimSpace(q.Get("file"))
	if !k3dDNSSubdomain.MatchString(ns) || !k3dDNSSubdomain.MatchString(pod) {
		writeErr(w, http.StatusBadRequest, "namespace and name must be Kubernetes names")
		return
	}
	available := k3dArchivePodFiles(files, ns, pod)
	if len(available) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"namespace": ns, "name": pod, "files": []string{}, "text": "",
			"note": "the capture kept no log for this pod",
		})
		return
	}
	want := file
	file, matched := k3dArchivePickPodFile(available, want)
	text, got := k3dArchiveFile(files, ns, pod, file)
	if !got {
		writeErr(w, http.StatusNotFound, "no such file in this capture")
		return
	}
	truncated := false
	if len(text) > k3dStateLogCap {
		text = text[len(text)-k3dStateLogCap:]
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
		truncated = true
	}
	// What the note says depends on which collector took the capture: a current one writes
	// <container>.log per container, an older one a single logs.txt for the pod. Both are in
	// the wild, and telling somebody "one log per pod" about a capture that has one per
	// container would be wrong in the direction that matters.
	note := "from a capture: the files pt-k8s-debug-collector kept for this pod"
	if file == "logs.txt" {
		note = "from a capture: logs.txt is the whole pod's log, not one container's"
	} else if strings.HasSuffix(file, ".log") && !strings.Contains(file, "/") {
		note = "from a capture: this collector kept one log per container"
	}
	// A request for something the capture does not have — a container name from the live
	// board's picker, most often — is answered rather than refused, and says what happened.
	// The list beside it is the point: it is how the pane finds out what the capture DID keep.
	if !matched {
		note = fmt.Sprintf("the capture kept no %q for this pod — showing %s", k3dShortWant(want), file)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"namespace": ns, "name": pod, "file": file, "files": available,
		"text": text, "truncated": truncated, "note": note,
	})
}
