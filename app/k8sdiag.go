package main

// k8sdiag.go — the K3D node's Diagnostics capture: pt-k8s-debug-collector run
// against the cluster, kept on disk, and readable in Operator Summary.
//
// The collector does not run on the k3s node. It reaches the cluster over a
// kubeconfig so it does not have to, rancher/k3s is a minimal busybox image with
// no package manager, and the collector's most useful output is the per-pod
// database summary it makes by port-forwarding into a database pod and running
// pt-mysql-summary / pt-mongodb-summary / pg_gather — which need real clients. So
// a capture creates one throwaway container from dbcanvas-k8scollector on the
// stack network, writes the cluster's kubeconfig into it, runs the collector,
// reads the archive back out and removes the container. Same shape as
// gdbcore.go's mount probe.
//
// Two things about the tool that its documentation does not say, both found by
// running it rather than reading about it:
//
//   - It shells out to `kubectl`. Without one on PATH it fails at the first step
//     with `get namespaces: error: exec: "kubectl": executable file not found`.
//     The image carries one; see images/k8scollector.Dockerfile.
//   - percona-toolkit 3.6.0's flags are narrower than docs.percona.com describes,
//     and are single-dash Go flags: -resource, -namespace, -cluster, -kubeconfig,
//     -forwardport, -version. There is no -skip-pod-summary in this build.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	k8sCollectDir     = "/capture"
	k8sDumpPath       = k8sCollectDir + "/cluster-dump.tar.gz"
	k8sKubeconfigPath = k8sCollectDir + "/kubeconfig.yaml"
	k8sCollectKind    = "k8scollect"
	// A capture of a large cluster port-forwards into every database pod in turn,
	// and on an arm64 host the amd64-only image runs emulated. Generous on purpose.
	k8sCollectTimeout = 20 * time.Minute
)

// k8sCollectorImage is the image a capture runs in. Built by
// `make k8scollector-image`; amd64-only because Percona's apt repo has no arm64
// percona-toolkit (see the Dockerfile).
func k8sCollectorImage() string { return "dbcanvas-k8scollector:debian-12-amd64" }

// k8sCollectResource maps a frame's operator to the collector's -resource value.
// The two community PostgreSQL operators are not Percona's, so the collector has
// no resource mode for them: "none" still dumps every core resource, every pod
// log and the CRs it can see, which is all this summary reads anyway.
func k8sCollectResource(operator string) string {
	switch operator {
	case "pxc", "ps", "psmdb":
		return operator
	case "pg":
		// NOT "pg". The collector's `pg` mode collects the *v1* CRD group,
		// pg.percona.com, and DBCanvas installs the v2 operator, whose resources
		// live under pgv2.percona.com. Verified against a live 3.0.0 deployment:
		// `-resource pg` produced perconapgclusters.pg.percona.com.yaml holding
		// zero items while the cluster was running perfectly well, so a capture
		// taken that way silently contains no custom resources at all.
		return "pgv2"
	default:
		return "none"
	}
}

// ---------------------------------------------------------------- kept dumps

type k8sDump struct {
	ID         int64  `json:"id"`
	StackID    int64  `json:"stackId"`
	FrameID    string `json:"frameId"`
	Cluster    string `json:"cluster"`
	Operator   string `json:"operator"`
	CapturedAt string `json:"capturedAt"`
	SizeBytes  int64  `json:"sizeBytes"`
	Note       string `json:"note"`
	Path       string `json:"-"`
	StackName  string `json:"stackName,omitempty"`
}

// k8sDumpDir sits beside the SQLite file, like the pt-stalk archives: one volume
// holds all of dbcanvas's state, and a multi-megabyte tarball has no business in
// a database column that every backup and every read would carry.
func k8sDumpDir() string {
	dir := filepath.Dir(envOr("DB_PATH", "dbcanvas.db"))
	if dir == "" || dir == "." {
		dir = "."
	}
	return filepath.Join(dir, "k8sdumps")
}

