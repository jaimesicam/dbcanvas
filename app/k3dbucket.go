package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// k3dbucket.go — the object store an operator's backups go to, managed from inside the cluster.
//
// A backup object says where it went (`s3://bucket/some-key`) and stops there. What is actually in
// the bucket — the binlogs the PITR collector is uploading, the pgBackRest repository, the
// half-written prefix a failed backup left behind — is the other half of the question, and until
// now the only answer was the SeaweedFS node's own read-only browser, which can only ever see
// DBCanvas's own store.
//
// HOW IT WORKS, AND WHY IT IS A POD.
//
// Every operation here runs `aws` inside a pod on the cluster, not against the store from
// DBCanvas. That is a deliberate trade with three payoffs:
//
//   - It works for any S3 endpoint the operator was pointed at, not just the stack's SeaweedFS
//     node. The credentials are the cluster's own backup Secret; the endpoint is the cluster's own.
//     Nothing here knows what SeaweedFS is.
//   - Every operation is therefore a kubectl command against a manifest, which is the same
//     contract the rest of the Backups tab has: the pod's YAML is archived into
//     <OperatorSrc>/deploy/backup/ and the exact `aws` line is handed back with every answer, so
//     the panel teaches the thing it does rather than replacing it.
//   - It is how this is done for real. Percona's own backup images carry the AWS CLI (the PXC
//     backup image installs awscli v2 next to xbcloud), which is why "spawn an xtrabackup pod and
//     use the CLI" is the standard way to get at a Percona cluster's bucket from a cluster that
//     has no other S3 client on it.
//
// THE TOOLBOX IS LONG-LIVED, ON PURPOSE. A pod per operation would mean an image pull check, a
// schedule and a container start — several seconds — for each `ls`. Instead one pod per cluster
// sits on `sleep infinity` and every operation is a `kubectl exec` into it. It is started on
// demand, it holds no state, and it can be stopped from the panel; a cluster that is never asked
// about its bucket never gets one.
//
// NO SHELL IS INVOLVED AT ANY POINT. The credentials and the endpoint reach the container from
// its own manifest, and the bucket and key go into the argv of `kubectl exec … --`, which is
// handed to exec(2) intact. An object key is arbitrary text — S3 permits spaces, quotes and
// semicolons in one — and here it never has to be quoted because nothing ever parses it.

// k3dBucketPodSuffix names the toolbox pod: <cluster>-dbcanvas-s3. One per database cluster rather
// than one per namespace — two clusters in a namespace may have different credentials.
const k3dBucketPodSuffix = "-dbcanvas-s3"

// k3dBucketImage is the image the toolbox runs when the cluster's own backup image cannot serve.
//
// percona/percona-xtrabackup ships the AWS CLI v2 (see its Dockerfile in percona/percona-docker),
// which is the whole requirement. It is used for the MongoDB and PostgreSQL operators, whose
// backup images carry pbm and pgbackrest respectively and no S3 client — those two therefore cost
// one image pull the first time the toolbox starts.
const k3dBucketImage = "percona/percona-xtrabackup:8.0"

// k3dBucketListLimit is how many objects one listing returns. S3 pages with a continuation token,
// so a pgBackRest repository with tens of thousands of objects is walked a page at a time.
const k3dBucketListLimit = 500

// k3dBucketDownloadMax caps what the panel will stream out of a bucket. A backup artefact is
// routinely gigabytes and goes through the container's stdout, the Docker exec stream and the
// app's memory on the way to the browser; the things worth reading from here — a pgBackRest
// manifest, an xtrabackup_info, a .md5 — are kilobytes. The cap is what keeps a misplaced click on
// a 40 GiB xbstream from taking DBCanvas down with it.
const k3dBucketDownloadMax = 64 << 20 // 64 MiB

// k3dBucketExecTimeout bounds one `aws` call. A listing is milliseconds; a recursive delete over a
// large prefix is not, and neither should run forever.
const k3dBucketExecTimeout = 3 * time.Minute

