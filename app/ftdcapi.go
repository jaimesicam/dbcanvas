package main

// ftdcapi.go — getting diagnostic.data into the FTDC Summary.
//
// Two ways in, and the second is the one that matters. Uploading files works and is what
// somebody does with a directory a customer sent them; reading the directory straight off a
// running node is what makes the page usable during an incident, because diagnostic.data is
// already there — nobody had to have turned anything on, and nobody has to fetch it by
// hand while the thing is on fire.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// ftdcMaxUpload bounds an upload. A day of FTDC from one member is a few megabytes; this
// allows a week from several, and refuses somebody's backup.
const ftdcMaxUpload = 256 << 20

// ftdcDBPath is where mongod keeps diagnostic.data in every stack this app builds.
const ftdcDiagDir = "/var/lib/mongo/diagnostic.data"

// handleFTDCTargets lists the MongoDB nodes whose diagnostic.data can be read.
//
// It reuses the Packet Inspector's target walk and keeps the MongoDB ones: every mongod
// has a diagnostic.data directory, and every node type this app deploys that speaks
// MongoDB is a mongod except mongos, which has no storage engine and no replica-set status
// and is filtered out at read time by the directory simply not being there.
func (a *App) handleFTDCTargets(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	out := []qrTarget{}
	for _, t := range a.listPktTargets(u) {
		if t.Engine == pktEngineMongoDB {
			out = append(out, t)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleFTDCNode reads a running node's diagnostic.data directory and parses it.
//
// The directory is tarred and gzipped inside the container and pulled out base64-encoded,
// which is how everything else in this app moves bytes out of a container. It is not
// elegant and it does not need to be: a day of FTDC is a few megabytes, and the alternative
// is a second transport that has to be maintained.
func (a *App) handleFTDCNode(w http.ResponseWriter, r *http.Request) {
	dep, _, ok := a.loadRunningDBNode(w, r, pktEngineMongoDB)
	if !ok {
		return
	}
	ctx := r.Context()
	res, err := a.engCtx(ctx).Exec(ctx, dep.ContainerID,
		[]string{"bash", "-c", "tar czf - -C " + ftdcDiagDir + " . 2>/dev/null | base64 -w0"}, nil)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "read diagnostic.data: "+err.Error())
		return
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(res.Stdout))
	if err != nil || len(raw) == 0 {
		writeErr(w, http.StatusNotFound, "no diagnostic.data on this node — it exists only on mongod, not mongos")
		return
	}
	files, err := ftdcFromTarGz(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	a.ftdcRespond(w, files)
}

// handleFTDCUpload parses uploaded files: a whole diagnostic.data directory picked at once,
// or a .tar.gz of one.
func (a *App) handleFTDCUpload(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid upload")
		return
	}
	var named []ftdcNamed
	for _, fhs := range r.MultipartForm.File {
		for _, fh := range fhs {
			if fh.Size > ftdcMaxUpload {
				writeErr(w, http.StatusRequestEntityTooLarge, "file too large: "+fh.Filename)
				return
			}
			f, err := fh.Open()
			if err != nil {
				continue
			}
			data, err := io.ReadAll(io.LimitReader(f, ftdcMaxUpload))
			f.Close()
			if err != nil || len(data) == 0 {
				continue
			}
			// A .tar.gz of the directory is unpacked; anything else is taken as a raw
			// metrics file. Sniffing the gzip magic rather than trusting the name, because
			// people rename things.
			if len(data) > 2 && data[0] == 0x1f && data[1] == 0x8b {
				inner, err := ftdcFromTarGz(data)
				if err != nil {
					writeErr(w, http.StatusBadRequest, err.Error())
					return
				}
				named = append(named, inner...)
				continue
			}
			named = append(named, ftdcNamed{Name: fh.Filename, Data: data})
		}
	}
	if len(named) == 0 {
		writeErr(w, http.StatusBadRequest, "no files in the upload")
		return
	}
	a.ftdcRespond(w, named)
}

// ftdcNamed is one metrics file and the name it arrived under.
type ftdcNamed struct {
	Name string
	Data []byte
}

// ftdcRespond orders the files, parses them and writes the model.
func (a *App) ftdcRespond(w http.ResponseWriter, files []ftdcNamed) {
	// mongod names each file after its first sample, so name order IS time order — which
	// is what lets several files concatenate into one continuous series. metrics.interim
	// is the file currently being written and belongs last whatever its name sorts to.
	sort.Slice(files, func(i, j int) bool {
		ai := strings.Contains(files[i].Name, "interim")
		aj := strings.Contains(files[j].Name, "interim")
		if ai != aj {
			return aj
		}
		return files[i].Name < files[j].Name
	})
	var raw [][]byte
	for _, f := range files {
		raw = append(raw, f.Data)
	}
	d, err := ftdcParse(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ftdcSummarise(d))
}

// ftdcFromTarGz unpacks a gzipped tar into its regular files.
func ftdcFromTarGz(raw []byte) ([]ftdcNamed, error) {
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var out []ftdcNamed
	total := 0
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
		name := h.Name
		if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
			name = name[i+1:]
		}
		// Only the metrics files. A diagnostic.data directory holds nothing else, but an
		// archive somebody made by hand might hold the whole dbPath, and a 40 GB
		// collection file is not something to read into memory by accident.
		if !strings.HasPrefix(name, "metrics.") {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, ftdcMaxUpload))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		total += len(data)
		if total > ftdcMaxUpload {
			return nil, fmt.Errorf("archive holds more than %d MiB of metrics files", ftdcMaxUpload>>20)
		}
		out = append(out, ftdcNamed{Name: name, Data: data})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no metrics.* files in the archive — a diagnostic.data directory holds files named metrics.<timestamp>")
	}
	return out, nil
}
