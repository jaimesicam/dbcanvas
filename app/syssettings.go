package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// syssettings.go — instance-wide settings, as opposed to settings.go's per-user
// UI preferences.
//
// The distinction is who the setting belongs to. A theme is yours; the ceiling
// on how much a file drop may push into a container is the *server's*, because
// it bounds work the server does on everyone's behalf. So these are stored once
// (app_settings), readable by any signed-in user — the designer needs the number
// to refuse an over-size drop before uploading it, and everyone deserves to see
// the limit they are working under — and writable only by an admin.

// Keys in the app_settings table.
const (
	settingMaxUploadBytes = "maxUploadBytes"
	settingMaxTokenDays   = "maxTokenDays"
	// settingInternalWrites unlocks writes to the databases DBCanvas exposes
	// read-only — PMM Server's own PostgreSQL and ClickHouse. See the field below.
	settingInternalWrites = "internalWrites"
)

const (
	// defaultMaxUploadBytes is the ceiling on one file drop onto a node.
	defaultMaxUploadBytes int64 = 4 << 30 // 4 GiB
	// minMaxUploadBytes keeps an admin from setting a limit so low the feature
	// is effectively off but still looks configured.
	minMaxUploadBytes int64 = 1 << 20 // 1 MiB
	// maxMaxUploadBytes is the largest value accepted. The upload streams rather
	// than buffers (see nodeupload.go), so this is bounded by the temp space the
	// multipart parser needs, not by RAM.
	maxMaxUploadBytes int64 = 1 << 40 // 1 TiB
)

const (
	// defaultMaxTokenDays is the longest lifetime a non-admin may give an API
	// token. Ninety days is long enough that nobody re-authenticates weekly, short
	// enough that a token forgotten on an old laptop stops mattering.
	defaultMaxTokenDays = 90
	minMaxTokenDays     = 1
	// maxMaxTokenDays caps even an admin's ceiling. Past a year a lifetime is not
	// really a lifetime; an admin who wants that creates a never-expiring token
	// deliberately instead, which is a decision the UI makes them make.
	maxMaxTokenDays = 365
)

// SystemSettings is the instance-wide configuration served to the UI.
//
// It carries two kinds of value. MaxUploadBytes is stored and an admin can change
// it from the settings page. SSHForwarding, Experimental and EOL are derived from the
// environment (SSH_FORWARDING_HOST, EXPERIMENTAL, EOL) and are read-only — they ride
// along here because the UI already fetches this once and needs both before it
// draws anything: whether to offer a node's tunnel command, and whether the
// features tagged experimental are in the menus at all (see experimental.go). The
// update handler ignores them on the way in and re-derives them on the way out, so
// a client echoing the whole object back can neither set them nor lose them.
type SystemSettings struct {
	MaxUploadBytes int64 `json:"maxUploadBytes"`
	MaxTokenDays   int   `json:"maxTokenDays"`
	// InternalWrites allows the Database Explorer to write to the databases it
	// otherwise exposes read-only: PMM Server's internal PostgreSQL and its Query
	// Analytics ClickHouse.
	//
	// Off by default, and deliberately an instance-wide administrator setting
	// rather than something any user can flip. The databases behind it are the
	// monitoring system's own, and a stray UPDATE in one is how a PMM installation
	// stops working — so turning it on is a decision somebody makes for the whole
	// installation, once, knowingly.
	//
	// It is necessary all the same: a lab exists to break things in, and "what
	// happens to PMM when its inventory is wrong" is a scenario you cannot test
	// from a read-only connection. What it buys is that the answer is never
	// reached by accident.
	//
	// Turning it on is not sufficient on its own. A Database Explorer tab must also
	// be armed for writes explicitly before one is sent (dexQueryRequest.AllowWrites),
	// so a tab left open from before the setting changed cannot write into a PMM
	// database because somebody pressed Run.
	InternalWrites bool                 `json:"internalWrites"`
	SSHForwarding  SSHForwardingSetting `json:"sshForwarding"`
	Experimental   bool                 `json:"experimental"`
	// EOL is whether end-of-life releases are offered (EOL in .env; see eol.go). Derived and
	// read-only like Experimental, and for the same reason it rides here: the designer needs it
	// before it draws the node library and the Linux Client's OS picker.
	EOL bool `json:"eol"`
	// AllowGuestSessions lets a stack's owner share a live session through a link that
	// admits people without an account (share.go). Off by default: that is a decision
	// for the installation, made once, by an administrator.
	AllowGuestSessions bool `json:"allowGuestSessions"`
	// MaxGuestMinutes is the longest a share link may last, 5 to 120.
	MaxGuestMinutes int `json:"maxGuestMinutes"`
	// SessionRetentionDays is how long an ended shared session's records — its guests
	// with their names, emails and addresses, the transcript, the guest actions — are
	// kept before they are deleted. 0 keeps them for as long as the stack exists.
	SessionRetentionDays int `json:"sessionRetentionDays"`
	// RecordingRetentionDays is how long a session's screen recording is kept by
	// default, 1 to 3650 (sharerecord.go); its host can move the date.
	RecordingRetentionDays int `json:"recordingRetentionDays"`
	// LiveWatchSeconds is how often the watcher samples every deployed stack for its history and
	// alerts (livewatch.go), 15 to 600; 0 switches it off. Each pass costs one exec per database
	// node, which is why an administrator, not each user, decides it.
	LiveWatchSeconds int `json:"liveWatchSeconds"`
	// PublicURL is the base share links are built on (PUBLIC_URL in .env), or "" when
	// links use the address the host's browser used. Derived and read-only.
	PublicURL string `json:"publicUrl"`
}