// k3dBucketPodManifest is the toolbox. It is written out in full rather than created with
// `kubectl run` so that the archived copy is the thing that was applied.
//
// Three details are load-bearing:
//
//   - envFrom the cluster's backup Secret. Both keys in it are already called
//     AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY, which is exactly what the AWS CLI reads from the
//     environment, so no credentials file is written and none is interpolated into a command.
//   - AWS_EC2_METADATA_DISABLED. Without it the CLI spends a minute probing 169.254.169.254 for
//     instance credentials on every call before falling back to the environment.
//   - No resource requests. The same reason cr.yaml's are commented out on these clusters: a k3d
//     budget is small and a pod that cannot be admitted never starts.
func k3dBucketPodManifest(pod, image, endpoint, bucket, region, secret string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  labels:
    app.kubernetes.io/managed-by: dbcanvas
    dbcanvas.io/role: bucket-toolbox
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
  - name: s3
    image: %s
    imagePullPolicy: IfNotPresent
    command: ["sleep", "infinity"]
    env:
    - name: AWS_ENDPOINT_URL
      value: %q
    - name: AWS_DEFAULT_REGION
      value: %q
    - name: AWS_EC2_METADATA_DISABLED
      value: "true"
    - name: AWS_PAGER
      value: ""
    - name: BUCKET
      value: %q
    envFrom:
    - secretRef:
        name: %s
`, pod, image, endpoint, region, bucket, secret)
}

// k3dBucketArgs is one `aws` call: the argv, and the same call written for a human to run.
//
// The values — bucket, key, prefix — sit in the argv as themselves. That is safe here and only
// here, because this argv never reaches a shell: `kubectl exec … -- aws s3 rm <key>` hands the
// words straight to exec(2) in the container, so a key containing `; rm -rf /` is one argument
// called `; rm -rf /`. Nothing quotes it, and nothing needs to. (seaweedfs_browse.go passes its
// values through the environment for the opposite reason: it runs `sh -c`.)
//
// Display is what the panel prints beside the result, so that every operation in this tab can be
// re-run by hand. It is shell syntax and is quoted accordingly — the one place where quoting
// matters is the line a person is going to paste.
type k3dBucketArgs struct {
	Cmd     []string
	Display string
}

// shQuote renders one argv word for the copyable command line. Single quotes with the usual
// escape for a single quote inside them: everything else is literal to a POSIX shell.
func shQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\"'$`\\*?[]{}()!#&;<>|~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shCommand renders a whole argv as a copyable command line.
func shCommand(argv []string) string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = shQuote(a)
	}
	return strings.Join(out, " ")
}

// k3dBucketKeyMax is the longest key this panel will handle. S3's own limit is 1024 bytes.
const k3dBucketKeyMax = 1024

