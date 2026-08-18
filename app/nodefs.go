package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// nodefs.go — the node File Manager: browse, transfer and re-own a deployed
// node's filesystem from the designer.
//
// Scope, deliberately: this is arbitrary read/write inside the container,
// unlike nodeupload.go's four-destination drop. That is not a relaxation that
// snuck in — the same right-click menu already offers "Enter root console",
// which is a root shell and therefore strictly more. The boundary that matters
// is the *stack*: every handler goes through loadRunningNode, so you can only
// reach nodes on a stack you own (or any stack, if you are an admin), exactly
// like the terminal.
//
// Two mechanisms, chosen per operation:
//
//   - Metadata and mutation (list, chmod, chown, mkdir, rm, mv) exec a small
//     `sh` script. There is no way around a shell for these.
//   - Bulk bytes (download, upload, node→node copy) go through the engine's
//     archive endpoints, which need no shell, no tar and no coreutils in the
//     image, and stream rather than buffer.
//
// A shell-less image (distroless, e.g. the sim nodes) therefore cannot be
// browsed, and says so plainly instead of returning an empty directory that
// looks like a real answer.

// fsEntry is one directory entry as the browser sees it.
type fsEntry struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // file | dir | link | other
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`  // octal, e.g. "0644"
	Perms   string `json:"perms"` // ls-style, e.g. "-rw-r--r--"
	UID     int    `json:"uid"`
	GID     int    `json:"gid"`
	User    string `json:"user"` // resolved name, or the uid again
	Group   string `json:"group"`
	ModTime int64  `json:"modTime"`          // unix seconds
	Target  string `json:"target,omitempty"` // symlink target
}

// fsListScript prints one NUL-terminated record per entry in $DIR.
//
// The record format is `hexmode;size;uid;gid;user;group;mtime;perms;%N`, where
// %N is stat's quoted name (and, for a symlink, `'name' -> 'target'`). NUL
// terminators rather than newlines because a filename may legally contain a
// newline, and one such file would otherwise desynchronise the whole listing.
//
// `stat --printf` is GNU; busybox accepts only `-c`, which always appends a
// newline. The fallback trades newline-safety for working on Alpine-based
// nodes, which is the right way round: a shell-less image cannot be browsed at
// all, and a newline in a filename is rare where Alpine is common.
const fsListScript = `
set -u
[ -d "$DIR" ] || { echo "not a directory: $DIR" >&2; exit 2; }
if stat --printf '%f' "$DIR" >/dev/null 2>&1; then
  find "$DIR" -mindepth 1 -maxdepth 1 -exec stat --printf '%f;%s;%u;%g;%U;%G;%Y;%A;%N\0' {} + 2>/dev/null
else
  find "$DIR" -mindepth 1 -maxdepth 1 -exec stat -c '%f;%s;%u;%g;%U;%G;%Y;%A;%N' {} + 2>/dev/null | tr '\n' '\0'
