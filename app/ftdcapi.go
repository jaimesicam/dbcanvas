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
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ftdcMaxUpload bounds an upload. A day of FTDC from one member is a few megabytes; this
// allows a week from several, and refuses somebody's backup.
const ftdcMaxUpload = 256 << 20

// ftdcDiagDirs are the directories a MongoDB process writes FTDC into, most likely first.
//
// A mongod puts diagnostic.data inside its dbPath. A mongos has no dbPath at all, so it
// derives the directory from its LOG path instead: the extension is stripped and
// ".diagnostic.data" appended, which makes /var/log/mongo/mongos.log into
// /var/log/mongo/mongos.diagnostic.data.
//
// This used to be one constant, the mongod one, and the failure was reported to the user as
// "it exists only on mongod, not mongos" — which is simply not true. mongos captures FTDC
// like everything else; it was being looked for in the one place it could never be.
var ftdcDiagDirs = []string{
	"/var/lib/mongo/diagnostic.data",          // mongod, as this app provisions it
	"/var/log/mongo/mongos.diagnostic.data",   // mongos, derived from its log path
	"/var/lib/mongodb/diagnostic.data",        // the upstream/Debian mongod layout
	"/var/log/mongodb/mongos.diagnostic.data", //
	"/data/db/diagnostic.data",                // a common container layout
}

// handleFTDCTargets lists the MongoDB nodes whose diagnostic.data can be read.
//
// It reuses the Packet Inspector's target walk and keeps the MongoDB ones. Every MongoDB
// process captures FTDC, mongos included — see ftdcDiagDirs for where each one puts it.
// A sharded cluster's targets carry their role in the label, because which member a capture
// comes from decides what is in it: a mongos has no storage engine and no replica-set
// status, and a config server's replica set is the cluster's metadata rather than its data.
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
	files, err := a.ftdcReadNode(r.Context(), dep.ContainerID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	a.ftdcRespond(w, r, files)
}