// k3dBucketKeyOK reports whether an object key or prefix is one to act on.
//
// S3 permits almost any byte in a key; this permits printable ASCII and refuses control
// characters, which is the distinction that matters downstream. A key with a newline in it would
// break the one thing here that is line-oriented — counting the lines `aws s3 rm --recursive`
// prints to say how many objects went — and a key with a control character in it is not something
// a backup tool produced.
func k3dBucketKeyOK(p string) bool {
	if len(p) > k3dBucketKeyMax {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// cleanK3DBucketKey sanitizes a key or prefix. Traversal is refused rather than normalised away,
// as in seaweedfs_browse.go: a request carrying ".." is not one to answer quietly.
func cleanK3DBucketKey(p string) (string, error) {
	p = strings.TrimPrefix(strings.TrimSpace(p), "/")
	if p == "" {
		return "", nil
	}
	if !k3dBucketKeyOK(p) {
		return "", fmt.Errorf("that is not an object key this panel will handle")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." {
			return "", fmt.Errorf("invalid key")
		}
	}
	return p, nil
}

// k3dBucketObject is one entry of a listing: an object, or a prefix to descend into.
type k3dBucketObject struct {
	Name     string `json:"name"` // the entry's own name, not the whole key
	Key      string `json:"key"`  // the full key within the bucket
	Size     int64  `json:"size"`
	Modified string `json:"modified"` // RFC3339
	Dir      bool   `json:"dir"`      // a common prefix
}

// s3ListV2 is the part of `aws s3api list-objects-v2 --output json` this file reads.
type s3ListV2 struct {
	Contents []struct {
		Key          string `json:"Key"`
		Size         int64  `json:"Size"`
		LastModified string `json:"LastModified"`
	} `json:"Contents"`
	CommonPrefixes []struct {
		Prefix string `json:"Prefix"`
	} `json:"CommonPrefixes"`
	NextContinuationToken string `json:"NextContinuationToken"`
	IsTruncated           bool   `json:"IsTruncated"`
}

// parseK3DBucketList turns one page of list-objects-v2 into rows, folders first.
//
// `--delimiter /` is what makes a flat keyspace navigable: everything sharing a prefix up to the
// next slash collapses into one CommonPrefix, so a pgBackRest repository reads as directories
// rather than as forty thousand keys. The prefix itself comes back as a zero-length object on some
// implementations (a key ending in "/"), and that row is dropped — it is the folder the caller is
// already looking at.
func parseK3DBucketList(out []byte, prefix string) ([]k3dBucketObject, string, error) {
	var l s3ListV2
	// An empty prefix answers with an empty document rather than `{}` on some CLI versions.
	if len(strings.TrimSpace(string(out))) == 0 {
		return nil, "", nil
	}
	if err := json.Unmarshal(out, &l); err != nil {
		return nil, "", fmt.Errorf("the S3 endpoint did not answer with a listing")
	}
	objs := make([]k3dBucketObject, 0, len(l.Contents)+len(l.CommonPrefixes))
	base := func(key string) string {
		k := strings.TrimSuffix(key, "/")
		if i := strings.LastIndex(k, "/"); i >= 0 {
			return k[i+1:]
		}
		return k
	}
	for _, p := range l.CommonPrefixes {
		objs = append(objs, k3dBucketObject{Name: base(p.Prefix), Key: strings.TrimSuffix(p.Prefix, "/"), Dir: true})
	}
	for _, c := range l.Contents {
		if c.Key == prefix || strings.HasSuffix(c.Key, "/") {
			continue // the folder marker for the prefix being listed
		}
		o := k3dBucketObject{Name: base(c.Key), Key: c.Key, Size: c.Size}
		if t, err := time.Parse(time.RFC3339, c.LastModified); err == nil {
			o.Modified = t.UTC().Format(time.RFC3339)
		} else {
			o.Modified = c.LastModified
		}
		objs = append(objs, o)
	}
	sort.SliceStable(objs, func(i, j int) bool {
		if objs[i].Dir != objs[j].Dir {
			return objs[i].Dir
		}
		return objs[i].Name < objs[j].Name
	})
	if l.IsTruncated {
		return objs, l.NextContinuationToken, nil
	}
	return objs, "", nil
}

// ---------------------------------------------------------------- the toolbox

// k3dBucketPodName is the toolbox pod for a cluster.
func k3dBucketPodName(cluster string) string { return cluster + k3dBucketPodSuffix }

// k3dBucketImageFor picks what the toolbox runs.
//
// For the two MySQL operators the cluster's own backup image is already on every node (it is what
// the backup jobs run) and already carries the AWS CLI, so using it costs no pull and pins the
// toolbox to the same xtrabackup build that wrote the objects it is about to look at. For MongoDB
// and PostgreSQL the backup image is pbm or pgbackrest, neither of which has an S3 client, and the
// standalone xtrabackup image is pulled instead.
func (a *App) k3dBucketImageFor(ctx context.Context, serverID string, cfg k3dConfig) string {
	if cfg.Operator != "pxc" && cfg.Operator != "ps" {
		return k3dBucketImage
	}
	out, err := a.kubectlQuiet(ctx, serverID, "-n", cfg.Namespace, "get", cfg.Operator, cfg.ClusterName,
		"-o", "jsonpath={.spec.backup.image}")
	if img := strings.TrimSpace(out); err == nil && img != "" && !strings.ContainsAny(img, " \t\n\"'") {
		return img
	}
	return k3dBucketImage
}

// k3dBucketPodPhase reports the toolbox's pod phase, or "" when there is no such pod.
func (a *App) k3dBucketPodPhase(ctx context.Context, serverID, ns, pod string) string {
	out, err := a.kubectlQuiet(ctx, serverID, "-n", ns, "get", "pod", pod, "-o", "jsonpath={.status.phase}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// k3dBucketEnsure brings the toolbox up and waits for it to be ready, returning the pod name and
// the manifest that was applied.
//
// A pod stuck in anything but Running is deleted and recreated rather than waited on: the states
// it gets stuck in are ImagePullBackOff (the image name was wrong, and retrying the same one is
// pointless) and Succeeded/Failed (someone killed the sleep), neither of which resolves itself.
func (a *App) k3dBucketEnsure(ctx context.Context, dep Deployment, cfg k3dConfig) (string, string, error) {
	if cfg.BackupSecret == "" || cfg.BackupBucket == "" {
		return "", "", fmt.Errorf("this cluster has no object store: its backups go to %s",
			orDefault(cfg.BackupRepo, "nowhere"))
	}
	pod := k3dBucketPodName(cfg.ClusterName)
	image := a.k3dBucketImageFor(ctx, dep.ContainerID, cfg)
	manifest := k3dBucketPodManifest(pod, image, cfg.BackupEndpoint, cfg.BackupBucket,
		orDefault(cfg.BackupRegion, seaweedRegion), cfg.BackupSecret)

	switch a.k3dBucketPodPhase(ctx, dep.ContainerID, cfg.Namespace, pod) {
	case "Running":
		return pod, manifest, nil
	case "":
		// nothing there — create it
	default:
		_, _ = a.kubectlQuiet(ctx, dep.ContainerID, "-n", cfg.Namespace, "delete", "pod", pod,
			"--ignore-not-found", "--grace-period=1")
	}
	if _, err := a.k3dBackupArchive(ctx, dep.ContainerID, cfg.OperatorSrc, cfg.Namespace,
		"s3-toolbox.yaml", "Pod "+pod+" — the AWS CLI, in the cluster, on "+cfg.BackupBucket,
		manifest); err != nil {
		_ = err // archiving is a courtesy; the pod is the operation
	}
	if err := a.kubectlApply(ctx, dep.ContainerID, cfg.Namespace, []byte(manifest)); err != nil {
		return "", manifest, fmt.Errorf("start the bucket toolbox: %w", err)
	}
	// The wait is generous because the first start on a MongoDB or PostgreSQL cluster pulls the
	// xtrabackup image, and a slow registry is the usual reason to be here.
	if _, err := a.kubectl(ctx, dep.ContainerID, "-n", cfg.Namespace, "wait", "--for=condition=Ready",
		"pod/"+pod, "--timeout=300s"); err != nil {
		reason := a.k3dBucketPodTrouble(ctx, dep.ContainerID, cfg.Namespace, pod)
		return "", manifest, fmt.Errorf("the bucket toolbox did not become ready: %s", reason)
	}
	return pod, manifest, nil
}

// k3dBucketPodTrouble reads why a toolbox pod is not ready, so the panel can say "the image could
// not be pulled" rather than "timed out".
func (a *App) k3dBucketPodTrouble(ctx context.Context, serverID, ns, pod string) string {
	out, err := a.kubectlQuiet(ctx, serverID, "-n", ns, "get", "pod", pod, "-o",
		`jsonpath={.status.containerStatuses[0].state.waiting.reason}: {.status.containerStatuses[0].state.waiting.message}`)
	if s := strings.Trim(strings.TrimSpace(out), ":"); err == nil && s != "" {
		return strings.TrimSpace(s)
	}
	return "it is still not Running — `kubectl -n " + ns + " describe pod " + pod + "` says why"
}

// k3dBucketExec runs one `aws` call in the toolbox and returns its stdout.
//
// There is no shell anywhere in the path: `kubectl exec … --` hands the argv straight to exec(2)
// in the container, and the credentials and endpoint are already in the pod's environment from
// its manifest, so nothing is interpolated into a command line. Without a TTY, kubectl passes the
// container's stdout through byte for byte, which is what makes the same call usable for both a
// JSON listing and a binary download.
func (a *App) k3dBucketExec(ctx context.Context, serverID, ns, pod string, args k3dBucketArgs) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, k3dBucketExecTimeout)
	defer cancel()
	argv := append([]string{"kubectl", "-n", ns, "exec", pod, "--"}, args.Cmd...)
	res, err := a.engCtx(ctx).Exec(ctx, serverID, argv, []string{"KUBECONFIG=" + k3dKubeconfig})
	if err != nil {
		return "", err
	}
	if res.Code != 0 {
		return res.Stdout, fmt.Errorf("%s", strings.TrimSpace(lastLines(res.Stderr+res.Stdout, 400)))
	}
	return res.Stdout, nil
}

// k3dBucketListArgs is `aws s3api list-objects-v2`, one page, folded at the next slash.
//
// s3api rather than `aws s3 ls`: the high-level command prints a fixed-width text table that has
// to be re-parsed and loses the exact size on large objects, while s3api answers JSON with a
// continuation token, which is the same shape the SeaweedFS browser already pages with.
func k3dBucketListArgs(bucket, prefix, token string) k3dBucketArgs {
	cmd := []string{"aws", "s3api", "list-objects-v2",
		"--bucket", bucket, "--delimiter", "/",
		"--max-keys", strconv.Itoa(k3dBucketListLimit), "--output", "json"}
	if prefix != "" {
		cmd = append(cmd, "--prefix", prefix+"/")
	}
	if token != "" {
		cmd = append(cmd, "--starting-token", token)
	}
	return k3dBucketArgs{Cmd: cmd, Display: shCommand(cmd)}
}

// ---------------------------------------------------------------- handlers

// handleK3DBucket lists one prefix of the cluster's backup bucket.
func (a *App) handleK3DBucket(w http.ResponseWriter, r *http.Request) {
	dep, cfg, _, ok := a.k3dBackupCtx(w, r)
	if !ok {
		return
	}
	prefix, err := cleanK3DBucketKey(r.URL.Query().Get("prefix"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	pod, manifest, err := a.k3dBucketEnsure(r.Context(), dep, cfg)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	args := k3dBucketListArgs(cfg.BackupBucket, prefix, strings.TrimSpace(r.URL.Query().Get("after")))
	out, err := a.k3dBucketExec(r.Context(), dep.ContainerID, cfg.Namespace, pod, args)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list s3://"+cfg.BackupBucket+"/"+prefix+": "+err.Error())
		return
	}
	objs, next, perr := parseK3DBucketList([]byte(out), prefix+"/")
	if perr != nil {
		writeErr(w, http.StatusBadGateway, perr.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"bucket": cfg.BackupBucket, "endpoint": cfg.BackupEndpoint, "prefix": prefix,
		"objects": objs, "after": next, "more": next != "",
		"pod": pod, "command": args.Display, "manifest": manifest,
		"kubectl": fmt.Sprintf("kubectl -n %s exec %s -- %s", cfg.Namespace, pod, args.Display),
	})
}

// k3dBucketDeleteRequest removes one object, or everything under a prefix.
type k3dBucketDeleteRequest struct {
	Key string `json:"key"`
	// Recursive deletes a whole prefix. It is a separate flag rather than inferred from a
	// trailing slash: "delete this object" and "delete these forty thousand objects" should not
	// differ by a character.
	Recursive bool `json:"recursive"`
	DryRun    bool `json:"dryRun"`
}

// handleK3DBucketDelete removes objects from the bucket with `aws s3 rm`.
//
// This is the operation the whole pod exists for. The operators will clear a backup's own prefix
// when the delete-backup finalizer is set (k3dbackup.go), but nothing in Kubernetes will remove
// the binlogs a PITR collector left, a partial upload from a backup that failed, or a prefix whose
// backup object was deleted without the finalizer — and those are exactly the states a lab
// produces. `--dryrun` is offered first-class because `rm --recursive` over the wrong prefix is
// the one action in this tab with no counterpart anywhere else.
func (a *App) handleK3DBucketDelete(w http.ResponseWriter, r *http.Request) {
	dep, cfg, _, ok := a.k3dBackupCtx(w, r)
	if !ok {
		return
	}
	var req k3dBucketDeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	key, err := cleanK3DBucketKey(req.Key)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if key == "" {
		writeErr(w, http.StatusBadRequest, "refusing to delete the whole bucket — name an object or a prefix")
		return
	}
	pod, _, err := a.k3dBucketEnsure(r.Context(), dep, cfg)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	uri := "s3://" + cfg.BackupBucket + "/" + key
	cmd := []string{"aws", "s3", "rm", uri}
	if req.Recursive {
		cmd = append(cmd, "--recursive")
	}
	if req.DryRun {
		cmd = append(cmd, "--dryrun")
	}
	display := shCommand(cmd)
	out, err := a.k3dBucketExec(r.Context(), dep.ContainerID, cfg.Namespace, pod,
		k3dBucketArgs{Cmd: cmd, Display: display})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "delete "+uri+": "+err.Error())
		return
	}
	// `aws s3 rm` prints one line per object and nothing at all when it matched none, which is
	// the difference between "deleted" and "there was nothing there" and worth reporting as such.
	n := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	if req.Recursive && !req.DryRun {
		a.k3dBucketPruneDirs(r.Context(), dep.ContainerID, cfg.Namespace, pod, cfg.BackupBucket, key, 0, new(int))
	}
	if !req.DryRun {
		a.replLogln(dep.StackID, dep.NodeID, fmt.Sprintf(
			"bucket: %d object(s) deleted under %s from the panel (%s in %s)", n, uri, pod, cfg.Namespace))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "key": key, "count": n, "dryRun": req.DryRun, "output": lastLines(out, 400),
		"command": display, "pod": pod,
		"kubectl": fmt.Sprintf("kubectl -n %s exec %s -- %s", cfg.Namespace, pod, display),
		"message": k3dBucketDeleteMessage(n, req.DryRun),
	})
}