fi
`

// handleFSList lists one directory on a node.
//
//	GET /api/stacks/{id}/nodes/{nid}/fs/list?path=/etc
func (a *App) handleFSList(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return
	}
	dir := fsCleanPath(r.URL.Query().Get("path"))
	res, err := a.engCtx(r.Context()).Exec(r.Context(), dep.ContainerID,
		[]string{"sh", "-c", fsListScript}, []string{"DIR=" + dir})
	if err != nil {
		writeErr(w, http.StatusBadGateway, fsShellHint(err.Error()))
		return
	}
	if res.Code != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = "cannot read " + dir
		}
		writeErr(w, http.StatusBadRequest, fsShellHint(msg))
		return
	}
	entries := parseFSList(res.Stdout)
	// Directories first, then case-insensitive by name — the order every file
	// manager uses, done here so every client agrees without re-sorting.
	sort.Slice(entries, func(i, j int) bool {
		di, dj := entries[i].Type == "dir", entries[j].Type == "dir"
		if di != dj {
			return di
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	writeJSON(w, http.StatusOK, map[string]any{"path": dir, "entries": entries})
}

// fsShellHint explains the one failure mode that is not the user's fault: an
// image with no shell can never be browsed, and "exec failed" does not say so.
func fsShellHint(msg string) string {
	l := strings.ToLower(msg)
	if strings.Contains(l, "no such file or directory") && strings.Contains(l, "exec") ||
		strings.Contains(l, "executable file not found") ||
		strings.Contains(l, "starting container process") {
		return "this node's image has no shell, so its filesystem cannot be browsed"
	}
	return msg
}

// parseFSList turns the script's NUL-separated records into entries. A record
// that does not parse is skipped rather than failing the listing: one odd file
// should not make a directory unreadable.
func parseFSList(out string) []fsEntry {
	entries := []fsEntry{}
	for _, rec := range strings.Split(out, "\x00") {
		rec = strings.TrimLeft(rec, "\n")
		if strings.TrimSpace(rec) == "" {
			continue
		}
		// SplitN with 9: the last field (%N) may itself contain ';'.
		f := strings.SplitN(rec, ";", 9)
		if len(f) < 9 {
			continue
		}
		raw, err := strconv.ParseUint(f[0], 16, 32)
		if err != nil {
			continue
		}
		name, target := parseStatName(f[8])
		if name == "" {
			continue
		}
		e := fsEntry{
			Name:   path.Base(name),
			Type:   fsTypeOf(raw),
			Mode:   fmt.Sprintf("%04o", raw&0o7777),
			Perms:  f[7],
			User:   f[4],
			Group:  f[5],
			Target: target,
		}
		e.Size, _ = strconv.ParseInt(f[1], 10, 64)
		e.UID, _ = strconv.Atoi(f[2])
		e.GID, _ = strconv.Atoi(f[3])
		e.ModTime, _ = strconv.ParseInt(f[6], 10, 64)
		// An id with no passwd/group entry — routine after a copy between nodes
		// that do not share users. GNU stat prints the number, busybox prints
		// "UNKNOWN"; the number is the useful answer, so normalize to it.
		if e.User == "UNKNOWN" || e.User == "" {
			e.User = strconv.Itoa(e.UID)
		}
		if e.Group == "UNKNOWN" || e.Group == "" {
			e.Group = strconv.Itoa(e.GID)
		}
		entries = append(entries, e)
	}
	return entries
}

// fsTypeOf maps stat's raw hex mode (st_mode) to a coarse kind.
func fsTypeOf(raw uint64) string {
	switch raw & 0o170000 {
	case 0o040000:
		return "dir"
	case 0o120000:
		return "link"
	case 0o100000:
		return "file"
	default:
		return "other" // socket, fifo, block/char device
	}
}

// parseStatName unpacks stat's %N: `'name'`, or `'name' -> 'target'` for a
// symlink. Quoting is stat's own, and it escapes an embedded quote as '\”.
func parseStatName(s string) (name, target string) {
	s = strings.TrimSpace(s)
	unq := func(v string) string {
		v = strings.TrimSpace(v)
		if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
			v = v[1 : len(v)-1]
		}
		return strings.ReplaceAll(v, `'\''`, "'")
	}
	if i := strings.LastIndex(s, "' -> '"); i >= 0 {
		return unq(s[:i+1]), unq(s[i+5:])
	}
	return unq(s), ""
}

// fsCleanPath normalizes a client-supplied absolute path. Everything is
// resolved against "/" so a relative or traversing path can only ever land
// somewhere inside the container — which, for a tool whose whole job is the
// container's filesystem, is the only invariant worth enforcing here.
func fsCleanPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	c := path.Clean(p)
	if c == "." {
		return "/"
	}
	return c
}

// fsExec runs a script with $1.. bound to the given paths, so a filename with
// spaces, quotes or newlines needs no escaping anywhere.
func (a *App) fsExec(ctx context.Context, containerID, script string, env []string, args ...string) error {
	cmd := append([]string{"sh", "-c", script, "sh"}, args...)
	res, err := a.engCtx(ctx).Exec(ctx, containerID, cmd, env)
	if err != nil {
		return fmt.Errorf("%s", fsShellHint(err.Error()))
	}
	if res.Code != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		if msg == "" {
			msg = fmt.Sprintf("command failed (exit %d)", res.Code)
		}
		return fmt.Errorf("%s", fsShellHint(msg))
	}
	return nil
}

// fsPathsBody is the shape every mutating endpoint takes: the paths to act on,
// plus whether a directory among them should be walked.
type fsPathsBody struct {
	Paths     []string `json:"paths"`
	Recursive bool     `json:"recursive"`
	Mode      string   `json:"mode"`  // chmod: octal, or a symbolic clause like u+x
	Owner     string   `json:"owner"` // chown: user name or uid ("" leaves it)
	Group     string   `json:"group"` // chown: group name or gid ("" leaves it)
	Path      string   `json:"path"`  // mkdir / rename source
	To        string   `json:"to"`    // rename destination
}

// cleanPaths normalizes and rejects an empty selection.
func (b fsPathsBody) cleanPaths() ([]string, error) {
	out := make([]string, 0, len(b.Paths))
	for _, p := range b.Paths {
		c := fsCleanPath(p)
		if c == "/" {
			return nil, fmt.Errorf("refusing to operate on / itself")
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no paths given")
	}
	return out, nil
}

// loadFSRequest is the preamble every mutating handler shares.
func (a *App) loadFSRequest(w http.ResponseWriter, r *http.Request) (Deployment, fsPathsBody, bool) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return Deployment{}, fsPathsBody{}, false
	}
	var b fsPathsBody
	if err := decode(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return Deployment{}, fsPathsBody{}, false
	}
	return dep, b, true
}

// handleFSChmod changes permissions on the selected paths, optionally walking
// directories among them.
func (a *App) handleFSChmod(w http.ResponseWriter, r *http.Request) {
	dep, b, ok := a.loadFSRequest(w, r)
	if !ok {
		return
	}
	paths, err := b.cleanPaths()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	mode := strings.TrimSpace(b.Mode)
	if !validChmodMode(mode) {
		writeErr(w, http.StatusBadRequest, "mode must be octal (e.g. 0644, 755) or symbolic (e.g. u+x, go-w)")
		return
	}
	// $MODE via the environment and paths via $@: neither can be read as an
	// option or a second command however they are spelled. `--` stops chmod
	// treating a leading-dash filename as a flag.
	script := `set -e; ` + fsRecursiveFlag(b.Recursive, "chmod") + ` "$MODE" -- "$@"`
	if err := a.fsExec(r.Context(), dep.ContainerID, script, []string{"MODE=" + mode}, paths...); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"changed": len(paths)})
}

// handleFSChown changes owner and/or group on the selected paths.
func (a *App) handleFSChown(w http.ResponseWriter, r *http.Request) {
	dep, b, ok := a.loadFSRequest(w, r)
	if !ok {
		return
	}
	paths, err := b.cleanPaths()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	owner, group := strings.TrimSpace(b.Owner), strings.TrimSpace(b.Group)
	if !validIdentity(owner) || !validIdentity(group) {
		writeErr(w, http.StatusBadRequest, "owner and group must be a plain user/group name or numeric id")
		return
	}
	if owner == "" && group == "" {
		writeErr(w, http.StatusBadRequest, "give an owner, a group, or both")
		return
	}
	// "user:group", ":group" and "user" are all valid chown specs, which is
	// exactly the three cases the dialog can produce.
	spec := owner
	if group != "" {
		spec += ":" + group
	}
	script := `set -e; ` + fsRecursiveFlag(b.Recursive, "chown") + ` "$SPEC" -- "$@"`
	if err := a.fsExec(r.Context(), dep.ContainerID, script, []string{"SPEC=" + spec}, paths...); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"changed": len(paths)})
}

func fsRecursiveFlag(recursive bool, cmd string) string {
	if recursive {
		return cmd + " -R"
	}
	return cmd
}

// validChmodMode accepts an octal mode or a symbolic clause. Anything else is
// refused rather than passed through: the value reaches a shell, and while it
// travels in the environment (so it cannot become a second command), a typo
// silently applying the wrong bits is its own kind of damage.
func validChmodMode(m string) bool {
	if m == "" {
		return false
	}
	if n, err := strconv.ParseUint(m, 8, 32); err == nil {
		return n <= 0o7777
	}
	for _, c := range m {
		if !strings.ContainsRune("ugoa+-=rwxXst,", c) {
			return false
		}
	}
	return true
}

// validIdentity accepts "" (leave unchanged) or a plain user/group name or id.
func validIdentity(s string) bool {
	if s == "" {
		return true
	}
	if len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '_' || c == '-' || c == '.' || c == '$') {
			return false
		}
	}
	return true
}

// handleFSMkdir creates a directory (and any missing parents).
func (a *App) handleFSMkdir(w http.ResponseWriter, r *http.Request) {
	dep, b, ok := a.loadFSRequest(w, r)
	if !ok {
		return
	}
	p := fsCleanPath(b.Path)
	if p == "/" {
		writeErr(w, http.StatusBadRequest, "give a directory name")
		return
	}
	if err := a.fsExec(r.Context(), dep.ContainerID, `set -e; mkdir -p -- "$1"`, nil, p); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": p})
}

// handleFSDelete removes the selected paths. A directory needs Recursive, so a
// mis-click on a folder cannot take its contents with it.
func (a *App) handleFSDelete(w http.ResponseWriter, r *http.Request) {
	dep, b, ok := a.loadFSRequest(w, r)
	if !ok {
		return
	}
	paths, err := b.cleanPaths()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	script := `set -e; rm -f -- "$@"`
	if b.Recursive {
		script = `set -e; rm -rf -- "$@"`
	}
	if err := a.fsExec(r.Context(), dep.ContainerID, script, nil, paths...); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": len(paths)})
}

// handleFSRename moves/renames one path. `mv -n` so it can never silently
// clobber an existing file the user could not see.
func (a *App) handleFSRename(w http.ResponseWriter, r *http.Request) {
	dep, b, ok := a.loadFSRequest(w, r)
	if !ok {
		return
	}
	from, to := fsCleanPath(b.Path), fsCleanPath(b.To)
	if from == "/" || to == "/" || from == to {
		writeErr(w, http.StatusBadRequest, "give a different source and destination")
		return
	}
	script := `set -e; [ -e "$2" ] && { echo "already exists: $2" >&2; exit 1; }; mv -n -- "$1" "$2"`
	if err := a.fsExec(r.Context(), dep.ContainerID, script, nil, from, to); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": to})
}

// fsIdentitiesScript dumps the node's users and groups for the ownership
// pickers. /etc/passwd and /etc/group are read directly rather than via
// getent, which is not present on every image.
const fsIdentitiesScript = `
{ echo "#users"; cut -d: -f1,3 /etc/passwd 2>/dev/null; echo "#groups"; cut -d: -f1,3 /etc/group 2>/dev/null; }
`

// handleFSIdentities returns the node's users and groups, so the properties
// dialog can offer what the node actually has instead of a free-text box.
func (a *App) handleFSIdentities(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return
	}
	res, err := a.engCtx(r.Context()).Exec(r.Context(), dep.ContainerID,
		[]string{"sh", "-c", fsIdentitiesScript}, nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, fsShellHint(err.Error()))
		return
	}
	type ident struct {
		Name string `json:"name"`
		ID   int    `json:"id"`
	}
	users, groups := []ident{}, []ident{}
	cur := &users
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "#users":
			cur = &users
			continue
		case line == "#groups":
			cur = &groups
			continue
		case line == "":
			continue
		}
		name, idStr, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		id, err := strconv.Atoi(idStr)
		if err != nil {
			continue
		}
		*cur = append(*cur, ident{Name: name, ID: id})
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users, "groups": groups})
}

// handleFSUpload copies host files into an arbitrary directory on the node.
// Same machinery and the same configured ceiling as the drag-and-drop drop
// (nodeupload.go); only the destination whitelist is gone, which is the whole
// difference between "drop it somewhere obvious" and "a file manager".
func (a *App) handleFSUpload(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return
	}
	if err := r.ParseMultipartForm(nodeUploadMemory); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid upload")
		return
	}
	defer r.MultipartForm.RemoveAll()

	dest := fsCleanPath(r.FormValue("dest"))
	plan, err := planUpload(r.MultipartForm, a.maxUploadBytes())
	if err != nil {
		writeErr(w, uploadStatus(err), err.Error())
		return
	}
	// The destination must already exist: PutArchive against a missing path is
	// an opaque daemon error, and silently creating it would hide a typo that
	// scatters files into a directory nobody meant.
	if err := a.fsExec(r.Context(), dep.ContainerID, `[ -d "$1" ] || { echo "no such directory: $1" >&2; exit 1; }`, nil, dest); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	pr, pw := io.Pipe()
	buildErr := make(chan error, 1)
	go func() {
		err := writeUploadTar(pw, plan)
		buildErr <- err
		pw.CloseWithError(err)
	}()
	putErr := a.engCtx(r.Context()).PutArchiveStream(r.Context(), dep.ContainerID, dest, pr)
	pr.CloseWithError(putErr)
	if err := <-buildErr; err != nil {
		writeErr(w, uploadStatus(err), "read upload: "+err.Error())
		return
	}
	if putErr != nil {
		writeErr(w, http.StatusInternalServerError, "copy to node: "+putErr.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dest": dest, "files": plan.names, "bytes": plan.total})
}

// handleFSDownload streams paths off a node to the browser.
//
//	GET /api/stacks/{id}/nodes/{nid}/fs/download?path=/etc/hosts[&path=...]
//
// One regular file comes back as itself; a directory, or any multi-selection,
// comes back as a .tar.gz. Both stream — a download is not bounded by the
// upload ceiling and may be the whole of /var.
func (a *App) handleFSDownload(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return
	}
	raw := r.URL.Query()["path"]
	if len(raw) == 0 {
		writeErr(w, http.StatusBadRequest, "no path given")
		return
	}
	paths := make([]string, 0, len(raw))
	for _, p := range raw {
		paths = append(paths, fsCleanPath(p))
	}
	eng := a.engCtx(r.Context())

	if len(paths) == 1 {
		// Ask the archive endpoint for it and look at the header: one regular
		// file is served raw, anything else falls through to the tar.gz path.
		rc, err := eng.GetArchiveStream(r.Context(), dep.ContainerID, paths[0])
		if err != nil {
			writeErr(w, http.StatusNotFound, "cannot read "+paths[0])
			return
		}
		defer rc.Close()
		tr := tar.NewReader(rc)
		hdr, err := tr.Next()
		if err != nil {
			writeErr(w, http.StatusNotFound, "cannot read "+paths[0])
			return
		}
		if hdr.Typeflag == tar.TypeReg {
			name := path.Base(paths[0])
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.FormatInt(hdr.Size, 10))
			w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(name))
			io.Copy(w, tr)
			return
		}
		// A directory: rewind by re-fetching, and archive it below. (The stream
		// is one-way, so there is nothing to seek back to.)
	}

	name := path.Base(paths[0])
	if name == "/" || name == "." || name == "" {
		name = "root"
	}
	if len(paths) > 1 {
		name = "files"
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(name+".tar.gz"))
	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	for _, p := range paths {
		if err := copyArchiveInto(r.Context(), eng, dep.ContainerID, p, tw); err != nil {
			// The response is already streaming, so the status is long since
			// sent; the truncated archive plus this log is all that can be said.
			return
		}
	}
}

// copyArchiveInto pipes one path's tar from a node into tw, entry by entry, so
// several selections become one archive without any of it touching disk.
func copyArchiveInto(ctx context.Context, eng Engine, containerID, p string, tw *tar.Writer) error {
	rc, err := eng.GetArchiveStream(ctx, containerID, p)
	if err != nil {
		return err
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := io.Copy(tw, tr); err != nil {
				return err
			}
		}
	}
}

// handleFSTransfer copies paths from this node into a directory on another node
// in the same stack.
//
//	POST /api/stacks/{id}/nodes/{nid}/fs/transfer
//	{ paths: [...], toNodeId: "...", toPath: "/tmp" }
//
// Source tar → destination extract, streamed through a pipe: nothing lands on
// the DBCanvas host, so copying a 10 GiB directory between two nodes needs no
// space here at all. The two nodes may be on different engines (a Docker node
// and a Vagrant VM in a hybrid stack), which is why each side resolves its own.
func (a *App) handleFSTransfer(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	srcID := r.PathValue("nid")
	var b struct {
		Paths    []string `json:"paths"`
		ToNodeID string   `json:"toNodeId"`
		ToPath   string   `json:"toPath"`
	}
	if err := decode(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(b.Paths) == 0 {
		writeErr(w, http.StatusBadRequest, "no paths given")
		return
	}
	if b.ToNodeID == "" {
		writeErr(w, http.StatusBadRequest, "no destination node given")
		return
	}
	if b.ToNodeID == srcID {
		writeErr(w, http.StatusBadRequest, "source and destination are the same node")
		return
	}
	src, err := a.runningNode(st, srcID)
	if err != nil {
		writeErr(w, http.StatusConflict, "source node: "+err.Error())
		return
	}
	dst, err := a.runningNode(st, b.ToNodeID)
	if err != nil {
		writeErr(w, http.StatusConflict, "destination node: "+err.Error())
		return
	}
	srcEng := a.depEngine(st, srcID)
	dstEng := a.depEngine(st, b.ToNodeID)
	toPath := fsCleanPath(b.ToPath)

	ctx := r.Context()
	if err := a.fsExec(withEngine(ctx, dstEng), dst.ContainerID,
		`[ -d "$1" ] || { echo "no such directory: $1" >&2; exit 1; }`, nil, toPath); err != nil {
		writeErr(w, http.StatusBadRequest, "destination node: "+err.Error())
		return
	}

	pr, pw := io.Pipe()
	readErr := make(chan error, 1)
	go func() {
		tw := tar.NewWriter(pw)
		var err error
		for _, p := range b.Paths {
			if err = copyArchiveInto(ctx, srcEng, src.ContainerID, fsCleanPath(p), tw); err != nil {
				break
			}
		}
		if err == nil {
			err = tw.Close()
		}
		readErr <- err
		pw.CloseWithError(err)
	}()
	putErr := dstEng.PutArchiveStream(ctx, dst.ContainerID, toPath, pr)
	pr.CloseWithError(putErr)
	if err := <-readErr; err != nil {
		writeErr(w, http.StatusInternalServerError, "read from source: "+err.Error())
		return
	}
	if putErr != nil {
		writeErr(w, http.StatusInternalServerError, "write to destination: "+putErr.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"copied": len(b.Paths), "toPath": toPath})
}

// runningNode resolves a node of a stack that is deployed and running. Used for
// the *other* end of a transfer, which no request path names and so
// loadRunningNode cannot reach.
func (a *App) runningNode(st Stack, nodeID string) (Deployment, error) {
	dep, err := a.store.GetDeployment(st.ID, nodeID)
	if err != nil || dep.ContainerID == "" {
		return Deployment{}, fmt.Errorf("not deployed")
	}
	if dep.State != DeployRunning {
		return Deployment{}, fmt.Errorf("not running")
	}
	return dep, nil
}

// handleFSNodes lists the stack's other running nodes, for the file manager's
// second pane and the transfer destination picker.
func (a *App) handleFSNodes(w http.ResponseWriter, r *http.Request) {
	st, _, ok := a.loadOwnedStack(w, r)
	if !ok {
		return
	}
	var doc designDoc
	json.Unmarshal(st.Design, &doc)
	deps, _ := a.store.ListDeployments(st.ID)
	running := map[string]bool{}
	for _, d := range deps {
		running[d.NodeID] = d.State == DeployRunning && d.ContainerID != ""
	}
	type node struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Type  string `json:"type"`
	}
	out := []node{}
	for _, n := range doc.Nodes {
		if running[n.ID] {
			out = append(out, node{ID: n.ID, Label: n.Label, Type: n.Type})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": out})
}

// --- editing a file in place -----------------------------------------------

// fsMaxEdit bounds what the editor will open. A textarea is not a pager, and
// pulling an arbitrary file into one is how you wedge a browser tab; anything
// larger is a download, which is unbounded and streams.
const fsMaxEdit = 2 << 20 // 2 MiB

// handleFSRead returns a text file's contents for the editor, along with the
// metadata a save has to preserve and the mtime a save checks against.
//
//	GET /api/stacks/{id}/nodes/{nid}/fs/read?path=/etc/hosts
//
// Read through the archive endpoint rather than `cat`: it needs no shell, and
// the tar header hands over mode, uid/gid and size in the same round trip —
// which is exactly what handleFSWrite must put back.
func (a *App) handleFSRead(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return
	}
	p := fsCleanPath(r.URL.Query().Get("path"))
	rc, err := a.engCtx(r.Context()).GetArchiveStream(r.Context(), dep.ContainerID, p)
	if err != nil {
		writeErr(w, http.StatusNotFound, "cannot read "+p)
		return
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	hdr, err := tr.Next()
	if err != nil {
		writeErr(w, http.StatusNotFound, "cannot read "+p)
		return
	}
	if hdr.Typeflag != tar.TypeReg {
		writeErr(w, http.StatusBadRequest, "only a regular file can be edited")
		return
	}
	if hdr.Size > fsMaxEdit {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
			"%s is %s — too large to edit (limit %s). Download it instead.",
			path.Base(p), humanLimit(hdr.Size), humanLimit(fsMaxEdit)))
		return
	}
	// tr is positioned at the entry's body now that Next() has consumed its
	// header; the limit is belt-and-braces behind the Size check above.
	body, err := io.ReadAll(io.LimitReader(tr, fsMaxEdit))
	if err != nil {
		writeErr(w, http.StatusBadGateway, "cannot read "+p+": "+err.Error())
		return
	}
	if !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 {
		writeErr(w, http.StatusBadRequest,
			path.Base(p)+" looks like a binary file. Editing it as text would corrupt it — download it instead.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":    p,
		"content": string(body),
		"size":    hdr.Size,
		"mode":    fmt.Sprintf("%04o", hdr.Mode&0o7777),
		"uid":     hdr.Uid,
		"gid":     hdr.Gid,
		"modTime": hdr.ModTime.Unix(),
	})
}

// handleFSWrite saves the editor's buffer back over the file.
//
//	POST /api/stacks/{id}/nodes/{nid}/fs/write
//	{ path, content, mode, uid, gid, ifModTime }
//
// The caller echoes back the mode/uid/gid it was given, and they go into the
// tar header, so saving a file does not quietly turn it into a root-owned 0644
// one — the classic way an in-place editor breaks a service that cared who
// owned its config.
func (a *App) handleFSWrite(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return
	}
	var b struct {
		Path      string `json:"path"`
		Content   string `json:"content"`
		Mode      string `json:"mode"`
		UID       int    `json:"uid"`
		GID       int    `json:"gid"`
		IfModTime int64  `json:"ifModTime"` // 0 = write regardless
	}
	if err := decode(r, &b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	p := fsCleanPath(b.Path)
	if p == "/" {
		writeErr(w, http.StatusBadRequest, "no file given")
		return
	}
	if int64(len(b.Content)) > fsMaxEdit {
		writeErr(w, http.StatusRequestEntityTooLarge,
			"the edited file is larger than the "+humanLimit(fsMaxEdit)+" editing limit")
		return
	}
	mode, err := strconv.ParseUint(strings.TrimSpace(b.Mode), 8, 32)
	if err != nil || mode > 0o7777 {
		mode = 0o644 // a new file, or a mode the client mangled
	}

	eng := a.engCtx(r.Context())
	// Refuse to clobber a file that changed underneath the editor — someone
	// else's console session, or a service rewriting its own config. Skipped
	// when the file does not exist yet, which is how "save as" works.
	if b.IfModTime > 0 {
		if rc, err := eng.GetArchiveStream(r.Context(), dep.ContainerID, p); err == nil {
			hdr, herr := tar.NewReader(rc).Next()
			rc.Close()
			if herr == nil && hdr.ModTime.Unix() != b.IfModTime {
				writeErr(w, http.StatusConflict,
					"the file changed on the node since you opened it — reopen it to see the current contents")
				return
			}
		}
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{
		Name: path.Base(p), Mode: int64(mode), Size: int64(len(b.Content)),
		Uid: b.UID, Gid: b.GID, ModTime: time.Now(), Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tw.Write([]byte(b.Content)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := tw.Close(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := eng.PutArchive(r.Context(), dep.ContainerID, path.Dir(p), buf.Bytes()); err != nil {
		writeErr(w, http.StatusInternalServerError, "save to node: "+err.Error())
		return
	}
	// Hand back the new mtime so the editor can keep saving without reopening.
	var modTime int64
	if rc, err := eng.GetArchiveStream(r.Context(), dep.ContainerID, p); err == nil {
		if h, herr := tar.NewReader(rc).Next(); herr == nil {
			modTime = h.ModTime.Unix()
		}
		rc.Close()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": p, "bytes": len(b.Content), "modTime": modTime,
	})
}
