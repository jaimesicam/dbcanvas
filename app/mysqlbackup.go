package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// mysqlbackup.go — Percona XtraBackup backups of a Percona Server standalone node, a Percona
// Server replication cluster or a PXC cluster to a SeaweedFS S3 bucket (xbcloud), and restoring
// them: one backup as it was taken, the latest point, or a point in time.
//
// What lands in the bucket, under xtrabackup/<cluster>/:
//
//	backups/<name>/…                      one xbcloud backup, full or incremental
//	catalog.json                          what the backups are (xbCatalog), written by DBCanvas
//	binlog/<host>/<epoch>/<binlog file>/… one closed binary log, shipped for point-in-time restore
//
// The catalog is DBCanvas's, not xtrabackup's: xbcloud cannot list, and an incremental needs
// the LSN its base ended at. It is read and written through the SeaweedFS filer from inside
// the SeaweedFS container (the same path the Buckets tab lists with, seaweedfs_browse.go).
//
// Incrementals chain on the newest backup, and only on the member that took it: LSNs are a
// property of one InnoDB instance, so an incremental of pxc02 on top of a full of pxc01 is
// garbage. Restore follows the chain back to its full and prepares it in order.
//
// Point-in-time restore replays binary logs on top of a backup. Every member runs a systemd
// timer (dbcanvas-binlog-ship) that uploads closed binary logs to binlog/<host>/<epoch>/ and
// closes the current one every five minutes when it has grown; a restore closes and ships it
// first so "now" is reachable. The epoch is renewed on every restore: a restored server starts
// numbering its binary logs from wherever its backup left off, and they must not collide with
// the files of the history the restore discarded.
//
// A restore downloads and prepares while the servers still run, and only then stops them:
//
//   - standalone: swap the datadir, start, replay.
//   - replication: restore every member from the same backup, re-attach the replicas to the
//     primary (GTID auto-position, or the primary's fresh coordinates), then replay on the
//     primary so the replicas receive the replayed transactions through replication.
//   - PXC: restore the bootstrap member, start it as a new cluster, replay, and let every other
//     member rejoin by SST.
//
// Each member keeps its own identity across a restore — auto.cnf (server_uuid), mysqld-auto.cnf
// (SET PERSIST: read_only, semi-sync) and its TLS material — so a replica restored from the
// primary's backup is still a replica, with its own server_uuid. A full backup is taken after
// every restore: it is the base the next incremental and point-in-time restore need on the new
// history.

// xbPrefixOf is the per-cluster path in the bucket, so several clusters can share one bucket.
func xbPrefixOf(label string) string {
	s := sanitizeName(strings.TrimSpace(label))
	if s == "" {
		s = "mysql"
	}
	return "xtrabackup/" + s
}

// xbBackup is one backup in the catalog.
type xbBackup struct {
	Name       string `json:"name"`
	Type       string `json:"type"`           // full | incremental
	Base       string `json:"base,omitempty"` // the backup an incremental applies on top of
	Start      string `json:"start"`          // RFC 3339
	End        string `json:"end"`            // RFC 3339 — the point the backup restores to
	Host       string `json:"host"`           // the member it was taken on
	NodeID     string `json:"nodeId"`
	Epoch      string `json:"epoch"` // that member's binary-log epoch at the time
	FromLSN    string `json:"fromLsn"`
	ToLSN      string `json:"toLsn"`
	BinlogFile string `json:"binlogFile,omitempty"`
	BinlogPos  string `json:"binlogPos,omitempty"`
	GTID       string `json:"gtid,omitempty"`
	Size       int64  `json:"size"` // bytes streamed
	Server     string `json:"server,omitempty"`
}

type xbCatalog struct {
	Backups []xbBackup `json:"backups"`
}

func (c xbCatalog) find(name string) (xbBackup, bool) {
	for _, b := range c.Backups {
		if b.Name == name {
			return b, true
		}
	}
	return xbBackup{}, false
}

// chain is the backups a restore of name prepares, full first.
func (c xbCatalog) chain(name string) ([]xbBackup, error) {
	var out []xbBackup
	for cur := name; ; {
		b, ok := c.find(cur)
		if !ok {
			if len(out) == 0 {
				return nil, fmt.Errorf("no backup named %s", name)
			}
			return nil, fmt.Errorf("backup %s needs %s, which is no longer in the repository", name, cur)
		}
		out = append([]xbBackup{b}, out...)
		if b.Type == "full" {
			return out, nil
		}
		if b.Base == "" || len(out) > 1000 {
			return nil, fmt.Errorf("incremental backup %s has no base", b.Name)
		}
		cur = b.Base
	}
}

// dependents is name and every incremental that builds on it.
func (c xbCatalog) dependents(name string) map[string]bool {
	out := map[string]bool{name: true}
	for grew := true; grew; {
		grew = false
		for _, b := range c.Backups {
			if !out[b.Name] && b.Base != "" && out[b.Base] {
				out[b.Name], grew = true, true
			}
		}
	}
	return out
}

// newest is the most recent backup (the catalog is kept in time order).
func (c xbCatalog) newest() (xbBackup, bool) {
	if len(c.Backups) == 0 {
		return xbBackup{}, false
	}
	return c.Backups[len(c.Backups)-1], true
}

// ------------------------------------------------------------------ the target

// xbMember is one database member of a target.
type xbMember struct {
	Node      designNode
	Dep       Deployment
	Host      string
	FQDN      string
	Bootstrap bool // PXC: the member that runs mysql@bootstrap
	Sec       pxcSecrets
}

func (m xbMember) running() bool { return m.Dep.State == DeployRunning && m.Dep.ContainerID != "" }

// xbTarget is what a backup or restore acts on: a standalone node or a cluster frame.
type xbTarget struct {
	St        Stack
	Kind      string // ps | mysql | pxc
	ID        string // node id (ps) or frame id
	Label     string
	Frame     designFrame // the frame; synthetic for a standalone node (as provisionPerconaServer builds it)
	SeaweedID string
	Bucket    string
	PITR      bool
	Members   []xbMember
}

func (t xbTarget) prefix() string { return xbPrefixOf(t.Label) }

func (t xbTarget) major() string {
	if t.Kind == "pxc" {
		return frameMajor(t.Frame)
	}
	return psMajorOf(t.Frame.PSMajor)
}

// units are the services a member's mysqld can be running under.
func (t xbTarget) units() string {
	if t.Kind == "pxc" {
		return "mysql mysql@bootstrap"
	}
	return mysqlUnit(t.Frame.OS)
}

func (t xbTarget) member(host string) (xbMember, bool) {
	for _, m := range t.Members {
		if m.Host == host {
			return m, true
		}
	}
	return xbMember{}, false
}

