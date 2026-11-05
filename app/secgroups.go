package main

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
)

// secgroups.go — security groups: which features each kind of account may use.
//
// There are three groups, and an account's group follows from what it already is:
//
//	Administrators   role admin
//	Regular Users    role user
//	Guests           the guest account (guestlogin.go)
//
// A feature is one page of the app — Dashboard, Database Stacks, Kanban, … — and an
// administrator decides, per group, which of them the group may use (Settings →
// Security groups). The decision is enforced here, on the server, for every API
// route: the web UI hides what a group cannot use, but the API, a token and
// dbcanvas-cli are refused the same things with a 403. Every route in api_routes.go
// belongs to one feature (routeFeature), or to the core every signed-in account needs
// whatever its group — who am I, preferences, notifications.
//
// The API feature is also the key to programmatic access: a request made with an API
// token is refused unless the token owner's group has it, and so is creating a token,
// which is how `dbcanvas-cli login` signs in. Without it a group uses DBCanvas in a
// browser only.
//
// Defaults: Administrators everything; Regular Users everything but Manage Users;
// Guests the Dashboard and Database Stacks. Two features are fixed rather than
// configurable, because the alternative is a trap: Administrators always keep Manage
// Users and Settings (otherwise an administrator could lock every administrator out
// of the setting that would undo it), and Manage Users is never given to another
// group (it is administrator access by another name: whoever manages accounts can
// make one an administrator).
//
// Most tools work on a deployed stack, so they need Database Stacks: the stack list,
// the nodes, their endpoints. Granting a tool grants Database Stacks with it, and
// taking Database Stacks away takes the tools with it (normalize).

// Group ids, which are also the keys of the stored setting.
const (
	GroupAdmin = "admin"
	GroupUser  = "user"
	GroupGuest = "guest"
)

var groupOrder = []string{GroupAdmin, GroupUser, GroupGuest}

var groupLabels = map[string]string{
	GroupAdmin: "Administrators",
	GroupUser:  "Regular Users",
	GroupGuest: "Guests",
}

// groupOf is the security group an account belongs to.
func groupOf(u User) string {
	switch {
	case u.GuestAccount:
		return GroupGuest
	case u.Role == RoleAdmin:
		return GroupAdmin
	}
	return GroupUser
}

// Feature ids. They are the page ids of the web UI's navigation (web/src/App.jsx,
// NAV and ADMIN_NAV), so the UI can filter its sidebar by them;
// TestFeaturesMatchNav keeps the two lists in step.
const (
	featDashboard   = "dashboard"
	featStacks      = "stack-designer"
	featKanban      = "kanban"
	featScratch     = "scratchboard"
	featDataGen     = "data-generator"
	featExplorer    = "database-explorer"
	featQueryRun    = "queryrun"
	featBenchmark   = "benchmark"
	featSampleCode  = "sample-code"
	featPacket      = "packet-inspector"
	featOpDebugger  = "operator-debugger"
	featCoreDump    = "core-dump"
	featStalk       = "stalk-summary"
	featLogSummary  = "log-summary"
	featFTDC        = "ftdc-summary"
	featOpSummary   = "operator-summary"
	featK8sStates   = "k8s-states"
	featAPI         = "api"
	featProfile     = "profile"
	featSettings    = "settings"
	featManageUsers = "users"
)

// Feature is one entry of the catalogue served to the settings page.
type Feature struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Requires is another feature this one cannot work without.
	Requires string `json:"requires,omitempty"`
}

// features is the catalogue, in the order the sidebar shows them.
var features = []Feature{
	{featDashboard, "Dashboard", ""},
	{featStacks, "Database Stacks", ""},
	{featKanban, "Kanban", ""},
	{featScratch, "Scratchboard", ""},
	{featDataGen, "Data Generator", featStacks},
	{featExplorer, "Database Explorer", featStacks},
	{featQueryRun, "Query Runner", featStacks},
	{featBenchmark, "Benchmark", featStacks},
	{featSampleCode, "Sample Client Code", featStacks},
	{featPacket, "Packet Inspector", featStacks},
	{featOpDebugger, "Operator Debugger", featStacks},
	{featCoreDump, "Core Dump Analyzer", featStacks},
	{featStalk, "Stalk Summary", featStacks},
	{featLogSummary, "Log Summary", featStacks},
	{featFTDC, "FTDC Summary", featStacks},
	{featOpSummary, "Operator Summary", featStacks},
	{featK8sStates, "Kubernetes States", featStacks},
	{featAPI, "API", ""},
	{featProfile, "Profile", ""},
	{featSettings, "Settings", ""},
	{featManageUsers, "Manage Users", ""},
}

