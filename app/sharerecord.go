package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// sharerecord.go — the host recording a shared session.
//
// What is recorded is what the host sees: their own tab, captured in their browser
// (getDisplayMedia, then MediaRecorder; web/src/session/recorder.js). That one picture
// already holds everything the session is — the workspace or the driver's mirrored
// screen, the drawings on top of it, and the host's session panel with the chat — so
// nothing is composed on the server, and a guest's screen is never captured.
//
// The browser sends the video as it is made, a chunk every few seconds, and the server
// appends each to one file: a recording survives a host whose tab crashes, up to the
// last chunk that arrived. A chunk carries its sequence number, so a retried upload is
// taken once and a lost one is noticed. When the host stops — or the session ends, or
// the chunks stop coming — the file is finished: MediaRecorder writes a WebM with no
// duration, which players show as a video that cannot be seeked, so the duration is
// written into its header (webmSetDuration).
//
// A recording is the host's. It outlives the session (and the session's records, past
// their own retention), is listed with the host's sessions, and is deleted on its
// purge date — the instance's default number of days after it starts, which the host
// can move — or sooner, when the host purges it. Everyone in the session is told when
// recording starts and stops, and sees that it is on.

const (
	settingRecordingRetentionDays = "recordingRetentionDays"
	defaultRecordingRetentionDays = 30

	shareRecChunkMax = 64 << 20 // one chunk; a few seconds of video is well under 1 MB
	shareRecMax      = 8 << 30  // one recording: a two-hour session at the browser's bitrate is ~2 GB
	// shareRecStale is how long a recording waits for its next chunk before it is
	// finished without the host — a closed tab, a lost network.
	shareRecStale = 3 * time.Minute

	recRecording  = "recording"
	recProcessing = "processing"
	recReady      = "ready"
)

// ShareRecording is one screen recording of a session.
type ShareRecording struct {
	ID          int64   `json:"id"`
	SessionID   int64   `json:"sessionId"`
	HostID      int64   `json:"hostId"`
	Title       string  `json:"title"`
	Mime        string  `json:"mime"`
	File        string  `json:"-"`
	Size        int64   `json:"size"`
	Chunks      int64   `json:"chunks"`
	DurationMs  int64   `json:"durationMs"`
	State       string  `json:"state"`
	StartedAt   string  `json:"startedAt"`
	EndedAt     *string `json:"endedAt,omitempty"`
	LastChunkAt *string `json:"lastChunkAt,omitempty"`
	PurgeAt     string  `json:"purgeAt"`
}

// recordingsDir is where the videos are kept: beside the database, which is the
// volume an installation already persists (DB_PATH). A var so a test can move it.
var recordingsDir = func() string {
	return filepath.Join(filepath.Dir(envOr("DB_PATH", "dbcanvas.db")), "recordings")
}

func (rec ShareRecording) path() string { return filepath.Join(recordingsDir(), rec.File) }

// recordingRetentionDays is how long a new recording is kept by default, 1 to 3650.
func (a *App) recordingRetentionDays() int {
	v, err := a.store.AppSetting(settingRecordingRetentionDays)
	if err != nil || strings.TrimSpace(v) == "" {
		return defaultRecordingRetentionDays
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return defaultRecordingRetentionDays
	}
	return clampRecordingDays(n)
}

func clampRecordingDays(n int) int {
	if n <= 0 {
		return defaultRecordingRetentionDays
	}
	if n > maxShareRetentionDays {
		return maxShareRetentionDays
	}
	return n
}

// recLocks serializes the writes to one recording's file: chunks append in order.
var recLocks sync.Map // id -> *sync.Mutex

func recLock(id int64) *sync.Mutex {
	l, _ := recLocks.LoadOrStore(id, &sync.Mutex{})
	return l.(*sync.Mutex)
}

// recordingExt is the file extension for what the browser recorded.
func recordingExt(mime string) string {
	if strings.HasPrefix(mime, "video/mp4") {
		return ".mp4"
	}
	return ".webm"
}

// ------------------------------------------------------------- handlers

