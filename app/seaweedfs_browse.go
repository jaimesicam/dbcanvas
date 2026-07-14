package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// seaweedfs_browse.go — reading a deployed SeaweedFS node's buckets from its properties panel.
//
// A SeaweedFS node is the backup target for most of a stack (pgBackRest, Barman, PBM, xbcloud, and
// the four Percona operators), and it can hold up to ten buckets — but nothing showed what actually
// landed in them. This is the read-only browser behind the node panel's Buckets tab: pick a bucket,
// list what is in it, walk into the folders (backup artefacts nest: pbm/<cluster>/…,
// pgbackrest/<cluster>/repo1/backup/db/…).
//
// Where the listing comes from. `weed server -s3` runs a **filer** on :8888 whose directory listing
// is already JSON — buckets are directories under /buckets. That beats parsing `weed shell`'s
// `fs.ls -l` text: it gives sizes, mtimes, a directory bit and real pagination. The S3 API on :8333
// could answer the same question, but only for a SigV4-signed request, which is a lot of ceremony
// for a listing the filer hands over for free.
//
// The exec is `sh -c`, not bash: the chrislusf/seaweedfs image is Alpine and has no bash (curl it
// does have). The bucket and path never reach the script as text — they are passed through the exec
// environment and referenced as "$BUCKET" / "$SUBPATH", so a bucket named `; rm -rf /` is inert.

// seaweedFilerPort is where `weed server` serves the filer (and its JSON directory listing).
const seaweedFilerPort = 8888

// seaweedListLimit is how many entries one listing returns. The filer pages with lastFileName, so a
// bucket with thousands of objects is walked a page at a time rather than materialised in one go.
const seaweedListLimit = 200

// seaweedObject is one entry in a bucket: a file, or a folder to descend into.
type seaweedObject struct {
	Name     string `json:"name"`     // the entry's own name, not its full path
	Path     string `json:"path"`     // path within the bucket ("pbm/cluster1/…"), what a click descends to
	Size     int64  `json:"size"`     // bytes (0 for a folder)
	Modified string `json:"modified"` // RFC3339, as the filer reports it
	Dir      bool   `json:"dir"`      //
}

// filerListing is the filer's JSON directory listing (only the fields we use).
type filerListing struct {
	Entries []struct {
		FullPath string    `json:"FullPath"`
		Mtime    time.Time `json:"Mtime"`
		Mode     uint32    `json:"Mode"` // Go's os.FileMode bits: the top bit marks a directory
		FileSize int64     `json:"FileSize"`
	} `json:"Entries"`
	LastFileName          string `json:"LastFileName"`
	ShouldDisplayLoadMore bool   `json:"ShouldDisplayLoadMore"`
}

// seaweedNode resolves the request's node to a running SeaweedFS container plus its config/secrets.
// loadRunningNode already does the authorization, the "not deployed"/"not running" answers and the
// container-id self-heal, and writes its own errors.
func (a *App) seaweedNode(w http.ResponseWriter, r *http.Request) (Deployment, seaweedConfig, bool) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return Deployment{}, seaweedConfig{}, false
	}
	var cfg seaweedConfig
	json.Unmarshal(dep.Config, &cfg)
	return dep, cfg, true
}

// cleanSeaweedPath sanitizes the sub-path inside a bucket. Traversal out of the bucket is refused
// outright rather than cleaned away: a request that contains ".." is not one we want to answer, and
// silently rewriting it would hide that.
func cleanSeaweedPath(p string) (string, error) {
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		return "", nil
	}
	if len(p) > 1024 {
		return "", fmt.Errorf("path is too long")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." {
			return "", fmt.Errorf("invalid path")
		}
	}
	return p, nil
}

// seaweedListScript asks the filer for one page of a directory listing. Every value it reads comes
// from the environment, never from the script text. The HTTP status is appended on its own last
// line: a folder that does not exist is a 404 with an empty body, which is a different answer from
// "the filer is not talking to us" and deserves a different message.
const seaweedListScript = `curl -sS -H 'Accept: application/json' -w '\n%{http_code}' \
  --get --data-urlencode "limit=$LIMIT" --data-urlencode "lastFileName=$AFTER" \
  "http://localhost:$PORT/buckets/$BUCKET/$SUBPATH"`