func featureByID(id string) (Feature, bool) {
	for _, f := range features {
		if f.ID == id {
			return f, true
		}
	}
	return Feature{}, false
}

// lockedFeature reports whether a group's access to a feature is fixed, and to what.
func lockedFeature(group, feat string) (locked, value bool) {
	switch {
	case group == GroupAdmin && (feat == featManageUsers || feat == featSettings):
		return true, true
	case group != GroupAdmin && feat == featManageUsers:
		return true, false
	}
	return false, false
}

// defaultAllowed is a group's access to a feature nobody has configured.
func defaultAllowed(group, feat string) bool {
	switch group {
	case GroupAdmin:
		return true
	case GroupUser:
		return feat != featManageUsers
	}
	return feat == featDashboard || feat == featStacks
}

// GroupPermissions maps group → feature → allowed. Stored as JSON under one
// app_settings key; a feature missing from it (a new one, in a later release) takes
// its group's default, so an upgrade never silently opens or closes anything the
// administrator decided.
type GroupPermissions map[string]map[string]bool

const settingSecurityGroups = "securityGroups"

// normalize fills in defaults, applies the locks and the requirements, and drops
// anything unknown.
func (p GroupPermissions) normalize() GroupPermissions {
	out := GroupPermissions{}
	for _, g := range groupOrder {
		m := map[string]bool{}
		for _, f := range features {
			v, ok := p[g][f.ID]
			if !ok {
				v = defaultAllowed(g, f.ID)
			}
			if locked, fixed := lockedFeature(g, f.ID); locked {
				v = fixed
			}
			m[f.ID] = v
		}
		// A tool without Database Stacks cannot list a stack to work on.
		for _, f := range features {
			if f.Requires != "" && m[f.ID] && !m[f.Requires] {
				m[f.ID] = false
			}
		}
		out[g] = m
	}
	return out
}

// groupPermissions is the stored configuration, normalized. A missing or unreadable
// row is the defaults.
func (a *App) groupPermissions() GroupPermissions {
	p := GroupPermissions{}
	if v, err := a.store.AppSetting(settingSecurityGroups); err == nil && v != "" {
		json.Unmarshal([]byte(v), &p)
	}
	return p.normalize()
}

// allowedFeatures lists the features an account may use, in catalogue order.
func (a *App) allowedFeatures(u User) []string {
	m := a.groupPermissions()[groupOf(u)]
	out := []string{}
	for _, f := range features {
		if m[f.ID] {
			out = append(out, f.ID)
		}
	}
	return out
}

// featureAllowed reports whether u may use feat ("" is the core: always).
func (a *App) featureAllowed(u User, feat string) bool {
	if feat == "" {
		return true
	}
	return a.groupPermissions()[groupOf(u)][feat]
}

// groupFeature maps an API route group to the feature that owns it; a group not
// listed belongs to Database Stacks, which is where nearly all of them hang off (a
// node, a frame, a stack's files and certificates, its shared sessions, …).
var groupFeature = map[string]string{
	gAuth:     "",
	gPrefs:    "",
	gNotif:    "",
	gUsers:    featManageUsers,
	gTokens:   featAPI,
	gMeta:     featAPI,
	gDash:     featDashboard,
	gKanban:   featKanban,
	gDataGen:  featDataGen,
	gQueryRun: featQueryRun,
	gExplorer: featExplorer,
	gBench:    featBenchmark,
	gSample:   featSampleCode,
	gPkt:      featPacket,
	gDebug:    featOpDebugger,
	gGDB:      featCoreDump,
	gStalk:    featStalk,
	gLog:      featLogSummary,
	gFTDC:     featFTDC,
	gOpSum:    featOpSummary,
}

// routeFeature is the feature a route belongs to, "" for the core.
func routeFeature(rt apiRoute) string {
	switch {
	case rt.Auth == authPublic:
		return ""
	case strings.HasPrefix(rt.Path, "/api/me/scratchboards"):
		return featScratch
	case rt.Path == "/api/me/profile" || rt.Path == "/api/me/password":
		return featProfile
	case rt.Path == "/api/system/settings" && rt.Method != http.MethodGet:
		return featSettings
	case rt.Path == "/api/security/groups":
		// The configuration itself, which only an administrator reaches anyway.
		return featManageUsers
	// The everyday stack list and a stack's own record are what every stack-based
	// tool opens with; they stay with Database Stacks, which those tools require.
	case rt.Group == gK3D && (strings.Contains(rt.Path, "/states") || strings.HasPrefix(rt.Path, "/api/k8sstates")):
		return featK8sStates
	}
	if f, ok := groupFeature[rt.Group]; ok {
		return f
	}
	return featStacks
}