// SSHForwardingSetting is where the app is reachable over SSH, if the
// administrator configured it. Enabled false means the feature is off and every
// other field is empty (see sshforward.go).
type SSHForwardingSetting struct {
	Enabled bool   `json:"enabled"`
	User    string `json:"user,omitempty"`
	Host    string `json:"host,omitempty"`
	Port    int    `json:"port,omitempty"`
}

// sshForwardingSetting reads SSH_FORWARDING_HOST for the settings payload. appUser is
// the signed-in account, used as the login when the configured value named none (see
// withLogin); "" is fine for callers that only want the upload ceiling.
func sshForwardingSetting(appUser string) SSHForwardingSetting {
	t, ok := sshForwardingTarget()
	if !ok {
		return SSHForwardingSetting{}
	}
	t = t.withLogin(appUser)
	return SSHForwardingSetting{Enabled: true, User: t.User, Host: t.Host, Port: t.Port}
}

func defaultSystemSettings() SystemSettings {
	return SystemSettings{MaxUploadBytes: defaultMaxUploadBytes, MaxTokenDays: defaultMaxTokenDays, MaxGuestMinutes: shareMaxMinutes,
		SessionRetentionDays: defaultShareRetentionDays, RecordingRetentionDays: defaultRecordingRetentionDays, LiveWatchSeconds: defaultWatchSeconds}
}

// normalize clamps out-of-range values rather than rejecting them, so a value
// typed into the settings page always lands somewhere usable.
func (s SystemSettings) normalize() SystemSettings {
	if s.MaxUploadBytes <= 0 {
		s.MaxUploadBytes = defaultMaxUploadBytes
	}
	if s.MaxUploadBytes < minMaxUploadBytes {
		s.MaxUploadBytes = minMaxUploadBytes
	}
	if s.MaxUploadBytes > maxMaxUploadBytes {
		s.MaxUploadBytes = maxMaxUploadBytes
	}
	if s.MaxTokenDays <= 0 {
		s.MaxTokenDays = defaultMaxTokenDays
	}
	if s.MaxTokenDays < minMaxTokenDays {
		s.MaxTokenDays = minMaxTokenDays
	}
	if s.MaxTokenDays > maxMaxTokenDays {
		s.MaxTokenDays = maxMaxTokenDays
	}
	s.MaxGuestMinutes = clampGuestMinutes(strconv.Itoa(s.MaxGuestMinutes))
	s.SessionRetentionDays = clampRetentionDays(s.SessionRetentionDays)
	s.RecordingRetentionDays = clampRecordingDays(s.RecordingRetentionDays)
	s.LiveWatchSeconds = clampWatchSeconds(s.LiveWatchSeconds)
	return s
}

// systemSettings reads the stored settings, falling back to the defaults for
// anything unset or unparseable — a hand-edited row can never wedge an upload.
func (a *App) systemSettings(appUser string) SystemSettings {
	s := defaultSystemSettings()
	if v, err := a.store.AppSetting(settingMaxUploadBytes); err == nil && v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			s.MaxUploadBytes = n
		}
	}
	if v, err := a.store.AppSetting(settingMaxTokenDays); err == nil && v != "" {
		s.MaxTokenDays = maxTokenDaysFromSetting(v)
	}
	// Anything but an explicit "1" is off, so a hand-edited or half-written row
	// fails closed rather than unlocking a PMM database.
	if v, err := a.store.AppSetting(settingInternalWrites); err == nil {
		s.InternalWrites = v == "1"
	}
	s.AllowGuestSessions, s.MaxGuestMinutes = a.guestSessionSettings()
	s.SessionRetentionDays = a.shareRetentionDays()
	s.RecordingRetentionDays = a.recordingRetentionDays()
	s.LiveWatchSeconds = a.watchSeconds()
	s = s.normalize()
	s.PublicURL = strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_URL")), "/")
	s.SSHForwarding = sshForwardingSetting(appUser)
	s.Experimental = experimentalEnabled()
	s.EOL = eolEnabled()
	return s
}

