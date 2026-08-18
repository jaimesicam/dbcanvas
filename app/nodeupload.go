package main

import (
	"archive/tar"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// nodeupload.go — drop files from the host onto a deployed node.
//
// The Stack Designer lets you drag files (or a whole folder) from the desktop
// onto a running node's card and pick where they land. This is the receiving
// end: one multipart POST holding the files, one destination directory, and a
// tar streamed into the node's engine, which extracts it (Docker's "upload to
// container", `tar -x` over ssh for a Vagrant VM).
//
// Nothing is buffered whole. The multipart parser spills parts past
// nodeUploadMemory to temp files and the tar is piped straight to the engine as
// it is built, so peak memory does not track the size of the drop — which is
// what lets the ceiling be the gibibyte-scale, admin-configurable
// maxUploadBytes (syssettings.go) instead of whatever fits in RAM.
//
// The relative path of each file rides in its multipart *field name*, not in
// the filename: Go's multipart reader runs filepath.Base() over the filename
// parameter (RFC 7578 §4.2), which would flatten a dropped folder into its
// leaf names and silently collide.

// nodeUploadDests is the closed set of destinations the UI offers and the
// server accepts. A free-form path would make this an arbitrary-write endpoint
// into the container, so the whitelist is the check — not just the UI's menu.
var nodeUploadDests = map[string]bool{"/": true, "/home": true, "/root": true, "/tmp": true}

// useDataTempDir points os.TempDir() at a directory beside the SQLite file, the
// same place ptStalkArchiveDir() keeps its tarballs — one volume holds all of
// dbcanvas's state.
//
// This matters because of node uploads. A multi-gigabyte drop spills out of the
// multipart parser onto disk, and the default /tmp inside the distroless
// container is the writable overlay layer: not sized for it, not a volume, and
// gone on the next `docker compose up --build`. Best-effort — if the directory
// cannot be made, the default is left alone rather than failing startup.
func useDataTempDir(dbPath string) {
	dir := filepath.Dir(dbPath)
	if dir == "" || dir == "." {
		return // running from the working directory in dev; the system /tmp is fine
	}
	tmp := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		log.Printf("upload temp dir %s unavailable (%v); using %s", tmp, err, os.TempDir())
		return
	}
	// Leftovers from a previous run: the parser removes its own files, but a
	// crash mid-upload strands gigabytes that nothing else will ever clean up.
	if ents, err := os.ReadDir(tmp); err == nil {
		for _, e := range ents {
			os.RemoveAll(filepath.Join(tmp, e.Name()))
		}
	}
	os.Setenv("TMPDIR", tmp) // os.TempDir() reads this at call time
}

// nodeUploadMemory is how much of the multipart body is buffered in memory;
// parts beyond it spill to temp files. The tar is then streamed from those
// files to the engine, so peak memory stays at this figure no matter how large
// the drop is — which is what makes a gibibyte-scale ceiling (see
// syssettings.go) something other than an out-of-memory kill.
const nodeUploadMemory = 8 << 20

// handleNodeUpload copies one or more host files into a running node.
//
//	POST /api/stacks/{id}/nodes/{nid}/upload
//	multipart/form-data: dest=<one of nodeUploadDests>, one file part per file
//	                     whose field name is the path relative to dest
func (a *App) handleNodeUpload(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningNode(w, r)
	if !ok {
		return
	}
	if err := r.ParseMultipartForm(nodeUploadMemory); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid upload")
		return
	}
	defer r.MultipartForm.RemoveAll()

	dest := strings.TrimSpace(r.FormValue("dest"))
	if !nodeUploadDests[dest] {
		writeErr(w, http.StatusBadRequest, "unsupported destination: "+dest)
		return
	}
	plan, err := planUpload(r.MultipartForm, a.maxUploadBytes())
	if err != nil {
		writeErr(w, uploadStatus(err), err.Error())
		return
	}

	// Stream the tar to the engine as it is built: at gibibyte scale, buffering
	// it first is the difference between a copy and an OOM. A pipe write fails
	// once the engine side errors, so the builder unwinds instead of blocking,
	// and its error is what gets reported — the engine's "broken pipe" is the
	// symptom, not the cause.
	pr, pw := io.Pipe()
	buildErr := make(chan error, 1)
	go func() {
		err := writeUploadTar(pw, plan)
		buildErr <- err
		pw.CloseWithError(err)
	}()
	putErr := a.engCtx(r.Context()).PutArchiveStream(r.Context(), dep.ContainerID, dest, pr)
	pr.CloseWithError(putErr) // unblock the builder if the engine gave up early
	if err := <-buildErr; err != nil {
		writeErr(w, uploadStatus(err), "read upload: "+err.Error())
		return
	}
	if putErr != nil {
		writeErr(w, http.StatusInternalServerError, "copy to node: "+putErr.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"dest":  dest,
		"files": plan.names,
		"bytes": plan.total,
	})
}