// xbTargetOf resolves a standalone node (scope "node") or a frame (scope "frame") to a target.
func (a *App) xbTargetOf(st Stack, doc designDoc, scope, id string) (xbTarget, error) {
	t := xbTarget{St: st, ID: id}
	var members []designNode
	switch scope {
	case "node":
		var n designNode
		for _, x := range doc.Nodes {
			if x.ID == id && x.Type == "ps" && x.FrameID == "" {
				n = x
			}
		}
		if n.ID == "" {
			return t, errors.New("Percona Server node not found")
		}
		if !n.UseXtraBackup {
			return t, errors.New("XtraBackup backups are not enabled for this node")
		}
		t.Kind, t.Label, t.SeaweedID, t.Bucket, t.PITR = "ps", n.Label, n.SeaweedFSNodeID, n.SeaweedFSBucket, n.XBPITR
		t.Frame = designFrame{Type: "mysql", Label: n.Label, OS: n.OS, PSMajor: n.PSMajor, PSVersion: n.PSVersion, GTID: n.GTID}
		members = []designNode{n}
	default:
		var f designFrame
		for _, x := range doc.Frames {
			if x.ID == id && (x.Type == "mysql" || x.Type == "pxc") {
				f = x
			}
		}
		if f.ID == "" {
			return t, errors.New("MySQL cluster not found")
		}
		if !f.UseXtraBackup {
			return t, errors.New("XtraBackup backups are not enabled for this cluster")
		}
		t.Kind, t.Label, t.Frame, t.SeaweedID, t.Bucket, t.PITR = f.Type, f.Label, f, f.SeaweedFSNodeID, f.SeaweedFSBucket, f.XBPITR
		for _, n := range doc.Nodes {
			if n.FrameID == f.ID && n.Type == f.Type && n.Role != "arbitrator" {
				members = append(members, n)
			}
		}
	}
	for _, n := range members {
		dep, _ := a.store.GetDeployment(st.ID, n.ID)
		var cfg struct {
			Hostname  string `json:"hostname"`
			FQDN      string `json:"fqdn"`
			Bootstrap bool   `json:"bootstrap"`
		}
		json.Unmarshal(dep.Config, &cfg)
		var sec pxcSecrets
		json.Unmarshal(dep.Secrets, &sec)
		host := cfg.Hostname
		if host == "" {
			host = sanitizeName(n.Label)
		}
		t.Members = append(t.Members, xbMember{Node: n, Dep: dep, Host: host, FQDN: cfg.FQDN, Bootstrap: cfg.Bootstrap, Sec: sec})
	}
	if len(t.Members) == 0 {
		return t, errors.New("the cluster has no database members")
	}
	return t, nil
}

// xbLoad resolves the request's node or frame, writing the HTTP error itself.
func (a *App) xbLoad(w http.ResponseWriter, r *http.Request) (xbTarget, bool) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return xbTarget{}, false
	}
	var doc designDoc
	if json.Unmarshal(st.Design, &doc) != nil {
		writeErr(w, http.StatusInternalServerError, "invalid stack design")
		return xbTarget{}, false
	}
	scope, id := "node", r.PathValue("nid")
	if fid := r.PathValue("fid"); fid != "" {
		scope, id = "frame", fid
	}
	t, err := a.xbTargetOf(st, doc, scope, id)
	if err != nil {
		code := http.StatusNotFound
		if strings.Contains(err.Error(), "not enabled") {
			code = http.StatusBadRequest
		}
		writeErr(w, code, err.Error())
		return xbTarget{}, false
	}
	return t, true
}

// xbRun runs a bash script on a member and fails with its output's tail.
func (a *App) xbRun(ctx context.Context, t xbTarget, m xbMember, script string, env []string) (string, error) {
	c := withEngine(ctx, a.depEngine(t.St, m.Node.ID))
	res, err := a.engCtx(c).Exec(c, m.Dep.ContainerID, []string{"bash", "-c", script}, env)
	if err != nil {
		return "", fmt.Errorf("%s: %w", m.Host, err)
	}
	if res.Code != 0 {
		return res.Stdout, fmt.Errorf("%s: %s", m.Host, lastLines(strings.TrimSpace(res.Stdout+"\n"+res.Stderr), 600))
	}
	return res.Stdout, nil
}

// xbPrimary is a replication cluster's current primary: the running member that is not
// read_only (failover may have moved it since deploy), else the first running member.
func (a *App) xbPrimary(ctx context.Context, t xbTarget) (xbMember, bool) {
	var first *xbMember
	for i, m := range t.Members {
		if !m.running() {
			continue
		}
		if first == nil {
			first = &t.Members[i]
		}
		out, err := a.xbRun(ctx, t, m, `mysql --defaults-extra-file=/root/.my.cnf -N -B -e 'SELECT @@global.read_only'`, nil)
		if err == nil && strings.TrimSpace(out) == "0" {
			return m, true
		}
	}
	if first == nil {
		return xbMember{}, false
	}
	return *first, true
}

// xbBackupMember picks where a full backup is taken: the primary of a replication cluster,
// the bootstrap member of PXC (else any running one), the node itself for a standalone.
func (a *App) xbBackupMember(ctx context.Context, t xbTarget) (xbMember, error) {
	if t.Kind == "mysql" {
		if m, ok := a.xbPrimary(ctx, t); ok {
			return m, nil
		}
	} else {
		var first *xbMember
		for i, m := range t.Members {
			if m.running() && (m.Bootstrap || t.Kind == "ps") {
				return m, nil
			}
			if m.running() && first == nil {
				first = &t.Members[i]
			}
		}
		if first != nil {
			return *first, nil
		}
	}
	return xbMember{}, errors.New("no member of " + t.Label + " is running")
}

// ------------------------------------------------------------------ the repository

// xbRepo is the target's bucket, reached through the SeaweedFS container.
type xbRepo struct {
	a      *App
	ctx    context.Context // carries the SeaweedFS node's engine
	cid    string
	bucket string
	prefix string
	cfg    seaweedConfig
	sec    seaweedSecrets
}

func (a *App) xbOpenRepo(ctx context.Context, t xbTarget) (*xbRepo, error) {
	if t.SeaweedID == "" {
		return nil, errors.New("no SeaweedFS node is selected for backups")
	}
	cfg, sec, err := a.waitSeaweedBucket(ctx, t.St.ID, t.SeaweedID, t.Bucket, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("the SeaweedFS node is not ready: %w", err)
	}
	dep, err := a.store.GetDeployment(t.St.ID, t.SeaweedID)
	if err != nil || dep.State != DeployRunning || dep.ContainerID == "" {
		return nil, errors.New("the SeaweedFS node is not running")
	}
	return &xbRepo{a: a, ctx: withEngine(ctx, a.depEngine(t.St, t.SeaweedID)), cid: dep.ContainerID,
		bucket: cfg.Bucket, prefix: t.prefix(), cfg: cfg, sec: sec}, nil
}