func (s *Store) InsertK8sDump(d k8sDump) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO k8s_dumps (stack_id, frame_id, cluster, operator, captured_at, size_bytes, note, path)
		 VALUES (?,?,?,?,?,?,?,?)`,
		d.StackID, d.FrameID, d.Cluster, d.Operator, d.CapturedAt, d.SizeBytes, d.Note, d.Path)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) K8sDumps(stackID int64, frameID string) ([]k8sDump, error) {
	rows, err := s.db.Query(
		`SELECT id, stack_id, frame_id, cluster, operator, captured_at, size_bytes, note, path
		   FROM k8s_dumps WHERE stack_id=? AND frame_id=? ORDER BY captured_at DESC, id DESC`,
		stackID, frameID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanK8sDumps(rows)
}

// AllK8sDumps powers the Operator Summary picker: every dump this installation
// holds, newest first, with the stack name so a row is identifiable after the
// stack it came from is gone.
func (s *Store) AllK8sDumps() ([]k8sDump, error) {
	rows, err := s.db.Query(
		`SELECT d.id, d.stack_id, d.frame_id, d.cluster, d.operator, d.captured_at,
		        d.size_bytes, d.note, d.path, COALESCE(st.name,'')
		   FROM k8s_dumps d LEFT JOIN stacks st ON st.id = d.stack_id
		  ORDER BY d.captured_at DESC, d.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []k8sDump
	for rows.Next() {
		var d k8sDump
		if err := rows.Scan(&d.ID, &d.StackID, &d.FrameID, &d.Cluster, &d.Operator,
			&d.CapturedAt, &d.SizeBytes, &d.Note, &d.Path, &d.StackName); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func scanK8sDumps(rows *sql.Rows) ([]k8sDump, error) {
	var out []k8sDump
	for rows.Next() {
		var d k8sDump
		if err := rows.Scan(&d.ID, &d.StackID, &d.FrameID, &d.Cluster, &d.Operator,
			&d.CapturedAt, &d.SizeBytes, &d.Note, &d.Path); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) K8sDump(id int64) (k8sDump, error) {
	var d k8sDump
	err := s.db.QueryRow(
		`SELECT id, stack_id, frame_id, cluster, operator, captured_at, size_bytes, note, path
		   FROM k8s_dumps WHERE id=?`, id).
		Scan(&d.ID, &d.StackID, &d.FrameID, &d.Cluster, &d.Operator,
			&d.CapturedAt, &d.SizeBytes, &d.Note, &d.Path)
	return d, err
}

func (s *Store) DeleteK8sDump(id int64) error {
	_, err := s.db.Exec(`DELETE FROM k8s_dumps WHERE id=?`, id)
	return err
}

// ---------------------------------------------------------------- the capture

// runK8sCollect performs one capture end to end and returns the archive bytes.
// Everything it creates is torn down before it returns, including on failure.
func (a *App) runK8sCollect(ctx context.Context, st Stack, frame designFrame, serverID string) ([]byte, error) {
	cluster := k3dClusterName(st.ID, frame)
	kubeconfig, err := a.k3dFetchKubeconfig(ctx, serverID, cluster)
	if err != nil {
		return nil, fmt.Errorf("fetch the cluster's kubeconfig: %w", err)
	}

	eng := a.engCtx(ctx)
	if ok, _ := eng.ImageExists(ctx, k8sCollectorImage()); !ok {
		return nil, fmt.Errorf("the collector image %s is not built — run `make k8scollector-image`", k8sCollectorImage())
	}
	cid, err := eng.ContainerCreate(ctx, ContainerSpec{
		Name:      fmt.Sprintf("dbcanvas-k8scollect-%d", time.Now().UnixNano()),
		Image:     k8sCollectorImage(),
		Cmd:       []string{"sleep", strconv.Itoa(int(k8sCollectTimeout.Seconds()))},
		Network:   networkName(st.ID),
		NoRestart: true,
		Platform:  platformAMD64,
	})
	if err != nil {
		return nil, fmt.Errorf("create the collector container: %w", err)
	}
	defer eng.ContainerRemove(context.WithoutCancel(ctx), cid)
	if err := eng.ContainerStart(ctx, cid); err != nil {
		return nil, fmt.Errorf("start the collector container: %w", err)
	}

	// The kubeconfig goes in through exec rather than a bind mount: a mount would
	// need a path on the Docker host, which is wrong under the Vagrant engine and
	// wrong again when DBCanvas is itself containerised.
	enc := base64.StdEncoding.EncodeToString([]byte(kubeconfig))
	if res, err := eng.Exec(ctx, cid, []string{"bash", "-c",
		"umask 077 && echo '" + enc + "' | base64 -d > " + k8sKubeconfigPath}, nil); err != nil || res.Code != 0 {
		return nil, fmt.Errorf("write the kubeconfig into the collector: %v %s", err, strings.TrimSpace(res.Stderr))
	}

	res, err := eng.Exec(ctx, cid, []string{"bash", "-c",
		"cd " + k8sCollectDir + " && pt-k8s-debug-collector" +
			" -kubeconfig " + k8sKubeconfigPath +
			" -resource $RESOURCE" +
			" 2>&1 | tail -40; test -s " + k8sDumpPath},
		[]string{"RESOURCE=" + k8sCollectResource(frame.K3DOperator)})
	if err != nil {
		return nil, fmt.Errorf("run the collector: %w", err)
	}
	if res.Code != 0 {
		return nil, fmt.Errorf("the collector produced no archive: %s", lastLines(res.Stdout+res.Stderr, 400))
	}
	data, err := a.readContainerFile(ctx, cid, k8sDumpPath)
	if err != nil {
		return nil, fmt.Errorf("read the archive back: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("the collector's archive was empty")
	}
	return data, nil
}

// keepK8sDump writes a finished capture to disk and indexes it.
func (a *App) keepK8sDump(st Stack, frame designFrame, data []byte) (k8sDump, error) {
	dir := k8sDumpDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return k8sDump{}, err
	}
	now := time.Now().UTC()
	cluster := k3dClusterName(st.ID, frame)
	name := fmt.Sprintf("clusterdump-%s-%s.tar.gz", sanitizeName(cluster), now.Format("20060102-150405"))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return k8sDump{}, err
	}
	d := k8sDump{
		StackID: st.ID, FrameID: frame.ID, Cluster: cluster,
		Operator:   frame.K3DOperator,
		CapturedAt: now.Format(time.RFC3339), SizeBytes: int64(len(data)), Path: path,
	}
	id, err := a.store.InsertK8sDump(d)
	if err != nil {
		os.Remove(path)
		return k8sDump{}, err
	}
	d.ID = id
	return d, nil
}

// ---------------------------------------------------------------- handlers

// k8sCollectKey tracks capture state per frame, not per node: the capture is of
// the whole cluster.
func k8sCollectKey(stackID int64, frameID string) string {
	return captureKey(stackID, frameID, k8sCollectKind)
}

func (a *App) handleK8sCollectStatus(w http.ResponseWriter, r *http.Request) {
	st, frame, _, ok := a.k3dRBACContext(w, r)
	if !ok {
		return
	}
	if v, loaded := a.captures.Load(k8sCollectKey(st.ID, frame.ID)); loaded {
		writeJSON(w, http.StatusOK, v.(*captureState))
		return
	}
	// No in-memory state: a kept dump is what "done" means after a restart.
	dumps, _ := a.store.K8sDumps(st.ID, frame.ID)
	if len(dumps) > 0 {
		writeJSON(w, http.StatusOK, captureState{Status: captureDone, Name: "cluster-dump.tar.gz", Finished: dumps[0].CapturedAt})
		return
	}
	writeJSON(w, http.StatusOK, captureState{Status: captureIdle})
}

func (a *App) handleK8sCollectStart(w http.ResponseWriter, r *http.Request) {
	st, frame, dep, ok := a.k3dRBACContext(w, r)
	if !ok {
		return
	}
	if dep.State != DeployRunning {
		writeErr(w, http.StatusConflict, "the cluster's server node is not running")
		return
	}
	key := k8sCollectKey(st.ID, frame.ID)
	if v, loaded := a.captures.Load(key); loaded && v.(*captureState).Status == captureRunning {
		writeJSON(w, http.StatusAccepted, map[string]any{"status": captureRunning})
		return
	}
	a.captures.Store(key, &captureState{Status: captureRunning, Started: time.Now().UTC().Format(time.RFC3339)})
	eng := a.engCtx(r.Context())
	serverID := dep.ContainerID
	go func() {
		ctx, cancel := context.WithTimeout(withEngine(context.Background(), eng), k8sCollectTimeout)
		defer cancel()
		state := &captureState{Name: "cluster-dump.tar.gz", Finished: time.Now().UTC().Format(time.RFC3339)}
		if v, loaded := a.captures.Load(key); loaded {
			state.Started = v.(*captureState).Started
		}
		data, err := a.runK8sCollect(ctx, st, frame, serverID)
		if err != nil {
			state.Status, state.Message = captureError, lastLines(err.Error(), 400)
			a.captures.Store(key, state)
			return
		}
		if _, err := a.keepK8sDump(st, frame, data); err != nil {
			state.Status, state.Message = captureError, "keep the capture: "+err.Error()
			a.captures.Store(key, state)
			return
		}
		state.Status = captureDone
		state.Message = fmt.Sprintf("cluster-dump.tar.gz ready (%s)", humanBytes(float64(len(data))))
		state.Finished = time.Now().UTC().Format(time.RFC3339)
		a.captures.Store(key, state)
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"status": captureRunning})
}

