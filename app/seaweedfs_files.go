package main

import (
	"archive/tar"
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// seaweedfs_files.go — writing to a SeaweedFS node's buckets, and reading objects back out.
//
// seaweedfs_browse.go made the buckets legible; this makes them usable: download one object,
// upload files into a folder, and copy objects from one bucket to another (on the same node or
// another SeaweedFS node in the stack). Together with the listing they are the file manager
// behind SeaweedFileManager.jsx.
//
// EVERYTHING GOES THROUGH THE FILER, INSIDE THE CONTAINER. Same as the listing, and for the same
// reasons plus one more:
//
//   - The S3 API (8333) is not published to the host — a SeaweedFS node publishes only its 8080
//     web port (seaweedfs.go) — so DBCanvas cannot reach S3 from outside the stack network at all.
//   - Nothing here would be gained if it could: an S3 request has to be SigV4-signed, and there is
//     no AWS SDK in this module, whereas the filer on :8888 answers GET/POST unsigned.
//   - Objects written through the filer are ordinary S3 objects. Verified against
//     chrislusf/seaweedfs: a file POSTed to /buckets/<b>/<key> comes back in ListObjectsV2 with the
//     right key, size and ETag, and S3 GET returns the bytes. The S3 gateway and the filer are two
//     views of one tree, which is why the listing could read it in the first place.
//
// AN OBJECT IS NOT A CONTAINER PATH, so a download cannot simply be GetArchiveStream: the bytes
// live in volume needles, not in the filesystem. Each transfer therefore stages through a temp
// file inside the container — curl writes it, the engine's archive endpoint streams it out, and it
// is removed afterwards. That costs a copy through container disk and buys true streaming: unlike
// the Kubernetes bucket browser (k3dbucket.go), which buffers an object in DBCanvas's memory and
// refuses anything large, nothing here is bounded by RAM.
//
// NOTHING USER-SUPPLIED IS EVER PARSED BY A SHELL. Bucket, key and temp path reach the scripts
// through the exec environment and are only ever referenced as "$BUCKET"/"$KEY"/"$TMP"; an object
// key is arbitrary text and S3 permits spaces, '#' and ';' in one. The temp path is ours and
// hex-named, which also keeps curl's own -F "@file" parsing (which treats ';' and ',' specially)
// away from anything a user chose.

// seaweedTmpDir stages objects on their way in or out. It is under /data — the store's own
// directory, and so the filesystem with room on it — rather than /tmp.
const seaweedTmpDir = "/data/.dbcanvas-tmp"

// errNoSuchObject is a key the filer does not have (404), as opposed to a filer that is not
// answering: the first is a stale panel, the second is a node that is still starting.
var errNoSuchObject = errors.New("no such object")

// seaweedTempPath is a fresh staging path. Random rather than derived from the key: two people
// downloading the same object must not collide, and the name must never contain anything a user
// chose (see the header).
func seaweedTempPath() string {
	var b [16]byte
	crand.Read(b[:])
	return seaweedTmpDir + "/" + hex.EncodeToString(b[:])
}

// seaweedFilerEnv is the exec environment every filer call shares.
func seaweedFilerEnv(bucket, key string) []string {
	return []string{
		"BUCKET=" + url.PathEscape(bucket),
		"KEY=" + seaweedURLPath(key),
		fmt.Sprintf("PORT=%d", seaweedFilerPort),
	}
}

// The filer calls. Each prints its HTTP status as the last thing on stdout, so "the object is not
// there" can be told from "curl could not reach the filer" — the convention seaweedListScript set.
const (
	seaweedHeadScript  = `curl -sS -I -w '\n%{http_code}' "http://localhost:$PORT/buckets/$BUCKET/$KEY"`
	seaweedFetchScript = `mkdir -p "$TMPDIR" && curl -sS -o "$TMP" -w '%{http_code}' "http://localhost:$PORT/buckets/$BUCKET/$KEY"`
	seaweedPutScript   = `curl -sS -o /dev/null -w '%{http_code}' -F "file=@$TMP" "http://localhost:$PORT/buckets/$BUCKET/$KEY"`
	seaweedDelScript   = `curl -sS -o /dev/null -w '%{http_code}' -X DELETE "http://localhost:$PORT/buckets/$BUCKET/$KEY$RECURSE"`
	seaweedMkTmpScript = `mkdir -p "$TMPDIR"`
	seaweedRmScript    = `rm -f "$TMP"`
)

// seaweedStat is what a HEAD says about a key.
type seaweedStat struct {
	Size int64
	Dir  bool
}

// seaweedStatus splits a script's output into its body and the HTTP status curl appended, then
// maps the status the way the browser needs it. Identical in spirit to listSeaweedObjects'
// switch, and kept in one place because three callers need the same three answers.
func seaweedStatus(res ExecResult, err error) (body, status string, _ error) {
	if err != nil {
		return "", "", err
	}
	body = res.Stdout
	if i := strings.LastIndex(body, "\n"); i >= 0 {
		body, status = body[:i], strings.TrimSpace(body[i+1:])
	} else {
		body, status = "", strings.TrimSpace(body)
	}
	switch {
	case status == "200", status == "201", status == "204":
		return body, status, nil
	case status == "404":
		return "", status, errNoSuchObject
	case res.Code != 0 || status == "000" || status == "":
		return "", status, fmt.Errorf("the filer is not answering — the node may still be starting")
	default:
		return "", status, fmt.Errorf("the filer answered %s", status)
	}
}

// statSeaweedObject is a HEAD on one key: does it exist, how big is it, and is it a folder?
//
// The folder test is the presence of an ETag, not the content type. A directory is rendered as
// HTML by the filer and carries neither ETag nor Content-Length; a file carries both — including
// an uploaded .html, which a content-type test would misread as a folder.
func (a *App) statSeaweedObject(ctx context.Context, containerID, bucket, key string) (seaweedStat, error) {
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"sh", "-c", seaweedHeadScript}, seaweedFilerEnv(bucket, key))
	head, _, err := seaweedStatus(res, err)
	if err != nil {
		return seaweedStat{}, err
	}
	return parseSeaweedHead(head), nil
}