const xbFilerGetScript = `curl -sS -w '\n%{http_code}' "http://localhost:$PORT/buckets/$BUCKET/$OBJ"`
const xbFilerPutScript = `printf '%s' "$BODY" | curl -sS -o /dev/null -w '%{http_code}' -F "file=@-;filename=$NAME" "http://localhost:$PORT/buckets/$BUCKET/$OBJ"`
const xbFilerDeleteScript = `curl -sS -o /dev/null -w '%{http_code}' -X DELETE "http://localhost:$PORT/buckets/$BUCKET/$OBJ?recursive=true"`

func (r *xbRepo) filer(script, obj string, extra ...string) (string, string, error) {
	env := append([]string{"PORT=" + fmt.Sprint(seaweedFilerPort), "BUCKET=" + url.PathEscape(r.bucket),
		"OBJ=" + seaweedURLPath(r.prefix+"/"+obj)}, extra...)
	res, err := r.a.engCtx(r.ctx).Exec(r.ctx, r.cid, []string{"sh", "-c", script}, env)
	if err != nil {
		return "", "", err
	}
	body, status := res.Stdout, strings.TrimSpace(res.Stdout)
	if i := strings.LastIndex(body, "\n"); i >= 0 {
		body, status = body[:i], strings.TrimSpace(body[i+1:])
	}
	if res.Code != 0 || status == "000" {
		return "", status, errors.New("the SeaweedFS filer is not answering")
	}
	return body, status, nil
}

func (r *xbRepo) catalog() (xbCatalog, error) {
	var c xbCatalog
	body, status, err := r.filer(xbFilerGetScript, "catalog.json")
	if err != nil {
		return c, err
	}
	if status == "404" {
		return c, nil
	}
	if status != "200" {
		return c, fmt.Errorf("reading the backup catalog: the filer answered %s", status)
	}
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		return c, fmt.Errorf("the backup catalog is unreadable: %w", err)
	}
	sort.SliceStable(c.Backups, func(i, j int) bool { return c.Backups[i].End < c.Backups[j].End })
	return c, nil
}

func (r *xbRepo) saveCatalog(c xbCatalog) error {
	js, _ := json.MarshalIndent(c, "", "  ")
	_, status, err := r.filer(xbFilerPutScript, "catalog.json", "BODY="+string(js), "NAME=catalog.json")
	if err != nil {
		return err
	}
	if status != "200" && status != "201" {
		return fmt.Errorf("writing the backup catalog: the filer answered %s", status)
	}
	return nil
}

func (r *xbRepo) remove(obj string) error {
	_, status, err := r.filer(xbFilerDeleteScript, obj)
	if err != nil {
		return err
	}
	if status != "204" && status != "200" && status != "404" {
		return fmt.Errorf("deleting %s: the filer answered %s", obj, status)
	}
	return nil
}

