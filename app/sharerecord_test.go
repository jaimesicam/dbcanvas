package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sharerecord_test.go — a recording is the host's, arrives in order, finishes with a
// duration a player can seek by, and goes on its purge date.

func recFixture(t *testing.T) *shareFixture {
	t.Helper()
	dir := t.TempDir()
	old := recordingsDir
	recordingsDir = func() string { return dir }
	t.Cleanup(func() { recordingsDir = old })
	return newShareFixture(t, RoleUser)
}

func (f *shareFixture) startRecording(t *testing.T) ShareRecording {
	t.Helper()
	w := httptest.NewRecorder()
	r := f.asHost(httptest.NewRequest("POST", "/", strings.NewReader(`{"mime":"video/webm;codecs=vp9"}`)))
	r.SetPathValue("sid", strconv.FormatInt(f.sess.ID, 10))
	f.app.handleShareRecordStart(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("start recording: %d %s", w.Code, w.Body)
	}
	var rec ShareRecording
	json.Unmarshal(w.Body.Bytes(), &rec)
	return rec
}

func (f *shareFixture) chunk(rid int64, seq int, body []byte) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := f.asHost(httptest.NewRequest("POST", "/?seq="+strconv.Itoa(seq), bytes.NewReader(body)))
	r.SetPathValue("rid", strconv.FormatInt(rid, 10))
	f.app.handleShareRecordChunk(w, r)
	return w
}