// handleShareRecordStart is the host starting to record: {mime}. A recording already
// running in this session — from a tab that was reloaded, say — is finished first.
func (a *App) handleShareRecordStart(w http.ResponseWriter, r *http.Request) {
	sess, u, ok := a.loadHostedSession(w, r)
	if !ok {
		return
	}
	if !sess.live(time.Now()) {
		writeErr(w, http.StatusConflict, "this session has ended")
		return
	}
	var in struct {
		Mime string `json:"mime"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	mime := strings.ToLower(strings.TrimSpace(in.Mime))
	if !strings.HasPrefix(mime, "video/webm") && !strings.HasPrefix(mime, "video/mp4") || len(mime) > 100 {
		writeErr(w, http.StatusBadRequest, "a recording is video/webm or video/mp4")
		return
	}
	if err := os.MkdirAll(recordingsDir(), 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, "the recordings directory is not writable")
		return
	}
	h := a.hubFor(sess)
	h.mu.Lock()
	prev := h.recording
	h.recording = nil
	h.mu.Unlock()
	if prev != nil {
		if cur, err := a.store.GetShareRecording(prev.ID); err == nil && cur.State == recRecording {
			a.finishRecording(cur, 0)
		}
	}

	raw := make([]byte, 8)
	rand.Read(raw)
	now := time.Now().UTC()
	title := "Shared session"
	if sess.StackName != "" {
		title += " from " + sess.StackName
	}
	title += " — " + now.Format("2006-01-02 15:04 UTC")
	rec := ShareRecording{
		SessionID: sess.ID, HostID: u.ID, Title: title, Mime: mime,
		File:    fmt.Sprintf("session-%d-%s-%s%s", sess.ID, now.Format("20060102-150405"), hex.EncodeToString(raw), recordingExt(mime)),
		State:   recRecording,
		PurgeAt: now.Add(time.Duration(a.recordingRetentionDays()) * 24 * time.Hour).Format(time.RFC3339),
	}
	f, err := os.OpenFile(rec.path(), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to create the recording")
		return
	}
	f.Close()
	if rec, err = a.store.CreateShareRecording(rec); err != nil {
		os.Remove(filepath.Join(recordingsDir(), rec.File))
		writeErr(w, http.StatusInternalServerError, "failed to create the recording")
		return
	}
	h.mu.Lock()
	h.recording = &rec
	h.mu.Unlock()
	h.event("record", 0, "", h.sess.HostName+" started recording the session — the recording shows the screen, the drawings and the chat")
	h.broadcastPresence()
	writeJSON(w, http.StatusOK, rec)
}

// loadOwnRecording resolves {rid} to a recording the signed-in user made.
func (a *App) loadOwnRecording(w http.ResponseWriter, r *http.Request) (ShareRecording, bool) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return ShareRecording{}, false
	}
	rid, err := strconv.ParseInt(r.PathValue("rid"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid recording id")
		return ShareRecording{}, false
	}
	rec, err := a.store.GetShareRecording(rid)
	if err != nil || rec.HostID != u.ID {
		writeErr(w, http.StatusNotFound, "recording not found")
		return ShareRecording{}, false
	}
	return rec, true
}

// handleShareRecordChunk appends one chunk: POST …/chunks?seq=N with the bytes as the
// body. N counts from 0; a chunk already taken is acknowledged again and dropped, one
// from the future is refused with the sequence the server expects.
func (a *App) handleShareRecordChunk(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.loadOwnRecording(w, r)
	if !ok {
		return
	}
	seq, err := strconv.ParseInt(r.URL.Query().Get("seq"), 10, 64)
	if err != nil || seq < 0 {
		writeErr(w, http.StatusBadRequest, "seq is the chunk's number, from 0")
		return
	}
	l := recLock(rec.ID)
	l.Lock()
	defer l.Unlock()
	rec, err = a.store.GetShareRecording(rec.ID) // fresh, under the lock
	if err != nil {
		writeErr(w, http.StatusNotFound, "recording not found")
		return
	}
	if rec.State != recRecording {
		writeErr(w, http.StatusConflict, "this recording has finished")
		return
	}
	if seq < rec.Chunks {
		writeJSON(w, http.StatusOK, map[string]any{"next": rec.Chunks, "size": rec.Size})
		return
	}
	if seq > rec.Chunks {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "a chunk is missing", "next": rec.Chunks})
		return
	}
	f, err := os.OpenFile(rec.path(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "the recording's file is gone")
		return
	}
	n, err := io.Copy(f, io.LimitReader(r.Body, shareRecChunkMax+1))
	if err == nil && n > shareRecChunkMax {
		err = errors.New("too large")
	}
	if err != nil {
		// Cut the file back to the chunks that were taken whole, so a retry appends
		// to a clean end.
		f.Truncate(rec.Size)
		f.Close()
		writeErr(w, http.StatusBadRequest, "the chunk did not arrive whole")
		return
	}
	f.Close()
	size := rec.Size + n
	if err := a.store.AddShareRecordingChunk(rec.ID, rec.Chunks+1, size); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to record the chunk")
		return
	}
	if size >= shareRecMax {
		rec.Size, rec.Chunks = size, rec.Chunks+1
		a.finishRecording(rec, 0)
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "the recording reached its 8 GB limit and was stopped", "next": rec.Chunks})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"next": rec.Chunks + 1, "size": size})
}

// handleShareRecordFinish is the host stopping: {durationMs}, how long the browser
// recorded for.
func (a *App) handleShareRecordFinish(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.loadOwnRecording(w, r)
	if !ok {
		return
	}
	var in struct {
		DurationMs int64 `json:"durationMs"`
	}
	decode(r, &in)
	l := recLock(rec.ID)
	l.Lock()
	rec, err := a.store.GetShareRecording(rec.ID)
	l.Unlock()
	if err != nil {
		writeErr(w, http.StatusNotFound, "recording not found")
		return
	}
	if rec.State == recRecording {
		a.finishRecording(rec, in.DurationMs)
	}
	rec, _ = a.store.GetShareRecording(rec.ID)
	writeJSON(w, http.StatusOK, rec)
}

// finishRecording closes a recording: the session is told, and the file gets its
// duration in the background — a rewrite of a large file takes a while. durationMs
// 0 means unknown, and is taken from the clock: first to last chunk.
func (a *App) finishRecording(rec ShareRecording, durationMs int64) {
	if err := a.store.SetShareRecordingState(rec.ID, recProcessing); err != nil {
		return
	}
	if h := shareHubs.get(rec.SessionID); h != nil {
		h.mu.Lock()
		was := h.recording != nil && h.recording.ID == rec.ID
		if was {
			h.recording = nil
		}
		h.mu.Unlock()
		if was {
			h.event("record", 0, "", h.sess.HostName+" stopped recording")
			h.broadcastPresence()
		}
	}
	started, _ := time.Parse(time.RFC3339, rec.StartedAt)
	last := time.Now()
	if rec.LastChunkAt != nil {
		if t, err := time.Parse(time.RFC3339, *rec.LastChunkAt); err == nil {
			last = t
		}
	}
	// What the browser says, unless it is plainly wrong: no longer than the time the
	// chunks took to arrive, give or take one chunk.
	clock := last.Sub(started).Milliseconds() + 5000
	if durationMs <= 0 || durationMs > clock {
		durationMs = clock - 5000
		if durationMs < 0 {
			durationMs = 0
		}
	}
	go func() {
		l := recLock(rec.ID)
		l.Lock()
		defer l.Unlock()
		cur, err := a.store.GetShareRecording(rec.ID)
		if err != nil {
			return
		}
		if cur.Chunks == 0 {
			// Nothing was ever recorded: nothing to keep.
			os.Remove(cur.path())
			a.store.DeleteShareRecording(cur.ID)
			return
		}
		if recordingExt(cur.Mime) == ".webm" && durationMs > 0 {
			if err := webmSetDuration(cur.path(), float64(durationMs)); err != nil && !errors.Is(err, errWebMLayout) {
				log.Printf("recording %d: could not write its duration: %v", cur.ID, err)
			}
		}
		size := cur.Size
		if st, err := os.Stat(cur.path()); err == nil {
			size = st.Size()
		}
		a.store.FinishShareRecording(cur.ID, durationMs, size)
		a.keepRecordingTranscript(cur)
	}()
}

// keepRecordingTranscript saves the session's transcript, as it is now, with a
// recording. Taken when the recording finishes and again just before the session's
// records are purged, so the copy is the whole session once it matters.
func (a *App) keepRecordingTranscript(rec ShareRecording) {
	sess, err := a.store.GetShareSession(rec.SessionID)
	if err != nil {
		return
	}
	b, err := json.Marshal(a.shareTranscriptOf(sess))
	if err != nil {
		return
	}
	if err := a.store.SetShareRecordingTranscript(rec.ID, string(b)); err != nil {
		log.Printf("recording %d: could not keep the transcript: %v", rec.ID, err)
	}
}

// handleShareRecordTranscript downloads the chat that goes with a recording: the
// session's transcript while its records are kept, then the copy the recording kept.
// Text, or ?format=json.
func (a *App) handleShareRecordTranscript(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.loadOwnRecording(w, r)
	if !ok {
		return
	}
	name := fmt.Sprintf("dbcanvas-session-%d-transcript", rec.SessionID)
	if sess, err := a.store.GetShareSession(rec.SessionID); err == nil && sess.HostID == rec.HostID {
		a.shareTranscriptOf(sess).write(w, r, name)
		return
	}
	raw, err := a.store.ShareRecordingTranscript(rec.ID)
	var t shareTranscript
	if err != nil || raw == "" || json.Unmarshal([]byte(raw), &t) != nil {
		writeErr(w, http.StatusNotFound, "no transcript was kept with this recording")
		return
	}
	if t.Session.ID != 0 {
		name = fmt.Sprintf("dbcanvas-session-%d-transcript", t.Session.ID)
	}
	t.write(w, r, name)
}

func (a *App) handleListShareRecordings(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	list, err := a.store.ListShareRecordings(u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list recordings")
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleShareRecordFile streams a finished recording: an attachment, or ?inline=1 to
// play it in the browser. Ranges are served, so a player can seek.
func (a *App) handleShareRecordFile(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.loadOwnRecording(w, r)
	if !ok {
		return
	}
	if rec.State != recReady {
		writeErr(w, http.StatusConflict, "this recording is not finished yet")
		return
	}
	f, err := os.Open(rec.path())
	if err != nil {
		writeErr(w, http.StatusNotFound, "the recording's file is gone")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to read the recording")
		return
	}
	started, _ := time.Parse(time.RFC3339, rec.StartedAt)
	name := fmt.Sprintf("dbcanvas-session-%d-%s%s", rec.SessionID, started.UTC().Format("20060102-1504"), recordingExt(rec.Mime))
	disp := "attachment"
	if r.URL.Query().Get("inline") == "1" {
		disp = "inline"
	}
	ct := "video/webm"
	if recordingExt(rec.Mime) == ".mp4" {
		ct = "video/mp4"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", disp+`; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// handleShareRecordUpdate renames a recording or moves its purge date:
// {title, purgeAt}. purgeAt is a date (2006-01-02, purged at the start of that day
// UTC) or a timestamp, in the future and at most ten years away.
func (a *App) handleShareRecordUpdate(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.loadOwnRecording(w, r)
	if !ok {
		return
	}
	var in struct {
		Title   *string `json:"title"`
		PurgeAt *string `json:"purgeAt"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if in.Title != nil {
		t := strings.TrimSpace(*in.Title)
		if t == "" || len([]rune(t)) > 200 {
			writeErr(w, http.StatusBadRequest, "a title is 1 to 200 characters")
			return
		}
		if err := a.store.SetShareRecordingTitle(rec.ID, t); err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to save")
			return
		}
	}
	if in.PurgeAt != nil {
		at, err := parsePurgeAt(*in.PurgeAt)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := a.store.SetShareRecordingPurge(rec.ID, at); err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to save")
			return
		}
	}
	rec, _ = a.store.GetShareRecording(rec.ID)
	writeJSON(w, http.StatusOK, rec)
}

func parsePurgeAt(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	at, err := time.Parse(time.RFC3339, v)
	if err != nil {
		if at, err = time.Parse("2006-01-02", v); err != nil {
			return time.Time{}, errors.New("purgeAt is a date, 2006-01-02, or an RFC 3339 timestamp")
		}
	}
	now := time.Now()
	if !at.After(now) {
		return time.Time{}, errors.New("the purge date must be in the future — to delete it now, purge it")
	}
	if at.After(now.Add(time.Duration(maxShareRetentionDays) * 24 * time.Hour)) {
		return time.Time{}, errors.New("the purge date is at most ten years away")
	}
	return at.UTC(), nil
}

// handleShareRecordDelete purges a recording now. One still being recorded is
// stopped for everyone first.
func (a *App) handleShareRecordDelete(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.loadOwnRecording(w, r)
	if !ok {
		return
	}
	if h := shareHubs.get(rec.SessionID); h != nil {
		h.mu.Lock()
		was := h.recording != nil && h.recording.ID == rec.ID
		if was {
			h.recording = nil
		}
		h.mu.Unlock()
		if was {
			h.event("record", 0, "", h.sess.HostName+" stopped recording and deleted it")
			h.broadcastPresence()
		}
	}
	a.purgeRecording(rec)
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

func (a *App) purgeRecording(rec ShareRecording) {
	l := recLock(rec.ID)
	l.Lock()
	defer l.Unlock()
	if err := os.Remove(rec.path()); err != nil && !os.IsNotExist(err) {
		log.Printf("recording %d: could not delete %s: %v", rec.ID, rec.File, err)
	}
	a.store.DeleteShareRecording(rec.ID)
	recLocks.Delete(rec.ID)
}

// ------------------------------------------------------------- upkeep

// recordingUpkeep runs with the share reaper: it finishes recordings whose chunks
// stopped coming, deletes the ones whose purge date came, and — once an hour —
// removes files no recording owns (an account deleted with its recordings, a crash
// between creating a file and its row).
func (a *App) recordingUpkeep(sweep bool) {
	now := time.Now()
	if stale, err := a.store.StaleShareRecordings(now.Add(-shareRecStale)); err == nil {
		for _, rec := range stale {
			a.finishRecording(rec, 0)
		}
	}
	// A restart in the middle of writing a duration leaves the row half-way; only the
	// first pass looks, since after it every "processing" row is this process's own.
	recUpkeepFirst.Do(func() {
		if stuck, err := a.store.ShareRecordingsInState(recProcessing); err == nil {
			for _, rec := range stuck {
				a.store.FinishShareRecording(rec.ID, rec.DurationMs, rec.Size)
			}
		}
	})
	if due, err := a.store.DueShareRecordings(now); err == nil {
		for _, rec := range due {
			a.purgeRecording(rec)
			log.Printf("recording %d: deleted on its purge date", rec.ID)
		}
	}
	if !sweep {
		return
	}
	known, err := a.store.ShareRecordingFiles()
	if err != nil {
		return
	}
	ents, err := os.ReadDir(recordingsDir())
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() || known[e.Name()] {
			continue
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > time.Hour {
			os.Remove(filepath.Join(recordingsDir(), e.Name()))
		}
	}
}

var recUpkeepFirst sync.Once

// ------------------------------------------------------------- WebM duration

// errWebMLayout is a file this does not know how to patch safely; it is left as is.
var errWebMLayout = errors.New("webm: unexpected layout")

const (
	ebmlHeaderID    = 0x1A45DFA3
	ebmlSegmentID   = 0x18538067
	ebmlSeekHeadID  = 0x114D9B74
	ebmlInfoID      = 0x1549A966
	ebmlTimescaleID = 0x2AD7B1
	ebmlDurationID  = 0x4489
	ebmlClusterID   = 0x1F43B675
	ebmlTracksID    = 0x1654AE6B
)

// ebmlVint reads a variable-length integer at b[0]: its value with the length
// marker removed (keepMarker for element ids, which keep it), its length, and whether
// it is the all-ones "unknown size".
func ebmlVint(b []byte, keepMarker bool) (v uint64, n int, unknown bool, ok bool) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false, false
	}
	for n = 1; n <= 8 && b[0]&(0x80>>(n-1)) == 0; n++ {
	}
	if n > 8 || len(b) < n {
		return 0, 0, false, false
	}
	first := uint64(b[0])
	if !keepMarker {
		first &^= 0x80 >> (n - 1)
	}
	v = first
	allOnes := first == uint64(0xFF>>n)
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(b[i])
		allOnes = allOnes && b[i] == 0xFF
	}
	return v, n, allOnes && !keepMarker, true
}

// ebmlSize encodes v as a size of exactly n bytes, or reports it does not fit.
func ebmlSize(v uint64, n int) ([]byte, bool) {
	if n < 1 || n > 8 || v >= (uint64(1)<<(7*n))-1 {
		return nil, false
	}
	out := make([]byte, n)
	for i := n - 1; i >= 0; i-- {
		out[i] = byte(v)
		v >>= 8
	}
	out[0] |= 0x80 >> (n - 1)
	return out, true
}

// webmSetDuration writes a duration (in milliseconds) into a WebM's Segment Info:
// in place when the file already has one, otherwise by inserting it, which moves
// every byte after it by eleven. That is only safe in a file with no byte offsets in
// it — no SeekHead, so no Cues to point at — which is what MediaRecorder writes; any
// other layout is left untouched (errWebMLayout).
func webmSetDuration(path string, ms float64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	head := make([]byte, 1<<20)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		f.Close()
		return err
	}
	head = head[:n]
	f.Close()

	elem := func(at int) (id uint64, size uint64, dataAt int, unknown bool, ok bool) {
		id, idLen, _, ok1 := ebmlVint(head[at:], true)
		if !ok1 {
			return
		}
		size, szLen, unknown, ok2 := ebmlVint(head[at+idLen:], false)
		if !ok2 {
			return
		}
		return id, size, at + idLen + szLen, unknown, true
	}
	id, size, at, unknown, ok := elem(0)
	if !ok || id != ebmlHeaderID || unknown {
		return errWebMLayout
	}
	segAt := at + int(size)
	if segAt >= len(head) {
		return errWebMLayout
	}
	id, segSize, segData, segUnknown, ok := elem(segAt)
	if !ok || id != ebmlSegmentID {
		return errWebMLayout
	}
	segSizeAt := segAt + 4 // the Segment id is four bytes
	segSizeLen := segData - segSizeAt

	for pos := segData; pos < len(head); {
		id, size, data, unknown, ok := elem(pos)
		if !ok || unknown {
			return errWebMLayout
		}
		switch id {
		case ebmlSeekHeadID, ebmlClusterID, ebmlTracksID:
			return errWebMLayout // offsets to keep, or no Info before the media
		case ebmlInfoID:
			end := data + int(size)
			if end > len(head) {
				return errWebMLayout
			}
			scale := 1e6
			durAt, durLen := -1, 0
			for p := data; p < end; {
				cid, csize, cdata, cunk, ok := elem(p)
				if !ok || cunk || cdata+int(csize) > end {
					return errWebMLayout
				}
				switch cid {
				case ebmlTimescaleID:
					var v uint64
					for _, c := range head[cdata : cdata+int(csize)] {
						v = v<<8 | uint64(c)
					}
					if v > 0 {
						scale = float64(v)
					}
				case ebmlDurationID:
					durAt, durLen = cdata, int(csize)
				}
				p = cdata + int(csize)
			}
			dur := ms * 1e6 / scale
			if durAt >= 0 {
				// Already there: overwrite it where it is.
				var buf []byte
				switch durLen {
				case 8:
					buf = binary.BigEndian.AppendUint64(nil, math.Float64bits(dur))
				case 4:
					buf = binary.BigEndian.AppendUint32(nil, math.Float32bits(float32(dur)))
				default:
					return errWebMLayout
				}
				wf, err := os.OpenFile(path, os.O_WRONLY, 0)
				if err != nil {
					return err
				}
				defer wf.Close()
				_, err = wf.WriteAt(buf, int64(durAt))
				return err
			}
			durElem := append([]byte{0x44, 0x89, 0x88}, binary.BigEndian.AppendUint64(nil, math.Float64bits(dur))...)
			_, infoIDLen, _, _ := ebmlVint(head[pos:], true)
			infoSizeLen := data - pos - infoIDLen
			newInfoSize, fits := ebmlSize(size+uint64(len(durElem)), infoSizeLen)
			if !fits {
				return errWebMLayout
			}
			var prefix bytes.Buffer
			prefix.Write(head[:segSizeAt])
			if segUnknown {
				prefix.Write(head[segSizeAt:segData])
			} else {
				b, fits := ebmlSize(segSize+uint64(len(durElem)), segSizeLen)
				if !fits {
					return errWebMLayout
				}
				prefix.Write(b)
			}
			prefix.Write(head[segData : pos+infoIDLen])
			prefix.Write(newInfoSize)
			prefix.Write(head[data:end])
			prefix.Write(durElem)
			return rewriteFrom(path, prefix.Bytes(), int64(end))
		}
		pos = data + int(size)
	}
	return errWebMLayout
}

// rewriteFrom replaces path with prefix followed by the old file from offset on,
// through a temporary file and a rename, so a crash leaves one or the other whole.
func rewriteFrom(path string, prefix []byte, offset int64) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".rewrite-*")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(prefix); err != nil {
		return err
	}
	if _, err := src.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.Copy(tmp, src); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	ok = true
	return nil
}