// k3dBucketPruneDirs removes the empty directory nodes a recursive delete leaves behind.
//
// Emptying a prefix does not remove the prefix. S3 has no directories, so `rm --recursive`
// deletes objects and nothing else — but a filer-backed store keeps a real directory node, and
// SeaweedFS (which is what a DBCanvas stack's bucket is) is exactly that. Without this, deleting
// a backup's prefix leaves its whole folder tree standing and empty in the next listing, and
// there is no object left to delete to get rid of it.
//
// It has to be depth-first: the node for a directory that still contains directories will not
// go, so the leaves are removed before their parent and the prefix itself last. The key carries a
// trailing slash, which is the one thing `aws s3 rm` will not send.
//
// Bounded twice, because this is tidying and must never become the expensive part of a delete:
// eight levels and 500 directories, after which it stops. Failures are ignored throughout — a
// store with no directory nodes answers "no such key" to every one of these, and tidying up must
// not turn a successful delete into an error.
func (a *App) k3dBucketPruneDirs(ctx context.Context, serverID, ns, pod, bucket, prefix string, depth int, budget *int) {
	if prefix == "" || depth > 8 || *budget > 500 {
		return
	}
	*budget++
	out, err := a.k3dBucketExec(ctx, serverID, ns, pod, k3dBucketArgs{
		Cmd: []string{"aws", "s3api", "list-objects-v2", "--bucket", bucket,
			"--delimiter", "/", "--prefix", prefix + "/", "--max-keys", "1000", "--output", "json"},
	})
	if err == nil {
		var l s3ListV2
		if json.Unmarshal([]byte(out), &l) == nil {
			for _, p := range l.CommonPrefixes {
				a.k3dBucketPruneDirs(ctx, serverID, ns, pod, bucket, strings.TrimSuffix(p.Prefix, "/"), depth+1, budget)
			}
		}
	}
	_, _ = a.k3dBucketExec(ctx, serverID, ns, pod, k3dBucketArgs{
		Cmd: []string{"aws", "s3api", "delete-object", "--bucket", bucket, "--key", prefix + "/"},
	})
}