// ftdcReadNode pulls one running node's diagnostic.data out of its container.
//
// One exec that tars whichever of the candidate directories exists, rather than one round
// trip per candidate: the directory that is there wins and the rest cost nothing.
func (a *App) ftdcReadNode(ctx context.Context, containerID string) ([]ftdcNamed, error) {
	script := "for d in " + strings.Join(ftdcDiagDirs, " ") +
		"; do if [ -d \"$d\" ]; then tar czf - -C \"$d\" . 2>/dev/null | base64 -w0; exit 0; fi; done; exit 3"
	res, err := a.engCtx(ctx).Exec(ctx, containerID, []string{"bash", "-c", script}, nil)
	if err != nil {
		return nil, fmt.Errorf("read diagnostic.data: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(res.Stdout))
	if err != nil || len(raw) == 0 {
		return nil, fmt.Errorf("no diagnostic.data found on this node — looked in %s", strings.Join(ftdcDiagDirs, ", "))
	}
	return ftdcFromTarGz(raw)
}

// handleFTDCCompare reads several members' captures in one request.
//
// Lag, elections, sync source and quorum are questions about a SET, and every chart on this
// page until now answered them from one member's file — which is the member's own opinion of
// the set. Three files answer them directly: the same second, read from three machines, is
// the difference between "this member thought it was behind" and "it was".
//
// Each capture is summarised independently and the page overlays them, rather than the
// server merging them: the merge needs the chart definitions, and duplicating those here
// would be a second place for them to drift.
func (a *App) handleFTDCCompare(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body struct {
		Targets []struct {
			StackID int64  `json:"stackId"`
			NodeID  string `json:"nodeId"`
		} `json:"targets"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(body.Targets) < 2 {
		writeErr(w, http.StatusBadRequest, "pick at least two members to compare")
		return
	}
	if len(body.Targets) > ftdcMaxCompare {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("at most %d members at a time — beyond that the overlay stops being readable", ftdcMaxCompare))
		return
	}
	from, to, windowed := ftdcRange(r)

	type member struct {
		Label   string   `json:"label"`
		StackID int64    `json:"stackId"`
		NodeID  string   `json:"nodeId"`
		Model   *fdModel `json:"model,omitempty"`
		Error   string   `json:"error,omitempty"`
	}
	out := make([]member, 0, len(body.Targets))
	for _, t := range body.Targets {
		m := member{StackID: t.StackID, NodeID: t.NodeID, Label: t.NodeID}
		dep, label, err := a.ftdcTarget(u, t.StackID, t.NodeID)
		if err != nil {
			m.Error = err.Error()
			out = append(out, m)
			continue
		}
		m.Label = label
		files, err := a.ftdcReadNode(r.Context(), dep.ContainerID)
		if err != nil {
			m.Error = err.Error()
			out = append(out, m)
			continue
		}
		raw := make([][]byte, 0, len(files))
		ftdcSortFiles(files)
		for _, f := range files {
			raw = append(raw, f.Data)
		}
		d, err := ftdcParse(raw)
		if err != nil {
			m.Error = err.Error()
			out = append(out, m)
			continue
		}
		if windowed {
			d = ftdcWindow(d, from, to)
			if len(d.TS) == 0 {
				m.Error = "no samples in that window"
				out = append(out, m)
				continue
			}
		}
		m.Model = ftdcSummarise(d)
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": out})
}

// ftdcMaxCompare is how many members one comparison may hold. The categorical palette is
// eight colours and a chart wants a line per member per series; past four the overlay is a
// thicket whatever the colours do.
const ftdcMaxCompare = 4

// ftdcTarget resolves one node the way the single-node endpoint does, without the HTTP
// plumbing: same ownership check, same "is it running" check, same engine check.
func (a *App) ftdcTarget(u User, stackID int64, nodeID string) (Deployment, string, error) {
	engine, containerID, label, _, _, _, err := a.pktResolveTarget(u, stackID, nodeID)
	if err != nil {
		return Deployment{}, "", err
	}
	if engine != pktEngineMongoDB {
		return Deployment{}, "", fmt.Errorf("%s is not a MongoDB node", label)
	}
	return Deployment{ContainerID: containerID}, label, nil
}

// handleFTDCUpload parses uploaded files: a whole diagnostic.data directory picked at once,
// or a .tar.gz / .zip of one.
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
	// A whole diagnostic.data folder arrives as one file per metrics.<timestamp>, so the
	// per-file limit alone bounds nothing: the total is what has to hold.
	total := 0
	for _, fhs := range r.MultipartForm.File {
		for _, fh := range fhs {
			if fh.Size > ftdcMaxUpload {
				writeErr(w, http.StatusRequestEntityTooLarge, "file too large: "+fh.Filename)
				return
			}
			if total += int(fh.Size); total > ftdcMaxUpload {
				writeErr(w, http.StatusRequestEntityTooLarge,
					fmt.Sprintf("upload holds more than %d MiB", ftdcMaxUpload>>20))
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
			// An archive of the directory is unpacked; anything else is taken as a raw
			// metrics file. Sniffing the magic bytes rather than trusting the name, because
			// people rename things — a .tar.gz that came off a ticket often arrives as
			// "diagnostic.data.tar.gz.20260814" or with no extension at all.
			switch {
			case len(data) > 2 && data[0] == 0x1f && data[1] == 0x8b:
				inner, err := ftdcFromTarGz(data)
				if err != nil {
					writeErr(w, http.StatusBadRequest, err.Error())
					return
				}
				named = append(named, inner...)
				continue
			case len(data) > 4 && data[0] == 'P' && data[1] == 'K' && data[2] == 0x03 && data[3] == 0x04:
				inner, err := ftdcFromZip(data)
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
	a.ftdcRespond(w, r, named)
}

// ftdcSortFiles puts a capture's files in time order. mongod names each file after its
// first sample, so name order IS time order — which is what lets several files concatenate
// into one continuous series. metrics.interim is the file currently being written and
// belongs last whatever its name sorts to.
func ftdcSortFiles(files []ftdcNamed) {
	sort.Slice(files, func(i, j int) bool {
		ai := strings.Contains(files[i].Name, "interim")
		aj := strings.Contains(files[j].Name, "interim")
		if ai != aj {
			return aj
		}
		return files[i].Name < files[j].Name
	})
}

// ftdcNamed is one metrics file and the name it arrived under.
type ftdcNamed struct {
	Name string
	Data []byte
}

// ftdcRespond orders the files, parses them and writes the model.
func (a *App) ftdcRespond(w http.ResponseWriter, r *http.Request, files []ftdcNamed) {
	ftdcSortFiles(files)
	var raw [][]byte
	for _, f := range files {
		raw = append(raw, f.Data)
	}
	d, err := ftdcParse(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// A zoom is a second read of the same bytes, narrowed. Doing it this way rather than
	// zooming the drawn points is the difference between magnifying a thinned line and
	// seeing what was actually recorded: a capture is downsampled to fdMaxPoints for the
	// page, so a sixty-second event in an eight-hour file is two points until it is the
	// whole window.
	if from, to, ok := ftdcRange(r); ok {
		d = ftdcWindow(d, from, to)
		if len(d.TS) == 0 {
			writeErr(w, http.StatusBadRequest, "no samples in that window")
			return
		}
	}
	writeJSON(w, http.StatusOK, ftdcSummarise(d))
}

// ftdcRange reads the requested window off the query string. Both bounds are epoch seconds
// and both are optional — a zoom that only sets one end is a zoom to the start or the end of
// the capture, which is what dragging off the edge of a chart means.
func ftdcRange(r *http.Request) (from, to float64, ok bool) {
	q := r.URL.Query()
	parse := func(k string) (float64, bool) {
		v := strings.TrimSpace(q.Get(k))
		if v == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f <= 0 {
			return 0, false
		}
		return f, true
	}
	from, okF := parse("from")
	to, okT := parse("to")
	if !okF && !okT {
		return 0, 0, false
	}
	if !okT {
		to = math.MaxInt32 * 1.0
	}
	return from, to, true
}

// ftdcWindow returns the samples inside [from, to]. Every series is sliced by the same
// indices, which is what keeps a chart's points and the timestamp column aligned.
func ftdcWindow(d *ftdcData, from, to float64) *ftdcData {
	lo, hi := -1, -1
	for i, t := range d.TS {
		if t >= from && lo < 0 {
			lo = i
		}
		if t <= to {
			hi = i
		}
	}
	if lo < 0 || hi < lo {
		return &ftdcData{Series: map[string]*ftdcSeries{}, Meta: d.Meta}
	}
	out := &ftdcData{
		TS: d.TS[lo : hi+1], Series: make(map[string]*ftdcSeries, len(d.Series)),
		Meta: d.Meta, Chunks: d.Chunks, Samples: hi - lo + 1, Skipped: d.Skipped,
	}
	for k, s := range d.Series {
		if len(s.Values) <= hi {
			continue
		}
		out.Series[k] = &ftdcSeries{Key: s.Key, Values: s.Values[lo : hi+1]}
	}
	return out
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
		name, ok := ftdcArchiveName(h.Name)
		if !ok {
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

// ftdcArchiveName reduces an archive member's path to its base name and says whether it is
// a metrics file worth reading.
//
// Only the metrics files. A diagnostic.data directory holds nothing else, but an archive
// somebody made by hand might hold the whole dbPath, and a 40 GB collection file is not
// something to read into memory by accident. It also drops the "__MACOSX/._metrics.*"
// resource forks a zip made on a Mac carries, which are not metrics files however much
// their names look like it.
func ftdcArchiveName(path string) (string, bool) {
	name := path
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		name = name[i+1:]
	}
	return name, strings.HasPrefix(name, "metrics.")
}

// ftdcFromZip unpacks a zip into its metrics files — the same job as ftdcFromTarGz, for the
// archive people actually make on Windows and on a Mac's right-click "Compress".
//
// A zip is read from a []byte rather than streamed because the format's index lives at the
// END of the file: there is no way to walk one without having all of it, which the upload
// path already does.
func ftdcFromZip(raw []byte) ([]ftdcNamed, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("not a zip archive: %w", err)
	}
	var out []ftdcNamed
	total := 0
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		name, ok := ftdcArchiveName(zf.Name)
		if !ok {
			continue
		}
		f, err := zf.Open()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		data, err := io.ReadAll(io.LimitReader(f, ftdcMaxUpload))
		f.Close()
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