// parseSeaweedHead reads the response headers of a HEAD. Split out of statSeaweedObject so the
// folder rule can be tested against real filer output rather than only in a live stack.
func parseSeaweedHead(head string) seaweedStat {
	st := seaweedStat{Dir: true}
	for _, line := range strings.Split(head, "\n") {
		name, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "etag":
			st.Dir = false
		case "content-length":
			st.Size, _ = strconv.ParseInt(strings.TrimSpace(val), 10, 64)
		}
	}
	return st
}

// fetchSeaweedObject stages one object at tmp inside the container.
func (a *App) fetchSeaweedObject(ctx context.Context, containerID, bucket, key, tmp string) error {
	env := append(seaweedFilerEnv(bucket, key), "TMP="+tmp, "TMPDIR="+seaweedTmpDir)
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"sh", "-c", seaweedFetchScript}, env)
	_, _, err = seaweedStatus(res, err)
	return err
}

// putSeaweedObject uploads the staged file at tmp to bucket/key, creating any missing folders on
// the way (the filer makes parents implicitly, which is why there is no mkdir here).
func (a *App) putSeaweedObject(ctx context.Context, containerID, bucket, key, tmp string) error {
	env := append(seaweedFilerEnv(bucket, key), "TMP="+tmp)
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"sh", "-c", seaweedPutScript}, env)
	_, _, err = seaweedStatus(res, err)
	return err
}

// seaweedRecurseQuery is the filer's "delete what is inside it too" flag. A non-empty folder
// without it is refused (the filer answers 500 with "fail to delete non-empty folder"), which is
// the right default: a prefix here is a whole backup.
func seaweedRecurseQuery(recursive bool) string {
	if recursive {
		return "?recursive=true"
	}
	return ""
}

// deleteSeaweedObject removes one key. The filer answers 204 whether or not the key was there, so
// a caller that wants "no such object" has to stat first — handleSeaweedDelete does, because it
// needs the folder bit anyway.
func (a *App) deleteSeaweedObject(ctx context.Context, containerID, bucket, key string, recursive bool) error {
	env := append(seaweedFilerEnv(bucket, key), "RECURSE="+seaweedRecurseQuery(recursive))
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"sh", "-c", seaweedDelScript}, env)
	_, _, err = seaweedStatus(res, err)
	return err
}

// seaweedMkTemp creates the staging directory. Separate from the upload itself because
// PutArchiveStream extracts into a directory that has to exist already.
func (a *App) seaweedMkTemp(ctx context.Context, containerID string) error {
	_, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"sh", "-c", seaweedMkTmpScript}, []string{"TMPDIR=" + seaweedTmpDir})
	return err
}