// handleK8sDumps lists the dumps kept for one cluster.
func (a *App) handleK8sDumps(w http.ResponseWriter, r *http.Request) {
	st, frame, _, ok := a.k3dRBACContext(w, r)
	if !ok {
		return
	}
	dumps, err := a.store.K8sDumps(st.ID, frame.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list captures: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dumps": dumps})
}

// loadOwnedDump resolves a kept dump, checks the caller owns the stack it came
// from, and reads it off disk. Mirrors ptstalkarchive.go's ownedArchive, including
// its one subtlety: a dump whose stack has since been deleted stays readable by an
// admin — the whole point of keeping captures is that they outlive what they came
// from — but is not offered to anyone else.
func (a *App) loadOwnedDump(w http.ResponseWriter, r *http.Request) (k8sDump, []byte, bool) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return k8sDump{}, nil, false
	}
	id, err := strconv.ParseInt(r.PathValue("did"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad capture id")
		return k8sDump{}, nil, false
	}
	d, err := a.store.K8sDump(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such capture")
		return k8sDump{}, nil, false
	}
	if !a.ownsDumpStack(u, d) {
		writeErr(w, http.StatusForbidden, "not your stack")
		return k8sDump{}, nil, false
	}
	data, err := os.ReadFile(d.Path)
	if err != nil {
		writeErr(w, http.StatusNotFound, "the capture's file is missing from storage")
		return k8sDump{}, nil, false
	}
	return d, data, true
}