// maxUploadBytes is the configured ceiling on one node file drop.
func (a *App) maxUploadBytes() int64 { return a.systemSettings("").MaxUploadBytes }

// maxTokenDays is the configured ceiling on a non-admin API token's lifetime.
func (a *App) maxTokenDays() int { return a.systemSettings("").MaxTokenDays }

// internalWritesAllowed reports whether an administrator has unlocked writes to
// the internal databases. Read on every request that could write to one — never
// cached — so revoking it takes effect on the next query rather than the next
// restart.
func (a *App) internalWritesAllowed() bool { return a.systemSettings("").InternalWrites }

func (a *App) handleGetSystemSettings(w http.ResponseWriter, r *http.Request) {
	u, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	writeJSON(w, http.StatusOK, a.systemSettings(u.Username))
}

// handleUpdateSystemSettings is admin-only (wired through requireAdmin in main.go).
func (a *App) handleUpdateSystemSettings(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var in SystemSettings
	if err != nil || json.Unmarshal(raw, &in) != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// 0 is a real answer for retention ("keep forever"), so a body that leaves the
	// field out keeps what is stored rather than reading as 0.
	var present map[string]json.RawMessage
	json.Unmarshal(raw, &present)
	if _, ok := present["sessionRetentionDays"]; !ok {
		in.SessionRetentionDays = a.shareRetentionDays()
	}
	if _, ok := present["recordingRetentionDays"]; !ok {
		in.RecordingRetentionDays = a.recordingRetentionDays()
	}
	if _, ok := present["liveWatchSeconds"]; !ok {
		in.LiveWatchSeconds = a.watchSeconds()
	}
	s := in.normalize()
	if err := a.store.SetAppSetting(settingMaxUploadBytes, strconv.FormatInt(s.MaxUploadBytes, 10)); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save settings")
		return
	}
	if err := a.store.SetAppSetting(settingMaxTokenDays, strconv.Itoa(s.MaxTokenDays)); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save settings")
		return
	}
	internal := "0"
	if s.InternalWrites {
		internal = "1"
	}
	if err := a.store.SetAppSetting(settingInternalWrites, internal); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save settings")
		return
	}
	guests := "0"
	if s.AllowGuestSessions {
		guests = "1"
	}
	if err := a.store.SetAppSetting(settingAllowGuestSessions, guests); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save settings")
		return
	}
	if err := a.store.SetAppSetting(settingMaxGuestMinutes, strconv.Itoa(s.MaxGuestMinutes)); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save settings")
		return
	}
	if err := a.store.SetAppSetting(settingShareRetentionDays, strconv.Itoa(s.SessionRetentionDays)); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save settings")
		return
	}
	if err := a.store.SetAppSetting(settingRecordingRetentionDays, strconv.Itoa(s.RecordingRetentionDays)); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save settings")
		return
	}
	if err := a.store.SetAppSetting(settingWatchSeconds, strconv.Itoa(s.LiveWatchSeconds)); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save settings")
		return
	}
	// A shorter retention applies now, not at the next pass.
	a.purgeShareSessions()
	// Turning guest sessions off ends every live one: the switch means "nobody gets in
	// through a link", not "nobody new".
	if !s.AllowGuestSessions {
		a.endAllShares("revoked")
	}
	// Re-derived, never taken from the request: SSH forwarding and the
	// experimental switch are environment settings, and the response has to keep
	// carrying them for the client that replaced its whole settings object with
	// this reply.
	appUser := ""
	if u, ok := a.currentUser(r); ok { // requireAdmin already established there is one
		appUser = u.Username
	}
	s.SSHForwarding = sshForwardingSetting(appUser)
	s.Experimental = experimentalEnabled()
	s.EOL = eolEnabled()
	s.PublicURL = strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_URL")), "/")
	writeJSON(w, http.StatusOK, s)
}

// humanLimit renders a byte count the way a configured limit reads — exact
// binary multiples stay whole ("4 GiB"), unlike stalksummary.go's humanBytes,
// which always carries decimals because it is labelling measured values.
func humanLimit(n int64) string {
	switch {
	case n >= 1<<40 && n%(1<<40) == 0:
		return fmt.Sprintf("%d TiB", n>>40)
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%d GiB", n>>30)
	// Fractional GiB before exact MiB: 3.5 GiB is an exact MiB multiple too, and
	// "3584 MiB" is the less useful way to say it.
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
