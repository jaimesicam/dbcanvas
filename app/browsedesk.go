package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// browsedesk.go — a node's web UI opened in the stack's own desktop, to share it.
//
// A browser window (browse.go) shares only an address. Every viewer loads their own
// copy of the page with their own cookies, so a guest watching Roundcube or PMM sees
// a login page, and a watcher cannot log in, because a watcher may not post. Even
// logged in, nothing that does not change the address (a preview pane, a scroll, a
// half-typed query) would follow the driver. Handing guests the host's cookies would
// not fix that, and would let every guest act as the host inside those apps.
//
// So the page is opened where there is only one copy of it: in Firefox on the
// stack's VNC desktop. The desktop is then shared like any window. One X display
// means everyone sees the same pixels; the login lives inside the container and never
// reaches a guest's browser; only the driver's keyboard and pointer reach it
// (browsevnc.go). Firefox reaches the node by its name on the stack network, so
// there is no path prefix to rewrite either.
//
// Opening a page takes control (the route is not read-only): a watcher may look at
// the desktop, not put things on it. The address must be a node link of a stack the
// caller may use, so the desktop is never pointed anywhere else.

// deskFirefoxScript opens $URL in Firefox inside the running desktop session, as the
// desktop user. The session's DISPLAY and D-Bus address are read from xfce4-session
// so the browser appears on display :1. A first call starts Firefox; later ones hand
// the address to it, and the policy below makes that a tab rather than a window.
// The process is firefox-bin on Ubuntu's Mozilla build and firefox elsewhere.
const deskFirefoxScript = `set -e
P=$(pgrep -u "$VNCUSER" -x xfce4-session | head -1)
[ -n "$P" ] || { echo "the desktop session is not running" >&2; exit 3; }
export $(tr '\0' '\n' < /proc/$P/environ | grep -E '^(DBUS_SESSION_BUS_ADDRESS|DISPLAY|HOME|XAUTHORITY)=' | xargs)
if pgrep -u "$VNCUSER" -x 'firefox|firefox-bin' >/dev/null; then
  timeout 20 firefox --new-tab "$URL" >/dev/null 2>&1 &
else
  setsid -f firefox --width 1440 --height 860 --new-window "$URL" >/dev/null 2>&1
fi
echo opened`

// deskFirefoxPolicyScript adds to the Firefox policy vncFirefoxCAScript writes what a
// browser everyone watches needs. Merged, not overwritten, so the Intranet CA stays
// trusted; Firefox reads it when it starts.
//
//   - a page handed in from DBCanvas opens as a tab, not a new window;
//   - no first-run "Welcome to Firefox" terms dialog, onboarding or default-browser
//     question covering the page;
//   - no password saving: a login typed into PMM would otherwise sit in the profile
//     for whoever is given control next, a guest included, to read in about:logins;
//   - no telemetry from a lab desktop.
const deskFirefoxPolicyScript = `set -e
F=/etc/firefox/policies/policies.json
install -d /etc/firefox/policies
python3 - "$F" <<'PY'
import json, os, sys
f = sys.argv[1]
d = {}
if os.path.exists(f):
    try:
        d = json.load(open(f))
    except Exception:
        d = {}
pol = d.setdefault("policies", {})
before = json.dumps(pol, sort_keys=True)
pol.setdefault("Preferences", {})["browser.link.open_newwindow.override.external"] = {"Value": 3, "Status": "default"}
pol.update({
    "SkipTermsOfUse": True,
    "DontCheckDefaultBrowser": True,
    "OverrideFirstRunPage": "",
    "OverridePostUpdatePage": "",
    "DisableTelemetry": True,
    "PasswordManagerEnabled": False,
    "OfferToSaveLogins": False,
})
um = pol.setdefault("UserMessaging", {})
um.update({"SkipOnboarding": True, "ExtensionRecommendations": False, "FeatureRecommendations": False,
           "MoreFromMozilla": False, "FirefoxLabs": False})
if json.dumps(pol, sort_keys=True) != before:
    json.dump(d, open(f, "w"), indent=1)
    print("firefox policy updated")
PY`

// stackDesktop is a running VNC desktop node of a stack.
type stackDesktop struct {
	Label       string
	ContainerID string
	WebPort     int
	User        string
}

// desktopOf finds a stack's running VNC desktop, if it has one.
func (a *App) desktopOf(st Stack) (stackDesktop, bool) {
	for _, n := range buildDoc(st).Nodes {
		if n.Type != "vnc" {
			continue
		}
		dep, err := a.store.GetDeployment(st.ID, n.ID)
		if err != nil || dep.State != DeployRunning || dep.ContainerID == "" {
			continue
		}
		var cfg vncConfig
		json.Unmarshal(dep.Config, &cfg)
		if cfg.WebPort == 0 {
			continue
		}
		user := cfg.VNCUser
		if user == "" {
			user = vncDefedUser
		}
		return stackDesktop{Label: n.Label, ContainerID: dep.ContainerID, WebPort: cfg.WebPort, User: user}, true
	}
	return stackDesktop{}, false
}