// errNoSuchFolder is a path the filer does not know — an empty prefix, or one that never existed.
var errNoSuchFolder = errors.New("no such folder")

// seaweedURLPath percent-encodes a path for the filer's URL, segment by segment. Object names are
// arbitrary — spaces, '#', '?' are all legal in S3 keys — and an unencoded one makes curl fail on
// the URL rather than simply listing nothing.
func seaweedURLPath(p string) string {
	if p == "" {
		return ""
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// listSeaweedObjects returns one page of a bucket's contents at the given sub-path.
func (a *App) listSeaweedObjects(ctx context.Context, containerID, bucket, path, after string) ([]seaweedObject, bool, string, error) {
	sub := seaweedURLPath(path)
	if sub != "" {
		sub += "/" // a directory listing, not the entry itself
	}
	env := []string{
		"BUCKET=" + url.PathEscape(bucket),
		"SUBPATH=" + sub,
		"AFTER=" + after,
		fmt.Sprintf("LIMIT=%d", seaweedListLimit),
		fmt.Sprintf("PORT=%d", seaweedFilerPort),
	}
	res, err := a.docker.Exec(ctx, containerID, []string{"sh", "-c", seaweedListScript}, env)
	if err != nil {
		return nil, false, "", err
	}
	body, status := res.Stdout, ""
	if i := strings.LastIndex(body, "\n"); i >= 0 {
		body, status = body[:i], strings.TrimSpace(body[i+1:])
	}
	switch {
	case status == "200":
	case status == "404":
		return nil, false, "", errNoSuchFolder
	case res.Code != 0 || status == "000":
		// curl could not reach the filer at all. The node takes a few seconds after a start before
		// the filer listens, and that is the usual reason to be here.
		return nil, false, "", fmt.Errorf("the filer is not answering yet — the node may still be starting")
	default:
		return nil, false, "", fmt.Errorf("the filer answered %s", status)
	}
	var listing filerListing
	if err := json.Unmarshal([]byte(body), &listing); err != nil {
		return nil, false, "", fmt.Errorf("the filer did not answer with a listing")
	}

	objs := make([]seaweedObject, 0, len(listing.Entries))
	for _, e := range listing.Entries {
		name := e.FullPath
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		dir := os.FileMode(e.Mode).IsDir()
		obj := seaweedObject{Name: name, Path: strings.Trim(path+"/"+name, "/"), Dir: dir}
		if !dir {
			obj.Size = e.FileSize
		}
		if !e.Mtime.IsZero() {
			obj.Modified = e.Mtime.UTC().Format(time.RFC3339)
		}
		objs = append(objs, obj)
	}
	return objs, listing.ShouldDisplayLoadMore, listing.LastFileName, nil
}

// handleSeaweedObjects — GET /api/stacks/{id}/nodes/{nid}/seaweed/objects?bucket=&path=&after=
func (a *App) handleSeaweedObjects(w http.ResponseWriter, r *http.Request) {
	dep, cfg, ok := a.seaweedNode(w, r)
	if !ok {
		return
	}
	// A bucket this node does not have falls back to its default — the panel asks for what it was
	// told the node holds, so a mismatch means a stale panel, not something worth erroring over.
	bucket := pickSeaweedBucket(cfg, r.URL.Query().Get("bucket"))
	if bucket == "" {
		writeErr(w, http.StatusNotFound, "this SeaweedFS node has no buckets")
		return
	}
	path, err := cleanSeaweedPath(r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	objs, more, after, err := a.listSeaweedObjects(r.Context(), dep.ContainerID, bucket, path, r.URL.Query().Get("after"))
	if errors.Is(err, errNoSuchFolder) {
		writeErr(w, http.StatusNotFound, "no such folder in "+bucket)
		return
	}
	// The deployment can still say "running" while the container is stopped (someone stopped it by
	// hand, or it crashed) — the exec then fails with Docker's own 409. Say what happened, rather
	// than handing the panel a container id and a status code.
	if err != nil && strings.Contains(err.Error(), "is not running") {
		writeErr(w, http.StatusConflict, "the SeaweedFS node is not running")
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list "+bucket+": "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"bucket":  bucket,
		"path":    path,
		"objects": objs,
		"more":    more,
		"after":   after,
	})
}