// uploadError carries the status code a build failure should surface as, so the
// browser can tell "you sent something bad" from "we could not read it".
type uploadError struct {
	status int
	msg    string
}

func (e uploadError) Error() string { return e.msg }

func uploadStatus(err error) int {
	if ue, ok := err.(uploadError); ok {
		return ue.status
	}
	return http.StatusInternalServerError
}

// uploadPlan is a validated drop: which parts go where, in what order, and how
// big the whole thing is. Everything that can be rejected is rejected here,
// before a byte reaches the engine — once the tar is streaming, an error means
// a half-extracted destination.
type uploadPlan struct {
	entries []uploadEntry
	names   []string // entries' paths, for the response
	total   int64
}

type uploadEntry struct {
	path string // cleaned, relative to the destination
	fh   *multipart.FileHeader
}

// planUpload validates the parsed multipart form against max and returns the
// order to write it in. Each form field name is one file's path relative to the
// destination.
func planUpload(form *multipart.Form, max int64) (uploadPlan, error) {
	var p uploadPlan
	// Field names are the relative paths; walk them in a stable order so the tar
	// (and the "copied N files" report) does not depend on map iteration.
	rels := make([]string, 0, len(form.File))
	for rel := range form.File {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	for _, rel := range rels {
		for _, fh := range form.File[rel] {
			p.total += fh.Size
		}
	}
	if p.total > max {
		return p, uploadError{http.StatusRequestEntityTooLarge,
			fmt.Sprintf("upload is %s, over the %s limit for this instance", humanLimit(p.total), humanLimit(max))}
	}

	for _, rel := range rels {
		clean, err := cleanUploadPath(rel)
		if err != nil {
			return p, uploadError{http.StatusBadRequest, err.Error()}
		}
		fhs := form.File[rel]
		if len(fhs) == 0 {
			continue
		}
		p.entries = append(p.entries, uploadEntry{path: clean, fh: fhs[0]})
		p.names = append(p.names, clean)
	}
	if len(p.entries) == 0 {
		return p, uploadError{http.StatusBadRequest, "no files in the upload"}
	}
	return p, nil
}

// writeUploadTar streams the planned drop as a tar into w.
func writeUploadTar(w io.Writer, p uploadPlan) error {
	tw := tar.NewWriter(w)
	dirs := map[string]bool{}
	for _, e := range p.entries {
		// A dropped folder arrives as its files with "dir/sub/file" paths; the
		// intermediate directories need their own tar entries, or extracting
		// creates them with whatever mode the extractor defaults to.
		if err := writeUploadDirs(tw, path.Dir(e.path), dirs); err != nil {
			return err
		}
		f, err := e.fh.Open()
		if err != nil {
			return fmt.Errorf("read %s: %w", e.path, err)
		}
		hdr := &tar.Header{Name: e.path, Mode: 0o644, ModTime: time.Now(), Size: e.fh.Size}
		if err := tw.WriteHeader(hdr); err != nil {
			f.Close()
			return err
		}
		// Copy exactly fh.Size bytes: the header is already written with that
		// size, so a short or long part would corrupt the stream.
		n, err := io.Copy(tw, io.LimitReader(f, e.fh.Size))
		f.Close()
		if err != nil {
			return err
		}
		if n != e.fh.Size {
			return fmt.Errorf("read %s: upload truncated", e.path)
		}
	}
	return tw.Close()
}

// cleanUploadPath validates one relative path from the drop and returns it in
// the slash-separated, no-leading-slash form tar wants. Anything that could
// escape the chosen destination — an absolute path, a ".." segment, an empty
// name — is rejected rather than sanitized, because a silently rewritten path
// puts the file somewhere the user did not ask for.
func cleanUploadPath(rel string) (string, error) {
	rel = strings.TrimSpace(strings.ReplaceAll(rel, `\`, "/"))
	if rel == "" {
		return "", fmt.Errorf("empty file name in the upload")
	}
	if strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("absolute path in the upload: %s", rel)
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("unsafe path in the upload: %s", rel)
		}
	}
	return rel, nil
}

// writeUploadDirs emits tar entries for dir and each of its parents, once each.
func writeUploadDirs(tw *tar.Writer, dir string, seen map[string]bool) error {
	if dir == "." || dir == "/" || dir == "" || seen[dir] {
		return nil
	}
	if err := writeUploadDirs(tw, path.Dir(dir), seen); err != nil {
		return err
	}
	seen[dir] = true
	return tw.WriteHeader(&tar.Header{
		Name: dir + "/", Mode: 0o755, ModTime: time.Now(), Typeflag: tar.TypeDir,
	})
}