func (a *App) ownsDumpStack(u User, d k8sDump) bool {
	st, err := a.store.GetStack(d.StackID)
	if err != nil {
		return u.Role == RoleAdmin // the stack is gone; the capture outlived it
	}
	return st.OwnerID == u.ID || u.Role == RoleAdmin
}

func (a *App) handleK8sDumpDownload(w http.ResponseWriter, r *http.Request) {
	d, data, ok := a.loadOwnedDump(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", "cluster-dump-"+sanitizeName(d.Cluster)+".tar.gz"))
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func (a *App) handleK8sDumpDelete(w http.ResponseWriter, r *http.Request) {
	d, _, ok := a.loadOwnedDump(w, r)
	if !ok {
		return
	}
	os.Remove(d.Path)
	if err := a.store.DeleteK8sDump(d.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, "delete: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": d.ID})
}

// ---------------------------------------------------------------- summary API

func (a *App) handleOpSummaryDumps(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	dumps, err := a.store.AllK8sDumps()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list captures: "+err.Error())
		return
	}
	// Only what the caller owns — AllK8sDumps is installation-wide.
	mine := []k8sDump{}
	for _, d := range dumps {
		if a.ownsDumpStack(u, d) {
			mine = append(mine, d)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"dumps": mine})
}

func (a *App) handleOpSummaryFromDump(w http.ResponseWriter, r *http.Request) {
	d, data, ok := a.loadOwnedDump(w, r)
	if !ok {
		return
	}
	u, _ := a.currentUser(r)
	m, err := a.opAnalyse(u, data, d.Cluster+" · "+d.CapturedAt, "node", d.StackID, d.Cluster)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	m.CapturedAt = d.CapturedAt
	writeJSON(w, http.StatusOK, m)
}

func (a *App) handleOpSummaryUpload(w http.ResponseWriter, r *http.Request) {
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
	// Kept clear of opArchiveLimit: a gzipped cluster-dump is roughly a tenth of its
	// uncompressed size, so the uncompressed check stays the one that fires — it is the
	// one that reports the size honestly. LimitReader truncates silently, so a cap set
	// too low here surfaces as a corrupt archive rather than as a size error.
	data, err := io.ReadAll(io.LimitReader(file, 1<<30))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read upload: "+err.Error())
		return
	}
	name := "uploaded archive"
	if hdr != nil && hdr.Filename != "" {
		name = hdr.Filename
	}
	// Registered as a Log Summary bundle like any capture: an archive from a
	// cluster this installation has never seen gets the same timeline.
	m, err := a.opAnalyse(u, data, name, "upload", 0, "")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// handleOpSummaryNode captures and analyses in one request — the "just tell me
// what is wrong" path from a K3D node's Diagnostics tab.
func (a *App) handleOpSummaryNode(w http.ResponseWriter, r *http.Request) {
	st, frame, dep, ok := a.k3dRBACContext(w, r)
	if !ok {
		return
	}
	if dep.State != DeployRunning {
		writeErr(w, http.StatusConflict, "the cluster's server node is not running")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), k8sCollectTimeout)
	defer cancel()
	data, err := a.runK8sCollect(ctx, st, frame, dep.ContainerID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	d, keepErr := a.keepK8sDump(st, frame, data)
	u, _ := a.currentUser(r)
	m, err := a.opAnalyse(u, data, k3dClusterName(st.ID, frame), "node", st.ID, st.Name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if keepErr == nil {
		m.CapturedAt = d.CapturedAt
	}
	writeJSON(w, http.StatusOK, m)
}