// binlogs lists the binary logs shipped for one member's epoch, oldest first.
func (r *xbRepo) binlogs(host, epoch string) ([]seaweedObject, error) {
	var out []seaweedObject
	after := ""
	for page := 0; page < 100; page++ {
		objs, more, last, err := r.a.listSeaweedObjects(r.ctx, r.cid, r.bucket, r.prefix+"/binlog/"+host+"/"+epoch, after)
		if errors.Is(err, errNoSuchFolder) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		for _, o := range objs {
			if o.Dir {
				out = append(out, o)
			}
		}
		if !more {
			break
		}
		after = last
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// xbcloudCnf is the [xbcloud] defaults file every member reads: SeaweedFS wants path-style
// addressing, and ignores the region S3 clients insist on.
func xbcloudCnf(sw seaweedConfig, sec seaweedSecrets) (cnf, flags string) {
	endpoint := sw.InternalEndpoint
	if endpoint == "" {
		endpoint = fmt.Sprintf("http://%s:%d", sw.FQDN, seaweedS3Port)
	}
	ak := sw.AccessKey
	if ak == "" {
		ak = sec.AccessKey
	}
	region := sw.Region
	if region == "" {
		region = seaweedRegion
	}
	cnf = fmt.Sprintf("[xbcloud]\nstorage=s3\ns3-endpoint=%s\ns3-access-key=%s\ns3-secret-key=%s\ns3-bucket=%s\ns3-bucket-lookup=path\ns3-api-version=4\ns3-region=%s",
		endpoint, ak, sec.SecretKey, sw.Bucket, region)
	// An HTTPS store may be self-signed; PBM skips verification against it for the same reason.
	if sw.TLS {
		flags = "--insecure"
	}
	return cnf, flags
}

// xbSetup (re)writes the xbcloud config, the binlog shipper and its timer on members, and
// returns each one's binary-log epoch. epoch non-empty starts a new one (after a restore).
func (a *App) xbSetup(ctx context.Context, t xbTarget, repo *xbRepo, members []xbMember, epoch string) (map[string]string, error) {
	cnf, flags := xbcloudCnf(repo.cfg, repo.sec)
	pitr := "0"
	if t.PITR {
		pitr = "1"
	}
	newEpoch, fresh := epoch, "1"
	if newEpoch == "" {
		newEpoch, fresh = xbEpochNow(), "0"
	}
	out := map[string]string{}
	for _, m := range members {
		if !m.running() {
			continue
		}
		res, err := a.xbRun(ctx, t, m, xbSetupScript, []string{
			"CNF=" + cnf, "PREFIX=" + t.prefix(), "HOST=" + m.Host, "XBFLAGS=" + flags, "PITR=" + pitr,
			"EPOCH=" + newEpoch, "NEWEPOCH=" + fresh,
			"SHIPPER=" + xbShipperScript, "SVC=" + xbShipperService, "TIMER=" + xbShipperTimer,
		})
		if err != nil {
			return nil, fmt.Errorf("configure backups: %w", err)
		}
		out[m.Host] = xbOutValue(res, "EPOCH")
	}
	return out, nil
}

func xbEpochNow() string { return time.Now().UTC().Format("20060102T150405Z") }

// xbOutValue reads a KEY=value line from a script's output.
func xbOutValue(out, key string) string {
	for _, l := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(l), key+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ------------------------------------------------------------------ backup

// xbBusy holds the targets with a backup or restore running — one at a time per cluster.
var xbBusy sync.Map

func xbKey(t xbTarget) string { return fmt.Sprintf("%d/%s", t.St.ID, t.ID) }

func xbAcquire(t xbTarget) bool {
	_, loaded := xbBusy.LoadOrStore(xbKey(t), struct{}{})
	return !loaded
}

func xbRelease(t xbTarget) { xbBusy.Delete(xbKey(t)) }

func xbIsBusy(t xbTarget) bool {
	_, ok := xbBusy.Load(xbKey(t))
	return ok
}

var xbBinlogPosRe = regexp.MustCompile(`filename '([^']+)', position '(\d+)'(?:, GTID of the last change '([^']*)')?`)

// xbParseInfo reads the key = value lines of xtrabackup_checkpoints and xtrabackup_info.
func xbParseInfo(out string) map[string]string {
	kv := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(l, " = "); ok {
			kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return kv
}

// xbTakeBackup takes a full or incremental backup and records it in the catalog.
func (a *App) xbTakeBackup(ctx context.Context, t xbTarget, repo *xbRepo, typ string) (xbBackup, error) {
	cat, err := repo.catalog()
	if err != nil {
		return xbBackup{}, err
	}
	var m xbMember
	var base xbBackup
	if typ == "incremental" {
		b, ok := cat.newest()
		if !ok {
			return xbBackup{}, errors.New("there is no backup to build an incremental on — take a full backup first")
		}
		base = b
		if m, ok = t.member(b.Host); !ok || !m.running() {
			return xbBackup{}, fmt.Errorf("the newest backup was taken on %s, which is not running — an incremental continues the member that took its base; take a full backup instead", b.Host)
		}
	} else if m, err = a.xbBackupMember(ctx, t); err != nil {
		return xbBackup{}, err
	}
	epochs, err := a.xbSetup(ctx, t, repo, t.Members, "")
	if err != nil {
		return xbBackup{}, err
	}
	if typ == "incremental" && epochs[m.Host] != base.Epoch {
		return xbBackup{}, fmt.Errorf("%s has been restored since backup %s was taken, so its LSNs no longer continue it — take a full backup first", m.Host, base.Name)
	}

	start := time.Now().UTC()
	suffix := "full"
	if typ == "incremental" {
		suffix = "incr"
	}
	name := start.Format("20060102T150405Z") + "-" + suffix
	env := []string{"NAME=" + name}
	if typ == "incremental" {
		env = append(env, "INCR_LSN="+base.ToLSN)
	}
	if t.Kind == "pxc" {
		env = append(env, "GALERA=1")
	}
	out, err := a.xbRun(ctx, t, m, xbBackupScript, env)
	if err != nil {
		return xbBackup{}, fmt.Errorf("backup failed on %w", err)
	}
	kv := xbParseInfo(out)
	b := xbBackup{
		Name: name, Type: typ, Start: start.Format(time.RFC3339), End: time.Now().UTC().Format(time.RFC3339),
		Host: m.Host, NodeID: m.Node.ID, Epoch: xbOutValue(out, "EPOCH"),
		FromLSN: kv["from_lsn"], ToLSN: kv["to_lsn"], Server: kv["server_version"],
	}
	if typ == "incremental" {
		b.Base = base.Name
	}
	fmt.Sscan(xbOutValue(out, "BYTES"), &b.Size)
	if mm := xbBinlogPosRe.FindStringSubmatch(kv["binlog_pos"]); mm != nil {
		b.BinlogFile, b.BinlogPos, b.GTID = mm[1], mm[2], mm[3]
	}
	if b.ToLSN == "" {
		return xbBackup{}, errors.New("the backup finished but its LSNs could not be read")
	}
	// Re-read: a delete may have happened while the backup ran.
	if cur, err := repo.catalog(); err == nil {
		cat = cur
	}
	cat.Backups = append(cat.Backups, b)
	if err := repo.saveCatalog(cat); err != nil {
		return xbBackup{}, err
	}
	return b, nil
}

// handleXBBackup is POST …/xb/backup {type: full|incremental}.
func (a *App) handleXBBackup(w http.ResponseWriter, r *http.Request) {
	t, ok := a.xbLoad(w, r)
	if !ok {
		return
	}
	var req struct {
		Type string `json:"type"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	switch req.Type {
	case "":
		req.Type = "full"
	case "full", "incremental":
	default:
		writeErr(w, http.StatusBadRequest, "type must be full or incremental")
		return
	}
	if !xbAcquire(t) {
		writeErr(w, http.StatusConflict, "a backup or restore of "+t.Label+" is already running")
		return
	}
	defer xbRelease(t)
	repo, err := a.xbOpenRepo(r.Context(), t)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	b, err := a.xbTakeBackup(r.Context(), t, repo, req.Type)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.notifyStack(t.St.ID, "backup.done", "success", "Backup completed",
		fmt.Sprintf("%s: XtraBackup %s backup %s finished on %s.", t.Label, req.Type, b.Name, b.Host), t.nodeForNotice())
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "backup": b})
}

func (t xbTarget) nodeForNotice() string {
	if t.Kind == "ps" {
		return t.ID
	}
	return ""
}

// ------------------------------------------------------------------ list / delete

// xbWindow is the span a point-in-time restore can reach.
type xbWindow struct {
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// handleXBBackups is GET …/xb/backups: the catalog, and the point-in-time window.
func (a *App) handleXBBackups(w http.ResponseWriter, r *http.Request) {
	t, ok := a.xbLoad(w, r)
	if !ok {
		return
	}
	repo, err := a.xbOpenRepo(r.Context(), t)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	cat, err := repo.catalog()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	out := map[string]any{"backups": cat.Backups, "pitr": t.PITR, "running": xbIsBusy(t),
		"repo": fmt.Sprintf("%s/%s", repo.bucket, repo.prefix)}
	if t.PITR && len(cat.Backups) > 0 {
		win := xbWindow{From: cat.Backups[0].End, To: cat.Backups[len(cat.Backups)-1].End}
		nb := cat.Backups[len(cat.Backups)-1]
		if logs, err := repo.binlogs(nb.Host, nb.Epoch); err == nil {
			for _, l := range logs {
				if l.Name >= nb.BinlogFile && l.Modified > win.To {
					win.To = l.Modified
				}
			}
		}
		out["window"] = win
	}
	writeJSON(w, http.StatusOK, out)
}

// handleXBDelete is POST …/xb/delete {name}: the backup and every incremental built on it.
func (a *App) handleXBDelete(w http.ResponseWriter, r *http.Request) {
	t, ok := a.xbLoad(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	if !xbAcquire(t) {
		writeErr(w, http.StatusConflict, "a backup or restore of "+t.Label+" is running")
		return
	}
	defer xbRelease(t)
	repo, err := a.xbOpenRepo(r.Context(), t)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	cat, err := repo.catalog()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if _, ok := cat.find(req.Name); !ok {
		writeErr(w, http.StatusNotFound, "no backup named "+req.Name)
		return
	}
	gone := cat.dependents(req.Name)
	keep := cat.Backups[:0:0]
	for _, b := range cat.Backups {
		if !gone[b.Name] {
			keep = append(keep, b)
		}
	}
	for n := range gone {
		if err := repo.remove("backups/" + n); err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	cat.Backups = keep
	if err := repo.saveCatalog(cat); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	names := make([]string, 0, len(gone))
	for n := range gone {
		names = append(names, n)
	}
	sort.Strings(names)
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "deleted": names})
}

// ------------------------------------------------------------------ restore

// xbRestoreRequest is POST …/xb/restore.
type xbRestoreRequest struct {
	Mode string `json:"mode"` // backup | latest | time
	Name string `json:"name"` // mode=backup
	Time string `json:"time"` // mode=time, RFC 3339
}

// xbPlan is what a restore does: the chain to prepare, and whether (and to when) to replay.
type xbPlan struct {
	Chain  []xbBackup
	Replay bool
	Stop   time.Time // zero: replay everything shipped
	What   string
}

func (p xbPlan) last() xbBackup { return p.Chain[len(p.Chain)-1] }

// xbPlanRestore turns a request into a plan against the catalog.
func xbPlanRestore(cat xbCatalog, req xbRestoreRequest, pitr bool, now time.Time) (xbPlan, error) {
	var p xbPlan
	var name string
	switch req.Mode {
	case "backup":
		name, p.What = req.Name, "backup "+req.Name
	case "", "latest":
		b, ok := cat.newest()
		if !ok {
			return p, errors.New("there are no backups to restore")
		}
		name, p.Replay = b.Name, pitr
		p.What = "the latest point"
		if !pitr {
			p.What = "the newest backup (" + b.Name + ")"
		}
	case "time":
		if !pitr {
			return p, errors.New("point-in-time restore needs binary-log shipping, which is off for this cluster")
		}
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(req.Time))
		if err != nil {
			return p, errors.New("time must be RFC 3339, e.g. 2026-10-14T09:30:00Z")
		}
		at = at.UTC()
		if at.After(now.Add(time.Minute)) {
			return p, fmt.Errorf("%s is in the future", at.Format(time.RFC3339))
		}
		for _, b := range cat.Backups {
			if e, err := time.Parse(time.RFC3339, b.End); err == nil && !e.After(at) {
				name = b.Name
			}
		}
		if name == "" {
			if len(cat.Backups) == 0 {
				return p, errors.New("there are no backups to restore")
			}
			return p, fmt.Errorf("no backup ends before %s — the earliest point that can be restored is %s", at.Format(time.RFC3339), cat.Backups[0].End)
		}
		p.Replay, p.Stop, p.What = true, at, at.Format(time.RFC3339)
	default:
		return p, errors.New("mode must be backup, latest or time")
	}
	chain, err := cat.chain(name)
	if err != nil {
		return p, err
	}
	p.Chain = chain
	if p.Replay && p.last().BinlogFile == "" {
		return p, fmt.Errorf("backup %s recorded no binary-log position, so nothing can be replayed after it", name)
	}
	return p, nil
}

// xbReplayLogs picks the shipped binary logs a replay reads: the one the backup ended in, and
// every one after it.
func xbReplayLogs(logs []seaweedObject, first string) ([]string, error) {
	var out []string
	for _, l := range logs {
		if l.Name >= first {
			out = append(out, l.Name)
		}
	}
	if len(out) == 0 || out[0] != first {
		return nil, fmt.Errorf("binary log %s, where the backup ends, was never shipped to the bucket", first)
	}
	return out, nil
}

// handleXBRestore is POST …/xb/restore.
func (a *App) handleXBRestore(w http.ResponseWriter, r *http.Request) {
	t, ok := a.xbLoad(w, r)
	if !ok {
		return
	}
	var req xbRestoreRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	for _, m := range t.Members {
		if !m.running() {
			writeErr(w, http.StatusConflict, m.Host+" is not running — a restore needs every member up")
			return
		}
	}
	if !xbAcquire(t) {
		writeErr(w, http.StatusConflict, "a backup or restore of "+t.Label+" is already running")
		return
	}
	defer xbRelease(t)
	ctx := r.Context()
	repo, err := a.xbOpenRepo(ctx, t)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	cat, err := repo.catalog()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	plan, err := xbPlanRestore(cat, req, t.PITR, time.Now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log, err := a.xbRestore(ctx, t, repo, plan)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "restore failed: "+err.Error()+"\n"+lastLines(strings.Join(log, "\n"), 1500))
		return
	}
	// The new base for the next incremental and point-in-time restore, on the new history.
	resp := map[string]any{"status": "ok", "restoredTo": plan.What, "log": strings.Join(log, "\n")}
	if b, err := a.xbTakeBackup(ctx, t, repo, "full"); err != nil {
		resp["backupError"] = err.Error()
	} else {
		resp["backup"] = b.Name
	}
	a.notifyStack(t.St.ID, "restore.done", "success", "Restore completed", t.Label+": restored to "+plan.What+".", t.nodeForNotice())
	writeJSON(w, http.StatusOK, resp)
}

// xbRestore carries out a plan. It returns what it did, line by line.
func (a *App) xbRestore(ctx context.Context, t xbTarget, repo *xbRepo, plan xbPlan) ([]string, error) {
	var log []string
	say := func(f string, v ...any) { log = append(log, fmt.Sprintf(f, v...)) }
	var chain []string
	for _, b := range plan.Chain {
		chain = append(chain, b.Name)
	}
	src := plan.last()

	// Who gets restored data, and who replays the binary logs.
	var apply xbMember
	restoreOn := t.Members
	switch t.Kind {
	case "ps":
		apply = t.Members[0]
	case "mysql":
		p, ok := a.xbPrimary(ctx, t)
		if !ok {
			return log, errors.New("no running member")
		}
		apply = p
	case "pxc":
		apply = t.Members[0]
		for _, m := range t.Members {
			if m.Bootstrap {
				apply = m
			}
		}
		restoreOn = []xbMember{apply}
	}

	// Close and ship the binary log the server is writing, so the moments up to now are there.
	var replay []string
	if plan.Replay {
		if m, ok := t.member(src.Host); ok && m.running() {
			if _, err := a.xbRun(ctx, t, m, `/usr/local/sbin/dbcanvas-binlog-ship --flush`, nil); err != nil {
				say("note: shipping the newest binary logs from %s failed: %v", m.Host, err)
			}
		}
		logs, err := repo.binlogs(src.Host, src.Epoch)
		if err != nil {
			return log, err
		}
		if replay, err = xbReplayLogs(logs, src.BinlogFile); err != nil {
			return log, err
		}
		say("replaying %d binary log(s) of %s from %s:%s", len(replay), src.Host, src.BinlogFile, src.BinlogPos)
	}

	// Download and prepare while the servers still run: that is most of the time a restore takes.
	var wg sync.WaitGroup
	errs := make([]error, len(restoreOn))
	for i, m := range restoreOn {
		env := []string{"CHAIN=" + strings.Join(chain, "\n")}
		if m.Host == apply.Host && len(replay) > 0 {
			env = append(env, "BINLOGS="+strings.Join(replay, "\n"), "BLDIR="+t.prefix()+"/binlog/"+src.Host+"/"+src.Epoch)
		}
		wg.Add(1)
		go func(i int, m xbMember, env []string) {
			defer wg.Done()
			_, errs[i] = a.xbRun(ctx, t, m, xbPrepareScript, env)
		}(i, m, env)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return log, fmt.Errorf("prepare: %w", err)
		}
	}
	say("prepared %s on %d member(s)", strings.Join(chain, " + "), len(restoreOn))

	epoch := xbEpochNow()
	swap := func(m xbMember) error {
		_, err := a.xbRun(ctx, t, m, xbSwapScript, []string{"UNITS=" + t.units(), "EPOCH=" + epoch})
		return err
	}
	start := func(m xbMember, unit string) error {
		_, err := a.xbRun(ctx, t, m, xbStartScript, []string{"UNIT=" + unit, "LOGERR=" + pxcLogError(t.Frame.OS)})
		return err
	}
	old := "0"
	if t.major() == "5.7" {
		old = "1"
	}
	detach := func(m xbMember) error {
		_, err := a.xbRun(ctx, t, m, xbDetachScript, []string{"OLD=" + old})
		return err
	}
	replayOn := func(m xbMember) error {
		if len(replay) == 0 {
			return nil
		}
		stop := ""
		if !plan.Stop.IsZero() {
			stop = plan.Stop.UTC().Format("2006-01-02 15:04:05")
		}
		out, err := a.xbRun(ctx, t, m, xbReplayScript, []string{"FIRST=" + src.BinlogFile, "POS=" + src.BinlogPos, "STOP=" + stop})
		if err == nil {
			say("%s", strings.TrimSpace(out))
		}
		return err
	}

	switch t.Kind {
	case "ps":
		m := apply
		if err := swap(m); err != nil {
			return log, err
		}
		if err := start(m, mysqlUnit(t.Frame.OS)); err != nil {
			return log, err
		}
		if err := detach(m); err != nil {
			return log, err
		}
		if err := replayOn(m); err != nil {
			return log, err
		}
		say("%s restored", m.Host)

	case "mysql":
		unit := mysqlUnit(t.Frame.OS)
		if err := swap(apply); err != nil {
			return log, err
		}
		if err := start(apply, unit); err != nil {
			return log, err
		}
		if err := detach(apply); err != nil {
			return log, err
		}
		say("primary %s restored", apply.Host)
		for _, m := range t.Members {
			if m.Host == apply.Host {
				continue
			}
			if err := swap(m); err != nil {
				return log, err
			}
			if err := start(m, unit); err != nil {
				return log, err
			}
			if err := detach(m); err != nil {
				return log, err
			}
			pr := &pxcProg{a: a, stackID: t.St.ID, nodeID: m.Node.ID, p: &provProgress{}, relay: func(s string) { say("%s: %s", m.Host, s) }}
			c := withEngine(ctx, a.depEngine(t.St, m.Node.ID))
			if err := a.mysqlAttachReplica(c, t.St, t.Frame, m.Node, apply.Node.ID, apply.FQDN, m.Sec, pr); err != nil {
				return log, fmt.Errorf("%s: re-attach to %s: %w", m.Host, apply.Host, err)
			}
			say("replica %s restored and replicating from %s", m.Host, apply.Host)
		}
		// Replay last, on the primary, so the replicas receive it through replication.
		if err := replayOn(apply); err != nil {
			return log, err
		}

	case "pxc":
		// Every other member down first: the restored member starts a new cluster on its own.
		for _, m := range t.Members {
			if m.Host == apply.Host {
				continue
			}
			if _, err := a.xbRun(ctx, t, m, xbPXCLeaveScript, []string{"EPOCH=" + epoch}); err != nil {
				return log, err
			}
		}
		if err := swap(apply); err != nil {
			return log, err
		}
		if err := start(apply, "mysql@bootstrap"); err != nil {
			return log, err
		}
		if err := replayOn(apply); err != nil {
			return log, err
		}
		say("%s restored and bootstrapped a new cluster", apply.Host)
		for _, m := range t.Members {
			if m.Host == apply.Host {
				continue
			}
			if _, err := a.xbRun(ctx, t, m, pxcJoinScript, []string{"LOGERR=" + pxcLogError(t.Frame.OS), "ROOT_PW=" + m.Sec.RootPassword}); err != nil {
				return log, fmt.Errorf("rejoin: %w", err)
			}
			say("%s rejoined by SST", m.Host)
		}
	}

	if _, err := a.xbSetup(ctx, t, repo, t.Members, epoch); err != nil {
		say("note: %v", err)
	}
	return log, nil
}

// ------------------------------------------------------------------ status chip

// xbFrameStatus is a cluster header's backup chip for an XtraBackup cluster (backupstatus.go).
func (a *App) xbFrameStatus(ctx context.Context, st Stack, doc designDoc, f designFrame) backupInfo {
	b := backupInfo{Engine: "xtrabackup"}
	t, err := a.xbTargetOf(st, doc, "frame", f.ID)
	if err != nil {
		b.Err = err.Error()
		return b
	}
	b.Running = xbIsBusy(t)
	repo, err := a.xbOpenRepo(ctx, t)
	if err != nil {
		b.Err = err.Error()
		return b
	}
	cat, err := repo.catalog()
	if err != nil {
		b.Err = err.Error()
		return b
	}
	b.Count = len(cat.Backups)
	if nb, ok := cat.newest(); ok {
		if e, err := time.Parse(time.RFC3339, nb.End); err == nil {
			b.LastAt = e.Unix()
		}
		b.Last, b.Type, b.Size = nb.Name, nb.Type, nb.Size
	}
	return b
}

// ------------------------------------------------------------------ validation

// xbIssues validates a design's XtraBackup option.
func xbIssues(who string, use bool, seaweedID, bucket string, vault bool, doc designDoc) []issue {
	if !use {
		return nil
	}
	if seaweedID == "" {
		return []issue{{Level: "error", Message: who + " has XtraBackup backups enabled but no SeaweedFS node selected"}}
	}
	if vault {
		return []issue{{Level: "error", Message: who + ": XtraBackup backups are not supported together with a keyring (data-at-rest encryption) — the keyring would have to travel with every backup"}}
	}
	return seaweedBucketIssues(who, seaweedID, bucket, doc)
}

// ------------------------------------------------------------------ scripts

// xbSetupScript writes the xbcloud defaults file, the shipper and its timer. Idempotent: run
// before every backup, so a rotated secret or a changed PITR option takes effect.
// Env: CNF PREFIX HOST XBFLAGS PITR EPOCH NEWEPOCH SHIPPER SVC TIMER.
const xbSetupScript = `set -e
D=/etc/dbcanvas-xb
mkdir -p $D /var/lib/dbcanvas-xb
chmod 700 $D
umask 077
printf '%s\n' "$CNF" > $D/xbcloud.cnf
printf 'PREFIX=%q\nHOST=%q\nXBFLAGS=%q\n' "$PREFIX" "$HOST" "$XBFLAGS" > $D/env
if [ "$NEWEPOCH" = 1 ] || [ ! -s $D/epoch ]; then printf '%s\n' "$EPOCH" > $D/epoch; fi
umask 022
printf '%s\n' "$SHIPPER" > /usr/local/sbin/dbcanvas-binlog-ship
chmod 755 /usr/local/sbin/dbcanvas-binlog-ship
printf '%s\n' "$SVC" > /etc/systemd/system/dbcanvas-binlog-ship.service
printf '%s\n' "$TIMER" > /etc/systemd/system/dbcanvas-binlog-ship.timer
systemctl daemon-reload
if [ "$PITR" = 1 ]; then
  systemctl enable --now dbcanvas-binlog-ship.timer >/dev/null 2>&1
else
  systemctl disable --now dbcanvas-binlog-ship.timer >/dev/null 2>&1 || true
fi
echo "EPOCH=$(cat $D/epoch)"`

// xbShipperScript uploads every closed binary log not yet shipped in this epoch. xbcloud only
// accepts a stream that ends in an xtrabackup_tablespaces file (it checks the upload is a whole
// backup), so each log travels with an empty one. --flush closes the current log first.
const xbShipperScript = `#!/bin/bash
# Written by DBCanvas: ships closed binary logs to SeaweedFS for point-in-time restore.
set -o pipefail
D=/etc/dbcanvas-xb
. $D/env || exit 1
EPOCH=$(cat $D/epoch 2>/dev/null)
[ -n "$EPOCH" ] || exit 0
exec 9>$D/ship.lock
flock -w 300 9 || exit 1
M() { mysql --defaults-extra-file=/root/.my.cnf -N -B -e "$1"; }
DATADIR=$(M 'SELECT @@datadir') || exit 1
IDX=$(M 'SELECT @@log_bin_index') || exit 1
[ -n "$IDX" ] && [ "$IDX" != NULL ] && [ -f "$IDX" ] || { echo "binary logging is off"; exit 1; }
path() { local f=${1#./}; case $f in /*) echo "$f" ;; *) echo "$DATADIR/$f" ;; esac; }
cur=$(path "$(tail -1 "$IDX")")
size=$(stat -c %s "$cur" 2>/dev/null || echo 0)
now=$(date +%s)
read -r last lastfile lastsize < $D/flushed 2>/dev/null || { last=0; lastfile=; lastsize=0; }
if [ "$1" = --flush ] || { [ $((now - last)) -ge 300 ] && { [ "$cur" != "$lastfile" ] || [ "$size" -gt "$lastsize" ]; }; }; then
  if M 'FLUSH BINARY LOGS'; then
    cur=$(path "$(tail -1 "$IDX")")
    echo "$now $cur $(stat -c %s "$cur" 2>/dev/null || echo 0)" > $D/flushed
  fi
fi
STATE=$D/shipped.$EPOCH
touch "$STATE"
W=/var/lib/dbcanvas-xb/ship
mkdir -p $W
rc=0
mapfile -t files < "$IDX"
for ((i = 0; i < ${#files[@]} - 1; i++)); do
  f=$(path "${files[$i]}")
  b=$(basename "$f")
  grep -qxF "$b" "$STATE" && continue
  [ -f "$f" ] || continue
  rm -rf "$W" && mkdir -p "$W"
  ln "$f" "$W/$b" 2>/dev/null || cp -p "$f" "$W/$b"
  : > "$W/xtrabackup_tablespaces"
  out=$(cd "$W" && xbstream -c "$b" xtrabackup_tablespaces | xbcloud --defaults-file=$D/xbcloud.cnf put $XBFLAGS "$PREFIX/binlog/$HOST/$EPOCH/$b" 2>&1)
  if [ $? -eq 0 ] || printf '%s' "$out" | grep -q 'already exists'; then
    echo "$b" >> "$STATE"
    echo "shipped $b"
  else
    echo "shipping $b failed: $(printf '%s' "$out" | tail -2)"
    rc=1
  fi
done
rm -rf "$W"
exit $rc`

const xbShipperService = `[Unit]
Description=Ship MySQL binary logs to SeaweedFS (DBCanvas point-in-time restore)
[Service]
Type=oneshot
Environment=HOME=/root
ExecStart=/usr/local/sbin/dbcanvas-binlog-ship`

const xbShipperTimer = `[Unit]
Description=Ship MySQL binary logs to SeaweedFS every minute (DBCanvas)
[Timer]
OnBootSec=60
OnUnitActiveSec=60
AccuracySec=5
[Install]
WantedBy=timers.target`

// xbBackupScript streams a backup into the bucket. Env: NAME, INCR_LSN (incremental), GALERA.
// It prints EPOCH= and BYTES=, then xtrabackup_checkpoints and xtrabackup_info.
const xbBackupScript = `set -o pipefail
D=/etc/dbcanvas-xb
. $D/env
W=/var/lib/dbcanvas-xb/backup
rm -rf $W && mkdir -p $W
args=(--backup --stream=xbstream --extra-lsndir=$W --parallel=4)
[ -n "$INCR_LSN" ] && args+=(--incremental-lsn="$INCR_LSN")
[ "$GALERA" = 1 ] && args+=(--galera-info)
xtrabackup --defaults-extra-file=/root/.my.cnf "${args[@]}" 2>$W/xtrabackup.log | tee >(wc -c > $W/bytes) |
  xbcloud --defaults-file=$D/xbcloud.cnf put $XBFLAGS --parallel=4 "$PREFIX/backups/$NAME" 2>$W/xbcloud.log
if [ $? -ne 0 ]; then
  echo "xtrabackup:"; tail -8 $W/xtrabackup.log
  echo "xbcloud:"; tail -5 $W/xbcloud.log
  xbcloud --defaults-file=$D/xbcloud.cnf delete $XBFLAGS "$PREFIX/backups/$NAME" >/dev/null 2>&1 || true
  exit 1
fi
for i in $(seq 1 50); do [ -s $W/bytes ] && break; sleep 0.1; done
echo "EPOCH=$(cat $D/epoch)"
echo "BYTES=$(cat $W/bytes 2>/dev/null)"
cat $W/xtrabackup_checkpoints $W/xtrabackup_info`

// xbPrepareScript downloads a chain (full first) and prepares it, and the binary logs to replay
// when BINLOGS is set — all while the server keeps running. Env: CHAIN, BINLOGS, BLDIR.
const xbPrepareScript = `set -o pipefail
D=/etc/dbcanvas-xb
. $D/env
R=/var/lib/dbcanvas-xb/restore
rm -rf $R && mkdir -p $R
get() {
  mkdir -p "$2"
  xbcloud --defaults-file=$D/xbcloud.cnf get $XBFLAGS --parallel=4 "$1" 2>$R/xbcloud.log | xbstream -x -C "$2" --parallel=4 ||
    { echo "download of $1 failed:"; tail -5 $R/xbcloud.log; exit 1; }
}
prep() { xtrabackup --prepare "$@" >$R/prepare.log 2>&1 || { echo "prepare failed:"; tail -15 $R/prepare.log; exit 1; }; }
mapfile -t chain < <(printf '%s\n' "$CHAIN" | sed '/^$/d')
n=${#chain[@]}
get "$PREFIX/backups/${chain[0]}" $R/full
if [ $n -eq 1 ]; then
  prep --target-dir=$R/full
else
  prep --apply-log-only --target-dir=$R/full
  for ((i = 1; i < n; i++)); do
    get "$PREFIX/backups/${chain[$i]}" $R/inc$i
    if [ $i -lt $((n - 1)) ]; then
      prep --apply-log-only --target-dir=$R/full --incremental-dir=$R/inc$i
    else
      prep --target-dir=$R/full --incremental-dir=$R/inc$i
    fi
    rm -rf $R/inc$i
  done
fi
if [ -n "$BINLOGS" ]; then
  while read -r b; do
    [ -n "$b" ] && get "$BLDIR/$b" $R/binlog
  done <<< "$BINLOGS"
  rm -f $R/binlog/xtrabackup_tablespaces
fi
echo "prepared ${chain[*]}"`

// xbSwapScript stops the server and replaces its datadir with the prepared backup, keeping what
// is this member's own: server_uuid (auto.cnf), SET PERSIST (mysqld-auto.cnf) and TLS files.
// Galera's state is dropped so a PXC member starts a new cluster (or asks for SST). The shipper
// stops, and the new epoch is in place before the server starts writing binary logs again.
// Env: UNITS, EPOCH.
const xbSwapScript = `set -e
R=/var/lib/dbcanvas-xb/restore
DATA=/var/lib/mysql
[ -d $R/full ] || { echo "no prepared backup"; exit 1; }
systemctl stop dbcanvas-binlog-ship.timer 2>/dev/null || true
for u in $UNITS; do systemctl stop "$u" 2>/dev/null || true; done
K=$R/keep
rm -rf $K && mkdir -p $K
for f in auto.cnf mysqld-auto.cnf; do [ -f $DATA/$f ] && cp -p $DATA/$f $K/; done
cp -p $DATA/*.pem $K/ 2>/dev/null || true
find $DATA -mindepth 1 -delete
xtrabackup --move-back --target-dir=$R/full --datadir=$DATA >$R/moveback.log 2>&1 || { echo "move-back failed:"; tail -10 $R/moveback.log; exit 1; }
cp -p $K/* $DATA/ 2>/dev/null || true
rm -f $DATA/grastate.dat $DATA/galera.cache
chown -R mysql:mysql $DATA
rm -rf $R/full $K
printf '%s\n' "$EPOCH" > /etc/dbcanvas-xb/epoch
echo "datadir replaced"`

// xbStartScript starts a unit and waits until the server answers. Env: UNIT, LOGERR.
const xbStartScript = `systemctl reset-failed "$UNIT" 2>/dev/null || true
systemctl start "$UNIT" || true
for i in $(seq 1 180); do
  mysqladmin --defaults-extra-file=/root/.my.cnf ping >/dev/null 2>&1 && { echo "$UNIT up"; exit 0; }
  sleep 1
done
echo "$UNIT did not come up after the restore:"
tail -20 "$LOGERR" 2>/dev/null
journalctl -u "$UNIT" --no-pager -n 10 2>/dev/null
exit 1`

// xbDetachScript drops the replication configuration the backup carried — a backup of a replica
// restored on the primary would otherwise replicate from itself. OLD=1 is 5.7's syntax.
const xbDetachScript = `M="mysql --defaults-extra-file=/root/.my.cnf"
if [ "$OLD" = 1 ]; then
  $M -e "STOP SLAVE; RESET SLAVE ALL" 2>/dev/null || true
else
  $M -e "STOP REPLICA; RESET REPLICA ALL" 2>/dev/null || true
fi`

// xbReplayScript replays the downloaded binary logs from the backup's position, up to STOP
// (UTC "YYYY-MM-DD HH:MM:SS") when it is set. Env: FIRST, POS, STOP.
const xbReplayScript = `set -o pipefail
B=/var/lib/dbcanvas-xb/restore/binlog
cd $B 2>/dev/null || { echo "no binary logs to replay"; exit 0; }
mapfile -t files < <(ls | sort)
[ ${#files[@]} -gt 0 ] || { echo "no binary logs to replay"; exit 0; }
[ "${files[0]}" = "$FIRST" ] || { echo "binary log $FIRST was not downloaded"; exit 1; }
args=(--start-position="$POS")
[ -n "$STOP" ] && args+=(--stop-datetime="$STOP")
TZ=UTC mysqlbinlog "${args[@]}" "${files[@]}" | mysql --defaults-extra-file=/root/.my.cnf || { echo "binary-log replay failed"; exit 1; }
cd / && rm -rf /var/lib/dbcanvas-xb/restore
if [ -n "$STOP" ]; then echo "replayed ${#files[@]} binary log(s) up to $STOP UTC"; else echo "replayed ${#files[@]} binary log(s) to the end"; fi`

// xbPXCLeaveScript takes a PXC member down for the restore and drops its Galera state, so it
// rejoins the restored cluster by SST. Env: EPOCH.
const xbPXCLeaveScript = `systemctl stop dbcanvas-binlog-ship.timer 2>/dev/null || true
systemctl stop mysql 2>/dev/null || true
systemctl stop mysql@bootstrap 2>/dev/null || true
rm -f /var/lib/mysql/grastate.dat /var/lib/mysql/galera.cache
mkdir -p /etc/dbcanvas-xb && printf '%s\n' "$EPOCH" > /etc/dbcanvas-xb/epoch
echo "left the cluster"`