// waitReady waits for the background finish.
func (f *shareFixture) waitReady(t *testing.T, id int64) ShareRecording {
	t.Helper()
	for i := 0; i < 200; i++ {
		rec, err := f.app.store.GetShareRecording(id)
		if err == nil && rec.State == recReady {
			return rec
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the recording never finished")
	return ShareRecording{}
}

// fakeWebM is the shape MediaRecorder writes: an EBML header, a Segment of unknown
// size, Info with a timescale and no Duration, Tracks, then a Cluster.
func fakeWebM() []byte {
	el := func(id []byte, data []byte) []byte {
		size, _ := ebmlSize(uint64(len(data)), 1)
		if len(data) > 126 {
			size, _ = ebmlSize(uint64(len(data)), 8)
		}
		return append(append(append([]byte{}, id...), size...), data...)
	}
	header := el([]byte{0x1A, 0x45, 0xDF, 0xA3}, el([]byte{0x42, 0x82}, []byte("webm")))
	info := el([]byte{0x15, 0x49, 0xA9, 0x66}, append(
		el([]byte{0x2A, 0xD7, 0xB1}, []byte{0x0F, 0x42, 0x40}), // 1,000,000 ns
		el([]byte{0x4D, 0x80}, []byte("Chrome"))...))
	tracks := el([]byte{0x16, 0x54, 0xAE, 0x6B}, []byte{0xAE, 0x81, 0x00})
	cluster := el([]byte{0x1F, 0x43, 0xB6, 0x75}, bytes.Repeat([]byte{0xA3}, 40))
	seg := append([]byte{0x18, 0x53, 0x80, 0x67, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, info...)
	seg = append(append(seg, tracks...), cluster...)
	return append(header, seg...)
}

// webmDuration reads the Duration back out of Info, in milliseconds.
func webmDuration(t *testing.T, b []byte) float64 {
	t.Helper()
	i := bytes.Index(b, []byte{0x44, 0x89, 0x88})
	if i < 0 {
		t.Fatal("no Duration in the file")
	}
	return math.Float64frombits(binary.BigEndian.Uint64(b[i+3 : i+11]))
}

func TestWebMDurationIsWritten(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.webm")
	orig := fakeWebM()
	os.WriteFile(p, orig, 0o600)
	if err := webmSetDuration(p, 61500); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if d := webmDuration(t, got); d != 61500 {
		t.Errorf("duration %v, want 61500", d)
	}
	if len(got) != len(orig)+11 {
		t.Errorf("file grew by %d bytes, want 11", len(got)-len(orig))
	}
	// Everything after Info is untouched.
	tail := orig[bytes.Index(orig, []byte{0x16, 0x54, 0xAE, 0x6B}):]
	if !bytes.HasSuffix(got, tail) {
		t.Error("the media after Info changed")
	}
	// Info's size says it holds the new element.
	i := bytes.Index(got, []byte{0x15, 0x49, 0xA9, 0x66})
	if sz, _, _, _ := ebmlVint(got[i+4:], false); int(sz) != bytes.Index(got, []byte{0x16, 0x54, 0xAE, 0x6B})-(i+5) {
		t.Errorf("Info's size %d does not cover its children", sz)
	}
	// Run again: the Duration is now there, and is overwritten in place.
	if err := webmSetDuration(p, 1000); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(p)
	if len(again) != len(got) || webmDuration(t, again) != 1000 {
		t.Error("a second duration was not written in place")
	}
}

func TestWebMWithOffsetsIsLeftAlone(t *testing.T) {
	p := filepath.Join(t.TempDir(), "b.webm")
	b := fakeWebM()
	// A SeekHead before Info: byte offsets that an insert would break.
	at := bytes.Index(b, []byte{0x15, 0x49, 0xA9, 0x66})
	b = append(append(append([]byte{}, b[:at]...), 0x11, 0x4D, 0x9B, 0x74, 0x80), b[at:]...)
	os.WriteFile(p, b, 0o600)
	if err := webmSetDuration(p, 5000); !errors.Is(err, errWebMLayout) {
		t.Errorf("got %v, want errWebMLayout", err)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, b) {
		t.Error("a file with offsets was changed")
	}
	os.WriteFile(p, []byte("not a video"), 0o600)
	if err := webmSetDuration(p, 5000); !errors.Is(err, errWebMLayout) {
		t.Errorf("junk: got %v", err)
	}
}

func TestARecordingArrivesInOrder(t *testing.T) {
	f := recFixture(t)
	rec := f.startRecording(t)
	if p := f.app.hubFor(f.sess).presence(); p["recording"] == nil {
		t.Error("the session is not told it is being recorded")
	}
	video := fakeWebM()
	a, b := video[:30], video[30:]
	if w := f.chunk(rec.ID, 0, a); w.Code != http.StatusOK {
		t.Fatalf("chunk 0: %d %s", w.Code, w.Body)
	}
	if w := f.chunk(rec.ID, 0, a); w.Code != http.StatusOK { // a retry
		t.Fatalf("retried chunk 0: %d", w.Code)
	}
	if w := f.chunk(rec.ID, 2, b); w.Code != http.StatusConflict {
		t.Errorf("a chunk from the future: %d, want 409", w.Code)
	}
	if w := f.chunk(rec.ID, 1, b); w.Code != http.StatusOK {
		t.Fatalf("chunk 1: %d", w.Code)
	}

	w := httptest.NewRecorder()
	r := f.asHost(httptest.NewRequest("POST", "/", strings.NewReader(`{"durationMs":2500}`)))
	r.SetPathValue("rid", strconv.FormatInt(rec.ID, 10))
	f.app.handleShareRecordFinish(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("finish: %d %s", w.Code, w.Body)
	}
	if p := f.app.hubFor(f.sess).presence(); p["recording"] != nil {
		t.Error("the session still shows a recording after it stopped")
	}
	done := f.waitReady(t, rec.ID)
	got, _ := os.ReadFile(done.path())
	if len(got) != len(video)+11 || done.Size != int64(len(got)) {
		t.Errorf("file is %d bytes (row says %d), want the two chunks once plus the duration", len(got), done.Size)
	}
	if d := webmDuration(t, got); d <= 0 || d > 2500 {
		t.Errorf("duration %v: the browser's 2500 ms, capped by the clock", d)
	}
	if w := f.chunk(rec.ID, 2, b); w.Code != http.StatusConflict {
		t.Errorf("a chunk after the finish: %d, want 409", w.Code)
	}

	// The download is the host's, with a range a player can seek by.
	w = httptest.NewRecorder()
	r = f.asHost(httptest.NewRequest("GET", "/", nil))
	r.Header.Set("Range", "bytes=0-3")
	r.SetPathValue("rid", strconv.FormatInt(rec.ID, 10))
	f.app.handleShareRecordFile(w, r)
	if w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), got[:4]) {
		t.Errorf("ranged download: %d %x", w.Code, w.Body.Bytes())
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Error("the download is not an attachment")
	}
}

func TestSomeoneElsesRecordingIsNotFound(t *testing.T) {
	f := recFixture(t)
	rec := f.startRecording(t)
	other, _ := f.app.store.CreateUser("other", "x", RoleAdmin, StatusApproved)
	w := httptest.NewRecorder()
	r := withPrincipal(httptest.NewRequest("GET", "/", nil), principal{User: other})
	r.SetPathValue("rid", strconv.FormatInt(rec.ID, 10))
	f.app.handleShareRecordFile(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("an admin reached another host's recording: %d", w.Code)
	}
	// And a guest reaches none of the recording routes.
	c, g := f.join(t, "Jane")
	f.admit(t, g)
	for _, p := range []string{"GET /api/share/recordings", "GET /api/share/recordings/{rid}/file",
		"POST /api/share/sessions/{sid}/recordings", "DELETE /api/share/recordings/{rid}",
		"POST /api/share/sessions/{sid}/draw", "POST /api/share/sessions/{sid}/guests/{gid}/draw"} {
		if _, reached := f.call(t, p, c); reached {
			t.Errorf("%s was reached by a guest", p)
		}
	}
}

func TestRecordingsArePurgedOnTheirDate(t *testing.T) {
	f := recFixture(t)
	rec := f.startRecording(t)
	f.chunk(rec.ID, 0, fakeWebM())
	f.app.finishRecording(rec, 1000)
	rec = f.waitReady(t, rec.ID)

	// Moving the date: only forward, and not to the past.
	put := func(body string) int {
		w := httptest.NewRecorder()
		r := f.asHost(httptest.NewRequest("PUT", "/", strings.NewReader(body)))
		r.SetPathValue("rid", strconv.FormatInt(rec.ID, 10))
		f.app.handleShareRecordUpdate(w, r)
		return w.Code
	}
	if code := put(`{"purgeAt":"2001-01-01"}`); code != http.StatusBadRequest {
		t.Errorf("a purge date in the past: %d", code)
	}
	future := time.Now().AddDate(0, 2, 0).Format("2006-01-02")
	if code := put(`{"purgeAt":"` + future + `","title":"Customer call"}`); code != http.StatusOK {
		t.Fatalf("moving the date: %d", code)
	}
	if got, _ := f.app.store.GetShareRecording(rec.ID); !strings.HasPrefix(got.PurgeAt, future) || got.Title != "Customer call" {
		t.Errorf("not saved: %+v", got)
	}

	f.app.recordingUpkeep(false)
	if _, err := os.Stat(rec.path()); err != nil {
		t.Fatal("a recording was purged before its date")
	}
	f.app.store.db.Exec(`UPDATE share_recordings SET purge_at = ? WHERE id = ?`, time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), rec.ID)
	f.app.recordingUpkeep(false)
	if _, err := os.Stat(rec.path()); !os.IsNotExist(err) {
		t.Error("the file outlived its purge date")
	}
	if _, err := f.app.store.GetShareRecording(rec.ID); err == nil {
		t.Error("the row outlived its purge date")
	}
}

