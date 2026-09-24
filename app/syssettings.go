package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
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
	return SystemSettings{MaxUploadBytes: defaultMaxUploadBytes, MaxTokenDays: defaultMaxTokenDays}
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
	s = s.normalize()
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
	var in SystemSettings
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
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