// k3dBucketDeleteMessage says what happened in the one sentence the panel shows.
func k3dBucketDeleteMessage(n int, dry bool) string {
	switch {
	case n == 0:
		return "nothing matched — the bucket is unchanged"
	case dry:
		return fmt.Sprintf("%d object(s) would be deleted — nothing has been", n)
	case n == 1:
		return "1 object deleted"
	default:
		return fmt.Sprintf("%d objects deleted", n)
	}
}

// handleK3DBucketDownload streams one object out of the bucket to the browser.
//
// `aws s3 cp <key> -` writes the object to stdout, kubectl exec (without a TTY) passes those bytes
// through untouched, and the Docker exec stream demultiplexes them back — so the bytes that arrive
// are the bytes in the bucket. The size is checked with `head-object` FIRST: the alternative is
// discovering a 40 GiB object by running out of memory holding it.
func (a *App) handleK3DBucketDownload(w http.ResponseWriter, r *http.Request) {
	dep, cfg, _, ok := a.k3dBackupCtx(w, r)
	if !ok {
		return
	}
	key, err := cleanK3DBucketKey(r.URL.Query().Get("key"))
	if err != nil || key == "" {
		writeErr(w, http.StatusBadRequest, "name the object to download")
		return
	}
	pod, _, err := a.k3dBucketEnsure(r.Context(), dep, cfg)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	head, err := a.k3dBucketExec(r.Context(), dep.ContainerID, cfg.Namespace, pod, k3dBucketArgs{
		Cmd: []string{"aws", "s3api", "head-object", "--bucket", cfg.BackupBucket, "--key", key, "--output", "json"},
	})
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such object: "+key)
		return
	}
	var meta struct {
		ContentLength int64  `json:"ContentLength"`
		ContentType   string `json:"ContentType"`
	}
	json.Unmarshal([]byte(head), &meta)
	if meta.ContentLength > k3dBucketDownloadMax {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"%s is %s — larger than the %s this panel will stream. Copy it inside the cluster instead: "+
				"kubectl -n %s exec %s -- aws s3 cp s3://%s/%s /backup/",
			key, byteSizeLabel(meta.ContentLength), byteSizeLabel(k3dBucketDownloadMax),
			cfg.Namespace, pod, cfg.BackupBucket, key))
		return
	}
	body, err := a.k3dBucketExec(r.Context(), dep.ContainerID, cfg.Namespace, pod, k3dBucketArgs{
		Cmd: []string{"aws", "s3", "cp", "s3://" + cfg.BackupBucket + "/" + key, "-"},
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "download "+key+": "+err.Error())
		return
	}
	name := key
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	w.Header().Set("Content-Type", orDefault(meta.ContentType, "application/octet-stream"))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	// The filename is quoted and percent-encoded: an object key is arbitrary text and a raw one
	// in a header would at best be dropped and at worst split the header.
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename*=UTF-8''%s`, url.PathEscape(name)))
	w.Write([]byte(body))
}

// byteSizeLabel renders a byte count the way the panel does.
func byteSizeLabel(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// handleK3DBucketToolbox reports the toolbox's state, and stops it.
//
// GET answers without starting anything — the panel opens on this, and opening a tab should not
// pull an image. POST with {"action":"stop"} deletes the pod; {"action":"start"} brings it up,
// which is the only way to pay the first pull deliberately rather than inside a listing.
func (a *App) handleK3DBucketToolbox(w http.ResponseWriter, r *http.Request) {
	dep, cfg, _, ok := a.k3dBackupCtx(w, r)
	if !ok {
		return
	}
	pod := k3dBucketPodName(cfg.ClusterName)
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{
			"pod": pod, "phase": a.k3dBucketPodPhase(r.Context(), dep.ContainerID, cfg.Namespace, pod),
			"namespace": cfg.Namespace, "bucket": cfg.BackupBucket, "endpoint": cfg.BackupEndpoint,
			"secret": cfg.BackupSecret,
			"image":  a.k3dBucketImageFor(r.Context(), dep.ContainerID, cfg),
		})
		return
	}
	var req struct {
		Action string `json:"action"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Action == "stop" {
		if _, err := a.kubectlQuiet(r.Context(), dep.ContainerID, "-n", cfg.Namespace, "delete", "pod", pod,
			"--ignore-not-found", "--grace-period=1"); err != nil {
			writeErr(w, http.StatusBadGateway, "stop "+pod+": "+lastLines(err.Error(), 300))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "pod": pod, "phase": "",
			"steps":   []string{fmt.Sprintf("kubectl -n %s delete pod %s", cfg.Namespace, pod)},
			"message": "the toolbox is gone — the bucket and its contents are untouched",
		})
		return
	}
	pod, manifest, err := a.k3dBucketEnsure(r.Context(), dep, cfg)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "pod": pod, "phase": "Running", "manifest": manifest,
		"steps": []string{
			fmt.Sprintf("kubectl apply -n %s -f %s/%s/s3-toolbox.yaml", cfg.Namespace, cfg.OperatorSrc, k3dBackupDir),
			fmt.Sprintf("kubectl -n %s exec -it %s -- aws s3 ls s3://%s/", cfg.Namespace, pod, cfg.BackupBucket),
		},
		"message": "the toolbox is running — every bucket operation is a kubectl exec into it",
	})
}