func TestAnAbandonedRecordingIsFinished(t *testing.T) {
	f := recFixture(t)
	rec := f.startRecording(t)
	f.chunk(rec.ID, 0, fakeWebM())
	old := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	f.app.store.db.Exec(`UPDATE share_recordings SET started_at = ?, last_chunk_at = ? WHERE id = ?`, old, old, rec.ID)
	f.app.recordingUpkeep(false)
	f.waitReady(t, rec.ID)

	// One that never got a chunk is not kept at all.
	empty := f.startRecording(t)
	f.app.store.db.Exec(`UPDATE share_recordings SET started_at = ? WHERE id = ?`, old, empty.ID)
	f.app.recordingUpkeep(false)
	for i := 0; i < 200; i++ {
		if _, err := f.app.store.GetShareRecording(empty.ID); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("an empty recording was kept")
}

func TestOrphanFilesAreSwept(t *testing.T) {
	f := recFixture(t)
	stray := filepath.Join(recordingsDir(), "session-9-x.webm")
	os.WriteFile(stray, []byte("x"), 0o600)
	os.Chtimes(stray, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour))
	rec := f.startRecording(t)
	os.Chtimes(rec.path(), time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour))
	f.app.recordingUpkeep(true)
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Error("a file no recording owns was kept")
	}
	if _, err := os.Stat(rec.path()); err != nil {
		t.Error("a recording's own file was swept")
	}
}

func TestARecordingKeepsItsTranscript(t *testing.T) {
	f := recFixture(t)
	h := f.app.hubFor(f.sess)
	host := &hubClient{guestID: 0, name: "host", out: make(chan []byte, 64), cancel: func() {}}
	h.handle(host, "chat", "the replica lag is on node 2", nil)
	rec := f.startRecording(t)
	f.chunk(rec.ID, 0, fakeWebM())
	f.app.finishRecording(rec, 1000)
	f.waitReady(t, rec.ID)

	get := func(q string) (int, string) {
		w := httptest.NewRecorder()
		r := f.asHost(httptest.NewRequest("GET", "/"+q, nil))
		r.SetPathValue("rid", strconv.FormatInt(rec.ID, 10))
		f.app.handleShareRecordTranscript(w, r)
		return w.Code, w.Body.String()
	}
	if code, body := get(""); code != http.StatusOK || !strings.Contains(body, "the replica lag is on node 2") {
		t.Fatalf("transcript with the recording: %d %s", code, body)
	}
	// The copy is sealed like the chat it copies.
	var raw string
	f.app.store.db.QueryRow(`SELECT transcript FROM share_recordings WHERE id = ?`, rec.ID).Scan(&raw)
	if raw == "" || strings.Contains(raw, "replica lag") {
		t.Errorf("the kept transcript is missing or in the clear: %q", raw)
	}

	// The session's records go with their retention; the recording's copy stays.
	f.app.endShare(f.sess, "ended")
	long := time.Now().AddDate(0, 0, -30).UTC().Format(time.RFC3339)
	f.app.store.db.Exec(`UPDATE share_sessions SET ended_at = ? WHERE id = ?`, long, f.sess.ID)
	f.app.store.SetAppSetting(settingShareRetentionDays, "7")
	if n := f.app.purgeShareSessions(); n != 1 {
		t.Fatalf("purged %d sessions, want 1", n)
	}
	if _, err := f.app.store.GetShareSession(f.sess.ID); err == nil {
		t.Fatal("the session outlived its retention")
	}
	code, body := get("")
	if code != http.StatusOK || !strings.Contains(body, "the replica lag is on node 2") || !strings.Contains(body, "The host ended the session") {
		t.Errorf("after the purge, the whole session's transcript: %d %s", code, body)
	}
	if code, body := get("?format=json"); code != http.StatusOK || !strings.Contains(body, `"messages"`) {
		t.Errorf("as JSON: %d %s", code, body)
	}
}