// featureGate is the per-request check, made by requireScope for a cookie session,
// an API token and a shared-session guest alike. A token also needs the API feature.
func (a *App) featureGate(w http.ResponseWriter, u User, rt apiRoute, viaToken bool) bool {
	if rt.Auth == authPublic {
		return true
	}
	if viaToken && !a.featureAllowed(u, featAPI) {
		writeErr(w, http.StatusForbidden, featureDenied(u, featAPI)+" — API tokens and dbcanvas-cli are not available to it")
		return false
	}
	if f := routeFeature(rt); !a.featureAllowed(u, f) {
		writeErr(w, http.StatusForbidden, featureDenied(u, f))
		return false
	}
	return true
}

func featureDenied(u User, feat string) string {
	label := feat
	if f, ok := featureByID(feat); ok {
		label = f.Label
	}
	return "your security group (" + groupLabels[groupOf(u)] + ") does not have access to " + label
}

// --- handlers ---

// securityGroupsView is GET /api/security/groups: the catalogue, the groups and what
// each may use, with the fixed cells marked so the settings page can lock them.
type securityGroupsView struct {
	Features []Feature                  `json:"features"`
	Groups   []securityGroup            `json:"groups"`
	Allowed  GroupPermissions           `json:"allowed"`
	Locked   map[string]map[string]bool `json:"locked"`
}

type securityGroup struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

func (a *App) securityGroupsView() securityGroupsView {
	v := securityGroupsView{Features: features, Allowed: a.groupPermissions(), Locked: map[string]map[string]bool{}}
	for _, g := range groupOrder {
		v.Groups = append(v.Groups, securityGroup{g, groupLabels[g]})
		v.Locked[g] = map[string]bool{}
		for _, f := range features {
			if locked, _ := lockedFeature(g, f.ID); locked {
				v.Locked[g][f.ID] = true
			}
		}
	}
	return v
}

func (a *App) handleGetSecurityGroups(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.securityGroupsView())
}

// handleSetSecurityGroups replaces the configuration: {"allowed": {group: {feature:
// bool}}}. A group or feature left out keeps what is stored, so a client may send
// only the cells it changed; locks and requirements are applied whatever is sent.
func (a *App) handleSetSecurityGroups(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Allowed GroupPermissions `json:"allowed"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || json.Unmarshal(raw, &in) != nil || in.Allowed == nil {
		writeErr(w, http.StatusBadRequest, `body is {"allowed": {"admin"|"user"|"guest": {"<feature id>": true|false}}}`)
		return
	}
	cur := a.groupPermissions()
	for g, m := range in.Allowed {
		if _, ok := groupLabels[g]; !ok {
			writeErr(w, http.StatusBadRequest, "unknown group "+g+": one of admin, user, guest")
			return
		}
		for f, v := range m {
			if _, ok := featureByID(f); !ok {
				writeErr(w, http.StatusBadRequest, "unknown feature "+f+": see GET /api/security/groups")
				return
			}
			cur[g][f] = v
		}
		// Granting a tool grants what it needs, unless the same request says
		// otherwise about that; then the requirement wins, and normalize takes the
		// tool away again.
		for f, v := range m {
			feat, _ := featureByID(f)
			if _, said := m[feat.Requires]; v && feat.Requires != "" && !said {
				cur[g][feat.Requires] = true
			}
		}
	}
	next := cur.normalize()
	b, _ := json.Marshal(next)
	if err := a.store.SetAppSetting(settingSecurityGroups, string(b)); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save the security groups")
		return
	}
	a.notify(Notification{Scope: "admin", Type: "security.groups", Severity: "info",
		Title: "Security groups changed", Body: "An administrator changed what " + changedGroups(in.Allowed) + " may use."})
	writeJSON(w, http.StatusOK, a.securityGroupsView())
}

func changedGroups(p GroupPermissions) string {
	var names []string
	for g := range p {
		names = append(names, groupLabels[g])
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