// seaweedRmTemp removes staged files. Best-effort, and on a context of its own: it runs after the
// response has been streamed, by which time a client that hung up has already cancelled the
// request's context — and that is exactly when the temp file most needs clearing.
func (a *App) seaweedRmTemp(ctx context.Context, containerID string, tmps ...string) {
	ctx = context.WithoutCancel(ctx)
	for _, tmp := range tmps {
		if tmp == "" {
			continue
		}
		a.engCtx(ctx).Exec(ctx, containerID, []string{"sh", "-c", seaweedRmScript}, []string{"TMP=" + tmp})
	}
}

// seaweedObjectError turns one of the shared filer errors into the status and sentence the panel
// should see. `what` names the object so a failure says which one.
func seaweedObjectError(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, errNoSuchObject):
		writeErr(w, http.StatusNotFound, "no such object: "+what)
	case strings.Contains(err.Error(), "is not running"):
		writeErr(w, http.StatusConflict, "the SeaweedFS node is not running")
	default:
		writeErr(w, http.StatusBadGateway, what+": "+err.Error())
	}
}

// handleSeaweedDownload streams one object out of a bucket.
//
//	GET /api/stacks/{id}/nodes/{nid}/seaweed/download?bucket=&path=
//
// One object, not a selection: a multi-object download would have to be built into an archive,
// and the sizes here (a pgBackRest repository, a PBM dump) make that a different feature with a
// different cost. The listing's folder walk is how you reach the one you want.
func (a *App) handleSeaweedDownload(w http.ResponseWriter, r *http.Request) {
	dep, cfg, ok := a.seaweedNode(w, r)
	if !ok {
		return
	}
	bucket := pickSeaweedBucket(cfg, r.URL.Query().Get("bucket"))
	if bucket == "" {
		writeErr(w, http.StatusNotFound, "this SeaweedFS node has no buckets")
		return
	}
	key, err := cleanSeaweedPath(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if key == "" {
		writeErr(w, http.StatusBadRequest, "name the object to download")
		return
	}
	st, err := a.statSeaweedObject(r.Context(), dep.ContainerID, bucket, key)
	if err != nil {
		seaweedObjectError(w, key, err)
		return
	}
	if st.Dir {
		writeErr(w, http.StatusBadRequest, key+" is a folder — open it and download the objects inside")
		return
	}

	tmp := seaweedTempPath()
	defer a.seaweedRmTemp(r.Context(), dep.ContainerID, tmp)
	if err := a.fetchSeaweedObject(r.Context(), dep.ContainerID, bucket, key, tmp); err != nil {
		seaweedObjectError(w, key, err)
		return
	}
	rc, err := a.engCtx(r.Context()).GetArchiveStream(r.Context(), dep.ContainerID, tmp)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "read "+key+": "+err.Error())
		return
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	hdr, err := tr.Next()
	if err != nil {
		writeErr(w, http.StatusBadGateway, "read "+key+": the staged copy could not be read back")
		return
	}
	// Octet-stream on purpose: this is a download, and the filer's own idea of the type would
	// have the browser try to render a .html or .txt object instead of saving it.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(hdr.Size, 10))
	// A key is arbitrary text; percent-encoded, a name with a quote or a newline in it cannot
	// break the header (the form k3dbucket.go uses for the same reason).
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename*=UTF-8''%s`, url.PathEscape(path.Base(key))))
	io.Copy(w, tr)
}

// seaweedUpload is one file on its way into a bucket: where it is staged in the container, and
// the key it becomes.
type seaweedUpload struct {
	tmp string
	key string
	fh  *multipart.FileHeader
}

// handleSeaweedUpload copies files from the browser into a bucket folder.
//
//	POST /api/stacks/{id}/nodes/{nid}/seaweed/upload
//	multipart/form-data: bucket=<name>, path=<folder>, one file part per file whose field
//	                     name is the path relative to that folder
//
// The two-step (stage into the container, then POST each to the filer) is what makes a large
// upload possible at all: the multipart body streams into the node as a tar, exactly as a node
// file drop does, and the same configured ceiling applies — an instance that allows a 4 GiB drop
// onto a node allows a 4 GiB object.
func (a *App) handleSeaweedUpload(w http.ResponseWriter, r *http.Request) {
	dep, cfg, ok := a.seaweedNode(w, r)
	if !ok {
		return
	}
	if err := r.ParseMultipartForm(nodeUploadMemory); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid upload")
		return
	}
	defer r.MultipartForm.RemoveAll()

	bucket := pickSeaweedBucket(cfg, r.FormValue("bucket"))
	if bucket == "" {
		writeErr(w, http.StatusNotFound, "this SeaweedFS node has no buckets")
		return
	}
	dest, err := cleanSeaweedPath(r.FormValue("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	plan, err := planUpload(r.MultipartForm, a.maxUploadBytes())
	if err != nil {
		writeErr(w, uploadStatus(err), err.Error())
		return
	}
	ups := make([]seaweedUpload, 0, len(plan.entries))
	keys := make([]string, 0, len(plan.entries))
	for _, e := range plan.entries {
		// A dropped folder arrives as "sub/dir/file" parts, and those become the key's own
		// folders — the filer creates them on write, so the bucket ends up with the shape that
		// was dropped.
		key, err := cleanSeaweedPath(path.Join(dest, e.path))
		if err != nil || key == "" {
			writeErr(w, http.StatusBadRequest, "invalid destination for "+e.path)
			return
		}
		ups = append(ups, seaweedUpload{tmp: seaweedTempPath(), key: key, fh: e.fh})
		keys = append(keys, key)
	}

	if err := a.seaweedMkTemp(r.Context(), dep.ContainerID); err != nil {
		writeErr(w, http.StatusBadGateway, "prepare the node: "+err.Error())
		return
	}
	tmps := make([]string, len(ups))
	for i, u := range ups {
		tmps[i] = u.tmp
	}
	defer a.seaweedRmTemp(r.Context(), dep.ContainerID, tmps...)

	// Stage everything first, streaming, then publish. A failure while staging leaves the bucket
	// untouched rather than half-written, which is the ordering an interrupted upload should have.
	pr, pw := io.Pipe()
	buildErr := make(chan error, 1)
	go func() {
		err := writeSeaweedUploadTar(pw, ups)
		buildErr <- err
		pw.CloseWithError(err)
	}()
	putErr := a.engCtx(r.Context()).PutArchiveStream(r.Context(), dep.ContainerID, seaweedTmpDir, pr)
	pr.CloseWithError(putErr)
	if err := <-buildErr; err != nil {
		writeErr(w, uploadStatus(err), "read upload: "+err.Error())
		return
	}
	if putErr != nil {
		writeErr(w, http.StatusInternalServerError, "copy to node: "+putErr.Error())
		return
	}
	for _, u := range ups {
		if err := a.putSeaweedObject(r.Context(), dep.ContainerID, bucket, u.key, u.tmp); err != nil {
			seaweedObjectError(w, u.key, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"bucket": bucket, "path": dest, "objects": keys, "bytes": plan.total})
}

// writeSeaweedUploadTar streams the staged copies into the container as a tar. Flat and
// hex-named: these files exist only until each has been POSTed to the filer, and the name a user
// chose belongs in the object key, never in a path a script will handle.
func writeSeaweedUploadTar(w io.Writer, ups []seaweedUpload) error {
	tw := tar.NewWriter(w)
	for _, u := range ups {
		f, err := u.fh.Open()
		if err != nil {
			return uploadError{http.StatusBadRequest, "cannot read " + u.fh.Filename}
		}
		err = tw.WriteHeader(&tar.Header{
			Name: path.Base(u.tmp), Typeflag: tar.TypeReg, Mode: 0o600, Size: u.fh.Size,
		})
		if err == nil {
			_, err = io.Copy(tw, f)
		}
		f.Close()
		if err != nil {
			return err
		}
	}
	return tw.Close()
}

// handleSeaweedTransfer copies objects from one bucket into another.
//
//	POST /api/stacks/{id}/nodes/{nid}/seaweed/transfer
//	{ bucket: "src", paths: [...], toNodeId: "...", toBucket: "dst", toPath: "folder" }
//
// The destination may be a bucket on this node or on another SeaweedFS node in the stack — which
// is the whole reason the file manager has two panes. Same node and same bucket is refused: that
// is a rename, and this endpoint does not do renames.
//
// Nothing lands on the DBCanvas host either way. Within one container the staged copy is simply
// POSTed back to the filer under its new key; across containers the source's archive stream is
// piped straight into the destination's, the way handleFSTransfer moves paths between nodes.
func (a *App) handleSeaweedTransfer(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	var b struct {
		Bucket   string   `json:"bucket"`
		Paths    []string `json:"paths"`
		ToNodeID string   `json:"toNodeId"`
		ToBucket string   `json:"toBucket"`
		ToPath   string   `json:"toPath"`
	}
	if err := decode(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(b.Paths) == 0 {
		writeErr(w, http.StatusBadRequest, "no objects given")
		return
	}
	srcDep, srcCfg, ok := a.seaweedNode(w, r)
	if !ok {
		return
	}
	dstNodeID := b.ToNodeID
	if dstNodeID == "" {
		dstNodeID = r.PathValue("nid")
	}
	dstDep, err := a.runningNode(st, dstNodeID)
	if err != nil {
		writeErr(w, http.StatusConflict, "destination node: "+err.Error())
		return
	}
	dstCfg, err := seaweedConfigOf(dstDep)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "the destination node is not a SeaweedFS node")
		return
	}
	bucket := pickSeaweedBucket(srcCfg, b.Bucket)
	toBucket := pickSeaweedBucket(dstCfg, b.ToBucket)
	if bucket == "" || toBucket == "" {
		writeErr(w, http.StatusNotFound, "this SeaweedFS node has no buckets")
		return
	}
	toPath, err := cleanSeaweedPath(b.ToPath)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sameNode := srcDep.ContainerID == dstDep.ContainerID
	if sameNode && bucket == toBucket {
		writeErr(w, http.StatusBadRequest, "the source and destination are the same folder")
		return
	}

	srcEng := a.depEngine(st, r.PathValue("nid"))
	dstEng := a.depEngine(st, dstNodeID)
	srcCtx := withEngine(r.Context(), srcEng)
	dstCtx := withEngine(r.Context(), dstEng)
	if err := a.seaweedMkTemp(dstCtx, dstDep.ContainerID); err != nil {
		writeErr(w, http.StatusBadGateway, "prepare the destination: "+err.Error())
		return
	}

	copied := 0
	for _, p := range b.Paths {
		key, err := cleanSeaweedPath(p)
		if err != nil || key == "" {
			writeErr(w, http.StatusBadRequest, "invalid object: "+p)
			return
		}
		stat, err := a.statSeaweedObject(srcCtx, srcDep.ContainerID, bucket, key)
		if err != nil {
			seaweedObjectError(w, key, err)
			return
		}
		// Folders are listings, not objects: copying one means walking it, which is the
		// recursive feature this endpoint deliberately is not.
		if stat.Dir {
			writeErr(w, http.StatusBadRequest, key+" is a folder — copy the objects inside it")
			return
		}
		toKey, err := cleanSeaweedPath(path.Join(toPath, path.Base(key)))
		if err != nil || toKey == "" {
			writeErr(w, http.StatusBadRequest, "invalid destination for "+key)
			return
		}
		if err := a.copySeaweedObject(srcCtx, dstCtx, srcDep, dstDep, bucket, key, toBucket, toKey, sameNode); err != nil {
			seaweedObjectError(w, key, err)
			return
		}
		copied++
	}
	writeJSON(w, http.StatusOK, map[string]any{"copied": copied, "toBucket": toBucket, "toPath": toPath})
}

// copySeaweedObject moves one object from a bucket to a bucket, staging it in the source
// container and — when the destination is a different container — piping that staged copy
// straight across as a tar stream.
func (a *App) copySeaweedObject(srcCtx, dstCtx context.Context, srcDep, dstDep Deployment, bucket, key, toBucket, toKey string, sameNode bool) error {
	tmp := seaweedTempPath()
	defer a.seaweedRmTemp(srcCtx, srcDep.ContainerID, tmp)
	if err := a.fetchSeaweedObject(srcCtx, srcDep.ContainerID, bucket, key, tmp); err != nil {
		return err
	}
	if sameNode {
		return a.putSeaweedObject(dstCtx, dstDep.ContainerID, toBucket, toKey, tmp)
	}
	// The archive stream's single entry is named after the temp file, so the same path exists on
	// the far side once it is extracted — no re-taring, and nothing buffered here.
	defer a.seaweedRmTemp(dstCtx, dstDep.ContainerID, tmp)
	rc, err := a.engCtx(srcCtx).GetArchiveStream(srcCtx, srcDep.ContainerID, tmp)
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := a.engCtx(dstCtx).PutArchiveStream(dstCtx, dstDep.ContainerID, seaweedTmpDir, rc); err != nil {
		return err
	}
	return a.putSeaweedObject(dstCtx, dstDep.ContainerID, toBucket, toKey, tmp)
}

// handleSeaweedDelete removes objects from a bucket.
//
//	POST /api/stacks/{id}/nodes/{nid}/seaweed/delete
//	{ bucket: "backups", paths: [...], recursive: false }
//
// Each key is stat-ed first, for two reasons: the filer answers 204 for a key that was never
// there, so without a stat "deleted 3 objects" could be a lie; and a folder needs the recursive
// flag, which the caller has to have asked for — deleting `pgbackrest/pg1` is deleting the whole
// repository, and that is not something to do because a click landed on a folder row.
func (a *App) handleSeaweedDelete(w http.ResponseWriter, r *http.Request) {
	dep, cfg, ok := a.seaweedNode(w, r)
	if !ok {
		return
	}
	var b struct {
		Bucket    string   `json:"bucket"`
		Paths     []string `json:"paths"`
		Recursive bool     `json:"recursive"`
	}
	if err := decode(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(b.Paths) == 0 {
		writeErr(w, http.StatusBadRequest, "no objects given")
		return
	}
	bucket := pickSeaweedBucket(cfg, b.Bucket)
	if bucket == "" {
		writeErr(w, http.StatusNotFound, "this SeaweedFS node has no buckets")
		return
	}

	// Everything is checked before anything is deleted: a selection that is half-refused should
	// leave the bucket as it was, not half-emptied.
	type victim struct {
		key string
		dir bool
	}
	victims := make([]victim, 0, len(b.Paths))
	for _, p := range b.Paths {
		key, err := cleanSeaweedPath(p)
		if err != nil || key == "" {
			writeErr(w, http.StatusBadRequest, "invalid object: "+p)
			return
		}
		st, err := a.statSeaweedObject(r.Context(), dep.ContainerID, bucket, key)
		if err != nil {
			seaweedObjectError(w, key, err)
			return
		}
		if st.Dir && !b.Recursive {
			writeErr(w, http.StatusBadRequest, key+" is a folder — deleting it removes everything inside, so say so explicitly")
			return
		}
		victims = append(victims, victim{key: key, dir: st.Dir})
	}
	for _, v := range victims {
		if err := a.deleteSeaweedObject(r.Context(), dep.ContainerID, bucket, v.key, v.dir); err != nil {
			seaweedObjectError(w, v.key, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"bucket": bucket, "deleted": len(victims)})
}

// handleSeaweedFSNodes lists the stack's running SeaweedFS nodes and their buckets, for the file
// manager's pane pickers — the same job handleFSNodes does for the node file manager, except that
// a pane here picks a bucket as well as a node, and only this endpoint knows which buckets exist.
//
//	GET /api/stacks/{id}/seaweed/nodes
func (a *App) handleSeaweedFSNodes(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	var doc designDoc
	json.Unmarshal(st.Design, &doc)
	deps, _ := a.store.ListDeployments(st.ID)
	byNode := map[string]Deployment{}
	for _, d := range deps {
		byNode[d.NodeID] = d
	}
	type node struct {
		ID      string   `json:"id"`
		Label   string   `json:"label"`
		Buckets []string `json:"buckets"`
	}
	out := []node{}
	for _, n := range doc.Nodes {
		if n.Type != "seaweedfs" {
			continue
		}
		dep, ok := byNode[n.ID]
		if !ok || dep.State != DeployRunning || dep.ContainerID == "" {
			continue
		}
		cfg, err := seaweedConfigOf(dep)
		if err != nil {
			continue
		}
		buckets := cfg.Buckets
		if len(buckets) == 0 && cfg.Bucket != "" {
			buckets = []string{cfg.Bucket}
		}
		out = append(out, node{ID: n.ID, Label: n.Label, Buckets: buckets})
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": out})
}

// seaweedConfigOf reads a deployment's SeaweedFS profile. A node of another type has no buckets
// in its config, and that is how the transfer endpoint recognises a destination that cannot be
// one — the design says what a node is, but the destination is named by a client.
func seaweedConfigOf(dep Deployment) (seaweedConfig, error) {
	var cfg seaweedConfig
	if err := json.Unmarshal(dep.Config, &cfg); err != nil {
		return cfg, err
	}
	if cfg.Bucket == "" && len(cfg.Buckets) == 0 {
		return cfg, fmt.Errorf("not a SeaweedFS node")
	}
	return cfg, nil
}