// insideAddress is how the desktop reaches the page a node link opens: the node's
// name on the stack network and its own port, with the link's path and query. A
// container that is not a design node (a K3D frame's load balancer) is reached by its
// address on the stack network.
func (a *App) insideAddress(ctx context.Context, nl nodeLinkTarget) (string, error) {
	host := ""
	for id, h := range stackHostnames(buildDoc(nl.st)) {
		if containerName(nl.st.ID, id) == nl.pp.Name {
			host = h
		}
	}
	if host == "" {
		ip, err := a.dialEngine(nl.st.ID, nl.pp.ContainerID).ContainerIP(ctx, nl.pp.ContainerID, networkName(nl.st.ID))
		if err != nil || ip == "" {
			return "", fmt.Errorf("the desktop cannot reach this node on the stack network")
		}
		host = ip
	}
	u := *nl.link
	u.Host = net.JoinHostPort(host, strconv.Itoa(nl.pp.ContainerPort))
	u.User = nil
	return u.String(), nil
}

// insidePage is the page a node offers only on the stack network, which no host port
// reaches: Keycloak's admin console. Keycloak runs with its hostname pinned to the
// issuer the databases check (keycloak.go), so the page is opened by that name, which
// the desktop resolves through the Intranet and whose certificate it trusts.
func (a *App) insidePage(st Stack, nodeID string) (string, error) {
	var typ string
	for _, n := range buildDoc(st).Nodes {
		if n.ID == nodeID {
			typ = n.Type
		}
	}
	if typ == "" {
		return "", fmt.Errorf("no node %q in this stack", nodeID)
	}
	dep, err := a.store.GetDeployment(st.ID, nodeID)
	if err != nil || dep.State != DeployRunning {
		return "", fmt.Errorf("the node is not running")
	}
	switch typ {
	case "keycloak":
		var cfg keycloakConfig
		json.Unmarshal(dep.Config, &cfg)
		host := firstNonEmpty(cfg.FQDN, cfg.Hostname)
		if host == "" {
			return "", fmt.Errorf("the Keycloak node has no stack address yet")
		}
		return keycloakIssuer(host, cfg.SSL) + "/admin/", nil
	}
	return "", fmt.Errorf("a %s node has no page that opens only inside the stack", typ)
}

// handleBrowseDesktop opens a page in Firefox on the stack's desktop and says which
// desktop, so the caller opens (and in a session, shares) a window on it. The page is
// a node link (url), or a node's in-stack page (stackId and nodeId) for a node that
// publishes none.
func (a *App) handleBrowseDesktop(w http.ResponseWriter, r *http.Request) {
	_, ok := a.currentUser(r)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var in struct {
		URL     string `json:"url"`
		StackID int64  `json:"stackId"`
		NodeID  string `json:"nodeId"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var (
		st     Stack
		host   string // the host the desktop's own link names
		inside string
	)
	if strings.TrimSpace(in.URL) == "" && in.StackID != 0 && in.NodeID != "" {
		s, err := a.store.GetStack(in.StackID)
		if err != nil {
			writeErr(w, http.StatusNotFound, "stack not found")
			return
		}
		st = s
		if host, _, err = net.SplitHostPort(r.Host); err != nil {
			host = r.Host
		}
		if inside, err = a.insidePage(st, in.NodeID); err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
	} else {
		nl, ok := a.resolveNodeLink(w, r, in.URL)
		if !ok {
			return
		}
		st, host = nl.st, nl.link.Hostname()
		desk, ok := a.desktopOf(st)
		if ok && nl.pp.ContainerID == desk.ContainerID {
			// The link is the desktop itself: nothing to open on it.
			writeJSON(w, http.StatusOK, map[string]any{"desktopLink": desktopLinkOf(host, desk), "desktop": desk.Label})
			return
		}
		if ok {
			var err error
			if inside, err = a.insideAddress(r.Context(), nl); err != nil {
				writeErr(w, http.StatusBadGateway, err.Error())
				return
			}
		}
	}
	desk, ok := a.desktopOf(st)
	if !ok {
		writeErr(w, http.StatusConflict, "this stack has no running VNC desktop — add one to the canvas to share pages through it")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	eng := a.dialEngine(st.ID, desk.ContainerID)
	// Best effort: without it a page opens as a window, behind a first-run dialog.
	eng.ExecAs(ctx, desk.ContainerID, "0", []string{"sh", "-c", deskFirefoxPolicyScript}, nil)
	res, err := eng.ExecAs(ctx, desk.ContainerID, desk.User, []string{"sh", "-c", deskFirefoxScript},
		[]string{"VNCUSER=" + desk.User, "URL=" + inside})
	if err != nil || res.Code != 0 {
		why := strings.TrimSpace(res.Stderr)
		if err != nil {
			why = err.Error()
		}
		if why == "" {
			why = fmt.Sprintf("exit %d", res.Code)
		}
		writeErr(w, http.StatusBadGateway, "could not open the page on the desktop: "+why)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"desktopLink": desktopLinkOf(host, desk), "desktop": desk.Label, "opened": inside})
}

// desktopLinkOf is the desktop's own page, on the host the caller reached DBCanvas by.
func desktopLinkOf(host string, desk stackDesktop) string {
	return "http://" + net.JoinHostPort(host, strconv.Itoa(desk.WebPort)) + "/vnc.html"
}
