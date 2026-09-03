package main

import (
	"net/http"
	"sync"
)

// api_routes.go — every HTTP endpoint DBCanvas serves, declared once.
//
// Why a table instead of 206 `mux.HandleFunc` calls. The route list is the app's
// public surface, and it is the one part of the app a person outside this repo has
// to be told about: it drives the API page, the OpenAPI document, `dbcanvas-cli`,
// and the scope a token needs to reach an endpoint. Written as loose registrations
// it could only be *described* — in prose, somewhere else, going stale a route at a
// time. Declared here it is published instead, from the same lines that install the
// handlers, so the description cannot disagree with what is actually served.
//
// Adding a route means adding a row. `TestEveryRouteIsDocumented` fails the build if
// that row has no Group or Summary, which is the whole point: the catalogue cannot
// fall behind the code, because the code does not compile without it.
//
// What this file deliberately does NOT do: enforce per-endpoint authentication.
// Every handler already resolves the caller itself (currentUser / loadOwnedStack /
// loadRunningNode), and moving that into a wrapper here would be a behavioural
// change wearing a refactor's clothes. `Auth` records what a handler enforces so the
// catalogue can say so and a token can be scoped against it; the single exception is
// authAdmin, which really is a wrapper today (requireAdmin) and stays one.

// authKind is who a route admits. authUser is the zero value because it describes
// all but a handful of routes — anything else is spelled out at the call site.
type authKind int

const (
	// authUser — any signed-in account; the handler scopes the result to what
	// they own (an admin usually sees everything).
	authUser authKind = iota
	// authPublic — reachable without a session. Only the five endpoints an
	// account needs before it has one.
	authPublic
	// authAdmin — wrapped in requireAdmin at registration.
	authAdmin
)

func (k authKind) String() string {
	switch k {
	case authPublic:
		return "public"
	case authAdmin:
		return "admin"
	}
	return "user"
}

// mediaKind is what a route speaks, where that is not plain JSON. It exists so the
// API page can show a `curl` line that actually works: an SSE stream, a WebSocket
// upgrade and a file download each need a different one, and offering the JSON
// example for all three would be worse than offering none.
type mediaKind string

const (
	mediaJSON      mediaKind = ""          // request and response are JSON
	mediaMultipart mediaKind = "multipart" // multipart/form-data upload
	mediaDownload  mediaKind = "download"  // streams a file back as an attachment
	mediaSSE       mediaKind = "sse"       // text/event-stream
	mediaWebSocket mediaKind = "websocket" // upgrades the connection
)

// Groups. These are the headings the API page renders under and the OpenAPI tags,
// so they read as feature names rather than file names.
const (
	gAuth      = "Setup & sign-in"
	gPrefs     = "Preferences"
	gUsers     = "Users"
	gCatalog   = "Version catalogues"
	gStacks    = "Stacks"
	gTemplates = "Templates"
	gLabs      = "Labs"
	gNodes     = "Nodes"
	gFS        = "Node file manager"
	gCaptures  = "Diagnostic captures"
	gStalk     = "Stalk Summary"
	gSamba     = "Samba AD DC"
	gDataGen   = "Data Generator"
	gQueryRun  = "Query Runner"
	gPkt       = "Packet Inspector"
	gLog       = "Log Summary"
	gFTDC      = "FTDC Summary"
	gBench     = "Benchmark"
	gDash      = "Dashboard"
	gNotif     = "Notifications"
	gMail      = "Intranet mail"
	gLDAP      = "Intranet LDAP"
	gCerts     = "Certificates"
	gSeaweed   = "SeaweedFS"
	gOpenBao   = "OpenBao"
	gClusters  = "Clusters"
	gK3D       = "Kubernetes frames"
	gDebug     = "Operator Debugger"
	gGDB       = "Core Dump Analyzer"
	gAIO       = "All in One"
	gStockSim  = "Stock Market Sim"
	gTokens    = "API tokens"
	gMeta      = "API metadata"
)

// apiGroupOrder is the order groups are presented in, which is roughly the order a
// person meets them: sign in, build a stack, run something against it, find out what
// happened, then the per-node management that hangs off a deployed node. A group
// missing from this list still renders, after the ones that are in it.
var apiGroupOrder = []string{
	gAuth, gPrefs, gUsers, gTokens, gMeta,
	gStacks, gTemplates, gCatalog, gNodes, gClusters, gLabs,
	gDataGen, gQueryRun, gBench, gStockSim,
	gPkt, gLog, gFTDC, gStalk, gCaptures, gDebug, gGDB,
	gDash, gNotif,
	gFS, gCerts, gMail, gLDAP, gSamba, gK3D, gAIO, gSeaweed, gOpenBao,
}

// apiRoute is one endpoint: how it is addressed, who may call it, what it is for,
// and which handler serves it.
type apiRoute struct {
	Method   string // GET | POST | PUT | DELETE
	Path     string // "/api/stacks/{id}/deploy", with Go 1.22 mux wildcards
	Group    string // one of the g* constants above
	Summary  string // one sentence, and the only prose a caller gets
	Auth     authKind
	ReadOnly bool // a POST/PUT that changes nothing, so a read-scoped caller may use it
	// NoToken bars bearer-token auth outright, whatever the token's scope. Only
	// token creation sets it: a token that could mint tokens would be a password
	// with none of a password's protections (see apitokens.go).
	NoToken bool
	Media   mediaKind
	Handler func(*App) http.HandlerFunc
}

// Pattern is the mux registration string ("GET /api/stacks").
func (rt apiRoute) Pattern() string { return rt.Method + " " + rt.Path }

// Mutates reports whether a caller needs write access. GET is always a read; so is
// a POST explicitly marked ReadOnly (validate, compare, preview — endpoints that are
// POSTs only because they take a body too large for a query string).
func (rt apiRoute) Mutates() bool { return rt.Method != http.MethodGet && !rt.ReadOnly }

// m adapts an *App method expression to the Handler shape, which keeps the common
// row down to one line: m((*App).handleListStacks). Routes whose handler is built by
// a factory (handleNodeAction("start") and friends) pass a closure instead.
func m(f func(*App, http.ResponseWriter, *http.Request)) func(*App) http.HandlerFunc {
	return func(a *App) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) { f(a, w, r) }
	}
}

// apiRoutes returns the whole surface, built once on first use.
//
// It is a function rather than a package-level slice for one specific reason: the
// table names handleAPIEndpoints, and handleAPIEndpoints reads the table back. Between
// two variables that is an initialization cycle, which Go rejects even though it is
// perfectly well-defined at run time — and it rejects `var x = sync.OnceValue(build)`
// for the same reason, because the cycle still runs through a variable. Between two
// functions there is no cycle to reject, so the memoisation is spelled out by hand.
var (
	apiRoutesOnce  sync.Once
	apiRoutesCache []apiRoute
)

func apiRoutes() []apiRoute {
	apiRoutesOnce.Do(func() { apiRoutesCache = buildAPIRoutes() })
	return apiRoutesCache
}

// buildAPIRoutes is the table itself. Order follows the feature it belongs to, not
// the URL, so a related pair (GET/POST on the same path) stays adjacent.
func buildAPIRoutes() []apiRoute {
	return []apiRoute{
		// --- setup & sign-in ----------------------------------------------------
		{Method: "GET", Path: "/api/setup/status", Group: gAuth, Auth: authPublic, Handler: m((*App).handleStatus),
			Summary: "Report whether this installation has an account yet, and who the caller is signed in as."},
		{Method: "POST", Path: "/api/setup", Group: gAuth, Auth: authPublic, Handler: m((*App).handleSetup),
			Summary: "Create the first account, which becomes the administrator; refused once one exists."},
		{Method: "POST", Path: "/api/auth/register", Group: gAuth, Auth: authPublic, Handler: m((*App).handleRegister),
			Summary: "Request an account. It is created pending, and waits for an admin to approve it."},
		{Method: "POST", Path: "/api/auth/login", Group: gAuth, Auth: authPublic, Handler: m((*App).handleLogin),
			Summary: "Sign in with a username and password and receive a session cookie."},
		{Method: "POST", Path: "/api/auth/logout", Group: gAuth, Auth: authPublic, Handler: m((*App).handleLogout),
			Summary: "Destroy the caller's session and clear the cookie."},
		{Method: "GET", Path: "/api/me", Group: gAuth, Handler: m((*App).handleMe),
			Summary: "The signed-in account: id, username, role and approval status."},

		// --- preferences --------------------------------------------------------
		{Method: "GET", Path: "/api/me/settings", Group: gPrefs, Handler: m((*App).handleGetSettings),
			Summary: "The caller's own UI preferences — terminal mode, theme, deployment backend."},
		{Method: "PUT", Path: "/api/me/settings", Group: gPrefs, Handler: m((*App).handleUpdateSettings),
			Summary: "Replace the caller's UI preferences. Unrecognised values fall back to the defaults."},
		{Method: "POST", Path: "/api/me/password", Group: gPrefs, NoToken: true, Handler: m((*App).handleChangePassword),
			Summary: "Change your own password. Requires the current one, and signs out every other session."},
		// Instance-wide settings: anyone signed in may read them (the designer needs
		// the upload ceiling to refuse an over-size drop client-side); only an admin
		// may change them.
		{Method: "GET", Path: "/api/system/settings", Group: gPrefs, Handler: m((*App).handleGetSystemSettings),
			Summary: "Instance-wide settings: the file-upload ceiling, and whether SSH tunnelling is configured."},
		{Method: "PUT", Path: "/api/system/settings", Group: gPrefs, Auth: authAdmin, Handler: m((*App).handleUpdateSystemSettings),
			Summary: "Change the instance-wide settings. Out-of-range values are clamped rather than rejected."},

		// --- users --------------------------------------------------------------
		{Method: "GET", Path: "/api/users", Group: gUsers, Auth: authAdmin, Handler: m((*App).handleListUsers),
			Summary: "Every account on the instance, accounts awaiting approval first."},
		{Method: "POST", Path: "/api/users/{id}/approve", Group: gUsers, Auth: authAdmin,
			Summary: "Approve a pending account so it can sign in.",
			Handler: func(a *App) http.HandlerFunc { return a.handleUserStatus(StatusApproved) }},
		{Method: "POST", Path: "/api/users/{id}/reject", Group: gUsers, Auth: authAdmin,
			Summary: "Reject an account request and revoke its sessions.",
			Handler: func(a *App) http.HandlerFunc { return a.handleUserStatus(StatusRejected) }},
		{Method: "POST", Path: "/api/users/{id}/disable", Group: gUsers, Auth: authAdmin,
			Summary: "Disable an approved account and revoke its sessions.",
			Handler: func(a *App) http.HandlerFunc { return a.handleUserStatus(StatusDisabled) }},
		{Method: "DELETE", Path: "/api/users/{id}", Group: gUsers, Auth: authAdmin, Handler: m((*App).handleDeleteUser),
			Summary: "Delete an account, cascading to its stacks, sessions and templates."},

		// --- version catalogues -------------------------------------------------
		{Method: "GET", Path: "/api/catalog/pmm", Group: gCatalog, Handler: m((*App).handlePMMCatalog),
			Summary: "PMM server versions this installation can deploy."},
		{Method: "GET", Path: "/api/catalog/pxc", Group: gCatalog, Handler: m((*App).handlePXCCatalog),
			Summary: "Percona XtraDB Cluster versions installable per OS, as recorded by `make versions`."},
		{Method: "GET", Path: "/api/catalog/proxysql", Group: gCatalog, Handler: m((*App).handleProxySQLCatalog),
			Summary: "ProxySQL versions installable per OS."},
		{Method: "GET", Path: "/api/catalog/valkey", Group: gCatalog, Handler: m((*App).handleValkeyCatalog),
			Summary: "Valkey versions installable per OS."},
		{Method: "GET", Path: "/api/catalog/ps", Group: gCatalog, Handler: m((*App).handlePSCatalog),
			Summary: "Percona Server for MySQL versions installable per OS."},
		{Method: "GET", Path: "/api/catalog/orchestrator", Group: gCatalog, Handler: m((*App).handleOrchestratorCatalog),
			Summary: "Orchestrator versions installable per OS."},
		{Method: "GET", Path: "/api/catalog/mariadb", Group: gCatalog, Handler: m((*App).handleMariaDBCatalog),
			Summary: "MariaDB versions installable per OS."},
		{Method: "GET", Path: "/api/catalog/mysqlce", Group: gCatalog, Handler: m((*App).handleMySQLCECatalog),
			Summary: "MySQL Community Edition versions installable per OS."},
		{Method: "GET", Path: "/api/catalog/psmdb", Group: gCatalog, Handler: m((*App).handlePSMDBCatalog),
			Summary: "Percona Server for MongoDB versions installable per OS."},
		{Method: "GET", Path: "/api/catalog/ppg", Group: gCatalog, Handler: m((*App).handlePPGCatalog),
			Summary: "Percona Distribution for PostgreSQL versions installable per OS."},
		{Method: "GET", Path: "/api/catalog/spock", Group: gCatalog, Handler: m((*App).handleSpockCatalog),
			Summary: "Spock (pgEdge multi-master) versions installable per OS."},
		{Method: "GET", Path: "/api/catalog/images", Group: gCatalog, Handler: m((*App).handleImagesCatalog),
			Summary: "The systemd base images built on this host, by OS family and platform."},
		{Method: "GET", Path: "/api/catalog/pdps", Group: gCatalog, Handler: m((*App).handlePDPSCatalog),
			Summary: "Percona Distribution for MySQL versions installable per OS."},
		{Method: "GET", Path: "/api/catalog/operators", Group: gCatalog, Handler: m((*App).handleOperatorsCatalog),
			Summary: "The six Kubernetes database operators a K3D frame can run, and their versions."},
		{Method: "GET", Path: "/api/catalog/k3s", Group: gCatalog, Handler: m((*App).handleK3SCatalog),
			Summary: "k3s versions a K3D cluster frame can be built on."},

		// --- stacks -------------------------------------------------------------
		{Method: "GET", Path: "/api/stacks", Group: gStacks, Handler: m((*App).handleListStacks),
			Summary: "The stacks you own; an admin sees every stack on the instance."},
		{Method: "POST", Path: "/api/stacks", Group: gStacks, Handler: m((*App).handleCreateStack),
			Summary: "Create a stack — a name, a TTL, and an initial canvas design."},
		{Method: "GET", Path: "/api/stacks/{id}", Group: gStacks, Handler: m((*App).handleGetStack),
			Summary: "One stack: its design, its TTL and the deployment record of every node in it."},
		{Method: "PUT", Path: "/api/stacks/{id}", Group: gStacks, Handler: m((*App).handleUpdateStack),
			Summary: "Replace a stack's name, TTL or canvas design."},
		{Method: "DELETE", Path: "/api/stacks/{id}", Group: gStacks, Handler: m((*App).handleDeleteStack),
			Summary: "Delete a stack and everything it deployed."},
		{Method: "POST", Path: "/api/stacks/{id}/validate", Group: gStacks, ReadOnly: true, Handler: m((*App).handleValidateStack),
			Summary: "Check a design for problems without building anything, and report them per node."},
		{Method: "POST", Path: "/api/stacks/compose", Group: gStacks, Handler: m((*App).handleComposeStack),
			Summary: "Build a stack from a short spec — kinds, counts, versions and OS — instead of a canvas design."},
		{Method: "GET", Path: "/api/stacks/compose/kinds", Group: gStacks, Handler: m((*App).handleComposeKinds),
			Summary: "The compose spec language: every kind, its options, the OS aliases, and what compose will not build."},
		{Method: "POST", Path: "/api/stacks/{id}/deploy", Group: gStacks, Handler: m((*App).handleDeployStack),
			Summary: "Provision every node in the design. Returns at once; watch the deployments for progress."},
		{Method: "POST", Path: "/api/stacks/{id}/destroy", Group: gStacks, Handler: m((*App).handleDestroyStack),
			Summary: "Cancel any deploy in flight and remove every container or VM the stack created."},

		// --- templates ----------------------------------------------------------
		// "import" is declared before "{id}" so the literal reads as the more specific
		// route it is. Go's mux prefers it regardless of order; the ordering is for
		// whoever adds the next route here.
		{Method: "GET", Path: "/api/templates", Group: gTemplates, Handler: m((*App).handleListTemplates),
			Summary: "Deployment templates available to you: the built-ins, your own, and any an admin published."},
		{Method: "POST", Path: "/api/templates", Group: gTemplates, Handler: m((*App).handleCreateTemplate),
			Summary: "Save a canvas as a reusable template, stripped of secrets, host paths and pinned ports."},
		{Method: "POST", Path: "/api/templates/import", Group: gTemplates, Handler: m((*App).handleImportTemplate),
			Summary: "Import a template from an exported `.json` document."},
		{Method: "GET", Path: "/api/templates/{id}", Group: gTemplates, Handler: m((*App).handleGetTemplate),
			Summary: "One template, including the design it will put on the canvas."},
		{Method: "PUT", Path: "/api/templates/{id}", Group: gTemplates, Handler: m((*App).handleUpdateTemplate),
			Summary: "Rename a template, re-describe it, or replace its design."},
		{Method: "DELETE", Path: "/api/templates/{id}", Group: gTemplates, Handler: m((*App).handleDeleteTemplate),
			Summary: "Delete one of your own templates."},
		{Method: "POST", Path: "/api/templates/{id}/share", Group: gTemplates, Handler: m((*App).handleShareTemplate),
			Summary: "Publish a template instance-wide, or withdraw it again. Admins only."},
		{Method: "GET", Path: "/api/templates/{id}/export", Group: gTemplates, Media: mediaDownload, Handler: m((*App).handleExportTemplate),
			Summary: "Download a template as a `.json` document you can hand over or commit."},

		// --- labs ---------------------------------------------------------------
		{Method: "GET", Path: "/api/labs", Group: gLabs, Handler: m((*App).handleListLabs),
			Summary: "The lab scenarios this installation ships, with their steps and the stack each needs."},
		{Method: "GET", Path: "/api/labs/runs", Group: gLabs, Handler: m((*App).handleListMyLabRuns),
			Summary: "Your lab attempts, finished and in progress, with the result of every graded step."},
		{Method: "POST", Path: "/api/labs/{id}/start", Group: gLabs, Handler: m((*App).handleStartLab),
			Summary: "Start a lab: deploy its disposable stack and record the cluster's starting state."},
		{Method: "POST", Path: "/api/labs/{id}/finish", Group: gLabs, Handler: m((*App).handleFinishLab),
			Summary: "Finish a lab run and stop grading it."},
		{Method: "POST", Path: "/api/labs/{id}/steps/{stepId}/check", Group: gLabs, ReadOnly: true, Handler: m((*App).handleCheckLabStep),
			Summary: "Grade one lab step against the real cluster and say why it passed or failed."},

		// --- nodes --------------------------------------------------------------
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}", Group: gNodes, Handler: m((*App).handleGetNode),
			Summary: "A deployed node's panel: what it is, where it is on the network, its credentials and its state."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/start", Group: gNodes,
			Summary: "Start a stopped node.",
			Handler: func(a *App) http.HandlerFunc { return a.handleNodeAction("start") }},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/stop", Group: gNodes,
			Summary: "Stop a running node, leaving its data in place.",
			Handler: func(a *App) http.HandlerFunc { return a.handleNodeAction("stop") }},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/restart", Group: gNodes,
			Summary: "Restart a node. Published ports may move, so re-read the node afterwards.",
			Handler: func(a *App) http.HandlerFunc { return a.handleNodeAction("restart") }},
		// The `ssh -L` line that tunnels a node's published ports to the operator's
		// own machine; off unless SSH_FORWARDING_HOST is set (see sshforward.go).
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/sshforward", Group: gNodes, Handler: m((*App).handleNodeSSHForward),
			Summary: "The exact `ssh -L` line forwarding every port this node publishes to the caller's machine."},
		// Drag files from the host onto a node's card in the designer (see nodeupload.go).
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/upload", Group: gNodes, Media: mediaMultipart, Handler: m((*App).handleNodeUpload),
			Summary: "Copy files or a whole directory into a node, streamed rather than buffered."},
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/term", Group: gNodes, Media: mediaWebSocket, Handler: m((*App).handleNodeTerminal),
			Summary: "Open a root console on a node: binary frames are keystrokes, text frames are resize messages."},

		// --- node file manager --------------------------------------------------
		// Arbitrary read/write inside a node's filesystem, scoped — like the web
		// terminal — to a stack the caller owns.
		{Method: "GET", Path: "/api/stacks/{id}/fs/nodes", Group: gFS, Handler: m((*App).handleFSNodes),
			Summary: "Which of a stack's nodes are running and can therefore be browsed."},
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/fs/list", Group: gFS, Handler: m((*App).handleFSList),
			Summary: "List a directory inside a node, with mode, owner, size and modification time."},
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/fs/identities", Group: gFS, Handler: m((*App).handleFSIdentities),
			Summary: "The users and groups that exist inside the node, for the chown picker."},
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/fs/download", Group: gFS, Media: mediaDownload, Handler: m((*App).handleFSDownload),
			Summary: "Download a file from a node, or a directory as a `.tar.gz`."},
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/fs/read", Group: gFS, Handler: m((*App).handleFSRead),
			Summary: "Read a text file from a node for editing in place."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/fs/write", Group: gFS, Handler: m((*App).handleFSWrite),
			Summary: "Write a text file back into a node, preserving its mode and ownership."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/fs/upload", Group: gFS, Media: mediaMultipart, Handler: m((*App).handleFSUpload),
			Summary: "Upload files into a directory inside a node."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/fs/mkdir", Group: gFS, Handler: m((*App).handleFSMkdir),
			Summary: "Create a directory inside a node."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/fs/delete", Group: gFS, Handler: m((*App).handleFSDelete),
			Summary: "Delete a file or directory inside a node."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/fs/rename", Group: gFS, Handler: m((*App).handleFSRename),
			Summary: "Rename or move a path inside a node."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/fs/chmod", Group: gFS, Handler: m((*App).handleFSChmod),
			Summary: "Change a path's permission bits inside a node."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/fs/chown", Group: gFS, Handler: m((*App).handleFSChown),
			Summary: "Change a path's owner or group inside a node."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/fs/transfer", Group: gFS, Handler: m((*App).handleFSTransfer),
			Summary: "Copy a path from one node straight to another — the fast way to seed every cluster member."},

		// --- diagnostic captures ------------------------------------------------
		// pg_gather (PostgreSQL) + pt-stalk (MySQL family), run on the node itself.
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/pggather", Group: gCaptures, Handler: m((*App).handlePGGatherStatus),
			Summary: "Whether a pg_gather capture is running on this node, and how far it has got."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/pggather", Group: gCaptures, Handler: m((*App).handlePGGatherStart),
			Summary: "Start a pg_gather capture on a PostgreSQL node."},
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/pggather/download", Group: gCaptures, Media: mediaDownload, Handler: m((*App).handlePGGatherDownload),
			Summary: "Download the finished pg_gather report."},
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/ptstalk", Group: gCaptures, Handler: m((*App).handlePTStalkStatus),
			Summary: "Whether pt-stalk is running on this node, and what it has collected so far."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/ptstalk", Group: gCaptures, Handler: m((*App).handlePTStalkStart),
			Summary: "Start a pt-stalk capture on a MySQL-family node."},
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/ptstalk/download", Group: gCaptures, Media: mediaDownload, Handler: m((*App).handlePTStalkDownload),
			Summary: "Download the current pt-stalk capture as a gzipped archive."},
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/ptstalk/archives", Group: gCaptures, Handler: m((*App).handlePTStalkArchives),
			Summary: "The kept pt-stalk archives captured from this node."},
		// Kept captures, addressed by their own id rather than by node: an archive
		// outlives the node it came from, which is the point of keeping it.
		{Method: "GET", Path: "/api/ptstalk/archives", Group: gCaptures, Handler: m((*App).handleStalkArchives),
			Summary: "Every pt-stalk archive you have kept, newest first — they outlive the nodes they came from."},
		{Method: "GET", Path: "/api/ptstalk/archives/{aid}/download", Group: gCaptures, Media: mediaDownload, Handler: m((*App).handleArchiveDownload),
			Summary: "Download a kept pt-stalk archive."},
		{Method: "POST", Path: "/api/ptstalk/archives/{aid}/note", Group: gCaptures, Handler: m((*App).handleArchiveNote),
			Summary: "Annotate a kept archive with what was happening when it was captured."},
		{Method: "DELETE", Path: "/api/ptstalk/archives/{aid}", Group: gCaptures, Handler: m((*App).handleArchiveDelete),
			Summary: "Delete a kept pt-stalk archive and its tarball on disk."},

		// --- Stalk Summary ------------------------------------------------------
		{Method: "POST", Path: "/api/stalksummary/upload", Group: gStalk, Media: mediaMultipart, Handler: m((*App).handleStalkUpload),
			Summary: "Upload a pt-stalk archive and turn it into timeline charts, verdicts and configuration advice."},
		{Method: "POST", Path: "/api/stalksummary/archive/{aid}", Group: gStalk, ReadOnly: true, Handler: m((*App).handleStalkFromArchive),
			Summary: "Analyse a pt-stalk archive already kept on this installation."},
		{Method: "POST", Path: "/api/stalksummary/compare", Group: gStalk, ReadOnly: true, Handler: m((*App).handleStalkCompare),
			Summary: "Put two pt-stalk captures head to head and report what changed between them."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/stalksummary", Group: gStalk, ReadOnly: true, Handler: m((*App).handleStalkNode),
			Summary: "Capture and analyse a node's pt-stalk output in one request."},

		// --- Stock Market Sim ---------------------------------------------------
		// Node-scoped by URL for consistency, though the check itself only needs the
		// stack's network — see handleStockSimTest.
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/stocksim/test", Group: gStockSim, ReadOnly: true, Handler: m((*App).handleStockSimTest),
			Summary: "Test a hand-entered database connection from inside the stack network before deploying with it."},

		// --- Samba AD DC --------------------------------------------------------
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/samba/users", Group: gSamba, Handler: m((*App).handleSambaUsers),
			Summary: "Directory users on a Samba AD DC node."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/samba/users", Group: gSamba, Handler: m((*App).handleSambaUserCreate),
			Summary: "Create a directory user on a Samba AD DC node."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/samba/users/update", Group: gSamba, Handler: m((*App).handleSambaUserUpdate),
			Summary: "Change a Samba directory user's attributes."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/samba/users/password", Group: gSamba, Handler: m((*App).handleSambaUserPassword),
			Summary: "Set a Samba directory user's password."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/samba/users/delete", Group: gSamba, Handler: m((*App).handleSambaUserDelete),
			Summary: "Delete a Samba directory user."},
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/samba/groups", Group: gSamba, Handler: m((*App).handleSambaGroups),
			Summary: "Directory groups on a Samba AD DC node, with their members."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/samba/groups", Group: gSamba, Handler: m((*App).handleSambaGroupCreate),
			Summary: "Create a directory group on a Samba AD DC node."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/samba/groups/members", Group: gSamba, Handler: m((*App).handleSambaGroupMembers),
			Summary: "Replace a Samba directory group's membership."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/samba/groups/delete", Group: gSamba, Handler: m((*App).handleSambaGroupDelete),
			Summary: "Delete a Samba directory group."},
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/samba/krb5", Group: gSamba, Handler: m((*App).handleSambaKrb5),
			Summary: "The `krb5.conf` a client needs to authenticate against this domain."},
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/samba/targets", Group: gSamba, Handler: m((*App).handleSambaTargets),
			Summary: "The database nodes in this stack a Kerberos service principal can be minted for."},
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/samba/principals", Group: gSamba, Handler: m((*App).handleSambaPrincipals),
			Summary: "Kerberos service principals registered in the domain."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/samba/principals", Group: gSamba, Handler: m((*App).handleSambaPrincipalCreate),
			Summary: "Register a Kerberos service principal for a database node."},
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/samba/keytab", Group: gSamba, Handler: m((*App).handleSambaKeytab),
			Summary: "Export a principal's keytab so the database node can accept GSSAPI logins."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/samba/cert", Group: gSamba, Handler: m((*App).handleSambaCert),
			Summary: "Issue or reissue the Samba node's LDAPS certificate from the Intranet CA."},

		// --- Data Generator -----------------------------------------------------
		{Method: "GET", Path: "/api/datagen/connections", Group: gDataGen, Handler: m((*App).handleDataGenConnections),
			Summary: "The deployed database nodes the Data Generator can write to."},
		{Method: "GET", Path: "/api/datagen/stacks/{id}/nodes/{nid}/databases", Group: gDataGen, Handler: m((*App).handleDataGenDatabases),
			Summary: "The databases on a node, as the generator sees them."},
		{Method: "GET", Path: "/api/datagen/stacks/{id}/nodes/{nid}/tables", Group: gDataGen, Handler: m((*App).handleDataGenTables),
			Summary: "The tables or collections in a database, with their current row counts."},
		{Method: "GET", Path: "/api/datagen/stacks/{id}/nodes/{nid}/columns", Group: gDataGen, Handler: m((*App).handleDataGenColumns),
			Summary: "A table's columns and inferred generators, which is what the form is built from."},
		{Method: "POST", Path: "/api/datagen/stacks/{id}/nodes/{nid}/preview", Group: gDataGen, ReadOnly: true, Handler: m((*App).handleDataGenPreview),
			Summary: "Generate a handful of sample rows without writing them, to check the shape before committing."},
		{Method: "POST", Path: "/api/datagen/stacks/{id}/nodes/{nid}/generate", Group: gDataGen, Handler: m((*App).handleDataGenGenerate),
			Summary: "Start a generation job and return its id. Progress is polled from the job endpoint."},
		{Method: "GET", Path: "/api/datagen/jobs/{job}", Group: gDataGen, Handler: m((*App).handleDataGenJob),
			Summary: "A generation job's progress, throughput and any error that stopped it."},
		{Method: "POST", Path: "/api/datagen/jobs/{job}/cancel", Group: gDataGen, Handler: m((*App).handleDataGenCancel),
			Summary: "Cancel a running generation job. Rows already written stay written."},

		// --- Query Runner -------------------------------------------------------
		{Method: "GET", Path: "/api/queryrun/targets", Group: gQueryRun, Handler: m((*App).handleQueryRunTargets),
			Summary: "The nodes a query can be run against, grouped by stack."},
		{Method: "POST", Path: "/api/queryrun/runs", Group: gQueryRun, Handler: m((*App).handleQueryRunStart),
			Summary: "Run SQL across several nodes in parallel, gated on the processlist."},
		{Method: "GET", Path: "/api/queryrun/runs/{id}", Group: gQueryRun, Handler: m((*App).handleQueryRunStatus),
			Summary: "A query run's per-node progress, result sets and errors."},
		{Method: "POST", Path: "/api/queryrun/runs/{id}/stop", Group: gQueryRun, Handler: m((*App).handleQueryRunStop),
			Summary: "Stop a query run and kill the statements it started."},
		{Method: "GET", Path: "/api/queryrun/history", Group: gQueryRun, Handler: m((*App).handleQueryRunHistory),
			Summary: "Your previous query runs."},

		// --- Packet Inspector ---------------------------------------------------
		// Ranges are query parameters on the list/timeline endpoints, so the timeline
		// can be zoomed and paged without re-capturing (see pktinspect.go).
		{Method: "GET", Path: "/api/pktinspect/targets", Group: gPkt, Handler: m((*App).handlePktTargets),
			Summary: "The database nodes a packet capture can run on, and which port each speaks."},
		{Method: "GET", Path: "/api/pktinspect/captures", Group: gPkt, Handler: m((*App).handlePktList),
			Summary: "Your packet captures, running and finished."},
		{Method: "POST", Path: "/api/pktinspect/captures", Group: gPkt, Handler: m((*App).handlePktStart),
			Summary: "Start a tcpdump capture on a node, bounded by duration, size or packet count."},
		{Method: "POST", Path: "/api/pktinspect/upload", Group: gPkt, Media: mediaMultipart, Handler: m((*App).handlePktUpload),
			Summary: "Upload a `.pcap` captured elsewhere and decode it here, optionally with the server log beside it."},
		{Method: "GET", Path: "/api/pktinspect/captures/{id}", Group: gPkt, Handler: m((*App).handlePktGet),
			Summary: "A capture's state, its summary counters and what protocol was decoded."},
		{Method: "POST", Path: "/api/pktinspect/captures/{id}/stop", Group: gPkt, Handler: m((*App).handlePktStop),
			Summary: "Stop a running capture and decode what it collected."},
		{Method: "GET", Path: "/api/pktinspect/captures/{id}/packets", Group: gPkt, Handler: m((*App).handlePktPackets),
			Summary: "A page of decoded packets, filtered and ranged by query parameters."},
		{Method: "GET", Path: "/api/pktinspect/captures/{id}/packets/{no}", Group: gPkt, Handler: m((*App).handlePktPacket),
			Summary: "One packet in full: its fields, its bytes, and the exchange it belongs to."},
		{Method: "GET", Path: "/api/pktinspect/captures/{id}/timeline", Group: gPkt, Handler: m((*App).handlePktTimeline),
			Summary: "The capture bucketed over time, for the zoomable timeline."},
		{Method: "GET", Path: "/api/pktinspect/captures/{id}/download", Group: gPkt, Media: mediaDownload, Handler: m((*App).handlePktDownload),
			Summary: "Download the raw `.pcap`, to open in Wireshark."},
		// The error-log side: the MY-xxxxxx records a capture is blind to by
		// construction (aborted connections, DNS, TLS, listener).
		{Method: "GET", Path: "/api/pktinspect/captures/{id}/serverlog", Group: gPkt, Handler: m((*App).handlePktServerLog),
			Summary: "The server-log records from the capture's window that no capture could see — aborts, TLS, DNS, listener."},

		// --- Log Summary --------------------------------------------------------
		{Method: "GET", Path: "/api/logsummary/targets", Group: gLog, Handler: m((*App).handleLogTargets),
			Summary: "The nodes, clusters and Kubernetes frames whose logs can be collected."},
		{Method: "GET", Path: "/api/logsummary/bundles", Group: gLog, Handler: m((*App).handleLogList),
			Summary: "Your log bundles, collected and uploaded."},
		{Method: "POST", Path: "/api/logsummary/collect", Group: gLog, Handler: m((*App).handleLogCollect),
			Summary: "Collect several nodes' logs in one request and read them as a single timeline."},
		{Method: "POST", Path: "/api/logsummary/upload", Group: gLog, Media: mediaMultipart, Handler: m((*App).handleLogUpload),
			Summary: "Upload log files from elsewhere and classify them the same way."},
		{Method: "GET", Path: "/api/logsummary/bundles/{id}", Group: gLog, Handler: m((*App).handleLogGet),
			Summary: "A bundle's sources, its verdict, and the counts behind it."},
		{Method: "DELETE", Path: "/api/logsummary/bundles/{id}", Group: gLog, Handler: m((*App).handleLogDelete),
			Summary: "Delete a log bundle."},
		{Method: "GET", Path: "/api/logsummary/bundles/{id}/events", Group: gLog, Handler: m((*App).handleLogEvents),
			Summary: "A page of classified events, filtered by severity, class, node or time."},
		{Method: "GET", Path: "/api/logsummary/bundles/{id}/timeline", Group: gLog, Handler: m((*App).handleLogTimeline),
			Summary: "The bundle bucketed over time, per source, for the swimlane."},
		{Method: "GET", Path: "/api/logsummary/bundles/{id}/at", Group: gLog, Handler: m((*App).handleLogAt),
			Summary: "What every node was doing at one instant — the cross-member snapshot."},
		{Method: "GET", Path: "/api/logsummary/bundles/{id}/sources/{src}/raw", Group: gLog, Media: mediaDownload, Handler: m((*App).handleLogRaw),
			Summary: "Download one source's log exactly as it was read."},
		{Method: "POST", Path: "/api/logsummary/bundles/{id}/offset", Group: gLog, Handler: m((*App).handleLogOffset),
			Summary: "Correct a source's clock offset so its events line up with the rest of the timeline."},

		// --- FTDC Summary -------------------------------------------------------
		{Method: "GET", Path: "/api/ftdc/targets", Group: gFTDC, Handler: m((*App).handleFTDCTargets),
			Summary: "The MongoDB nodes whose `diagnostic.data` can be read."},
		{Method: "POST", Path: "/api/ftdc/upload", Group: gFTDC, Media: mediaMultipart, Handler: m((*App).handleFTDCUpload),
			Summary: "Upload `diagnostic.data` files and turn them into charts and verdicts."},
		{Method: "POST", Path: "/api/ftdc/compare", Group: gFTDC, ReadOnly: true, Handler: m((*App).handleFTDCCompare),
			Summary: "Compare two FTDC windows and report what moved between them."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/ftdc", Group: gFTDC, ReadOnly: true, Handler: m((*App).handleFTDCNode),
			Summary: "Read a running mongod's own `diagnostic.data` straight off the node."},

		// --- Benchmark ----------------------------------------------------------
		{Method: "GET", Path: "/api/benchmark/targets", Group: gBench, Handler: m((*App).handleBenchTargets),
			Summary: "The nodes a benchmark can be run against, and which workloads each supports."},
		{Method: "POST", Path: "/api/benchmark/runs", Group: gBench, Handler: m((*App).handleBenchStart),
			Summary: "Start an OLTP, OLAP, read-write or read-only workload and measure it."},
		{Method: "GET", Path: "/api/benchmark/runs/{id}", Group: gBench, Handler: m((*App).handleBenchStatus),
			Summary: "A benchmark's live throughput and latency, and its final numbers once done."},
		{Method: "POST", Path: "/api/benchmark/runs/{id}/stop", Group: gBench, Handler: m((*App).handleBenchStop),
			Summary: "Stop a benchmark and keep the results measured so far."},
		{Method: "GET", Path: "/api/benchmark/history", Group: gBench, Handler: m((*App).handleBenchHistory),
			Summary: "Your previous benchmark runs, for comparison."},

		// --- dashboard ----------------------------------------------------------
		{Method: "GET", Path: "/api/dashboard/summary", Group: gDash, Handler: m((*App).handleDashboardSummary),
			Summary: "Cheap counters from the store: stacks, nodes, engines, jobs and recent activity."},
		{Method: "GET", Path: "/api/dashboard/stats", Group: gDash, Handler: m((*App).handleDashboardStats),
			Summary: "A live CPU, memory, network and disk sample of every running node. Cached for two seconds."},

		// --- notifications ------------------------------------------------------
		{Method: "GET", Path: "/api/notifications", Group: gNotif, Handler: m((*App).handleListNotifications),
			Summary: "Your notifications, newest first, with the unread count."},
		{Method: "GET", Path: "/api/notifications/stream", Group: gNotif, Media: mediaSSE, Handler: m((*App).handleNotifStream),
			Summary: "Server-sent events pushing each new notification as it happens."},
		{Method: "POST", Path: "/api/notifications/read-all", Group: gNotif, Handler: m((*App).handleMarkAllRead),
			Summary: "Mark every notification read."},
		{Method: "POST", Path: "/api/notifications/{id}/read", Group: gNotif, Handler: m((*App).handleMarkNotificationRead),
			Summary: "Mark one notification read."},

		// --- Intranet mail ------------------------------------------------------
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/email/users", Group: gMail, Handler: m((*App).handleEmailList),
			Summary: "Mailboxes on the Intranet node."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/email/users", Group: gMail,
			Summary: "Create a mailbox on the Intranet node.",
			Handler: func(a *App) http.HandlerFunc { return a.emailMutate(emailAddScript, true) }},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/email/users/password", Group: gMail,
			Summary: "Set a mailbox's password.",
			Handler: func(a *App) http.HandlerFunc { return a.emailMutate(emailPasswordScript, true) }},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/email/users/delete", Group: gMail,
			Summary: "Delete a mailbox from the Intranet node.",
			Handler: func(a *App) http.HandlerFunc { return a.emailMutate(emailDeleteScript, false) }},

		// --- Intranet LDAP ------------------------------------------------------
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/ldap/users", Group: gLDAP, Handler: m((*App).handleLdapUsers),
			Summary: "OpenLDAP users on the Intranet node — the accounts database LDAP auth binds against."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/ldap/users", Group: gLDAP,
			Summary: "Create an OpenLDAP user.",
			Handler: func(a *App) http.HandlerFunc { return a.ldapUserMutate(ldapUserCreateScript, false) }},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/ldap/users/update", Group: gLDAP,
			Summary: "Change an OpenLDAP user's attributes.",
			Handler: func(a *App) http.HandlerFunc { return a.ldapUserMutate(ldapUserUpdateScript, false) }},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/ldap/users/password", Group: gLDAP,
			Summary: "Set an OpenLDAP user's password.",
			Handler: func(a *App) http.HandlerFunc { return a.ldapUserMutate(ldapUserPasswordScript, true) }},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/ldap/users/delete", Group: gLDAP,
			Summary: "Delete an OpenLDAP user.",
			Handler: func(a *App) http.HandlerFunc { return a.ldapUserMutate(ldapUserDeleteScript, false) }},
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/ldap/groups", Group: gLDAP, Handler: m((*App).handleLdapGroups),
			Summary: "OpenLDAP groups on the Intranet node, with their members."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/ldap/groups", Group: gLDAP,
			Summary: "Create an OpenLDAP group.",
			Handler: func(a *App) http.HandlerFunc { return a.ldapGroupMutate(ldapGroupCreateScript, false) }},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/ldap/groups/members", Group: gLDAP,
			Summary: "Replace an OpenLDAP group's membership.",
			Handler: func(a *App) http.HandlerFunc { return a.ldapGroupMutate(ldapGroupMembersScript, true) }},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/ldap/groups/delete", Group: gLDAP,
			Summary: "Delete an OpenLDAP group.",
			Handler: func(a *App) http.HandlerFunc { return a.ldapGroupMutate(ldapGroupDeleteScript, false) }},

		// --- certificates -------------------------------------------------------
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/cert", Group: gCerts, Handler: m((*App).handleCertInfo),
			Summary: "The Intranet CA's own certificate: subject, validity and fingerprint."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/cert", Group: gCerts, Handler: m((*App).handleCertGenerate),
			Summary: "Reissue the Intranet node's certificate from its CA."},
		// Intranet CA — issue X.509 client certificates for MySQL/PostgreSQL/MongoDB users.
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/dbcerts", Group: gCerts, Handler: m((*App).handleDBCertList),
			Summary: "The client certificates the Intranet CA has issued for database users."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/dbcerts", Group: gCerts, Handler: m((*App).handleDBCertGenerate),
			Summary: "Issue an X.509 client certificate for a database user."},
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/dbcerts/{user}", Group: gCerts, Handler: m((*App).handleDBCertGet),
			Summary: "One issued client certificate, with the key and the CA chain to install with it."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/dbcerts/delete", Group: gCerts, Handler: m((*App).handleDBCertDelete),
			Summary: "Delete an issued client certificate."},
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/pmm/cert", Group: gCerts, Handler: m((*App).handlePMMCertInfo),
			Summary: "The PMM server's TLS certificate."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/pmm/cert", Group: gCerts, Handler: m((*App).handlePMMCertGenerate),
			Summary: "Reissue the PMM server's TLS certificate from the Intranet CA."},
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/pxc/cert", Group: gCerts, Handler: m((*App).handlePXCCertInfo),
			Summary: "A PXC node's server and cluster-internal certificates."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/pxc/cert", Group: gCerts, Handler: m((*App).handlePXCCertGenerate),
			Summary: "Reissue a PXC node's certificates from the Intranet CA."},
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/pg/cert", Group: gCerts, Handler: m((*App).handlePGCertInfo),
			Summary: "A PostgreSQL node's server certificate."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/pg/cert", Group: gCerts, Handler: m((*App).handlePGCertGenerate),
			Summary: "Reissue a PostgreSQL node's server certificate from the Intranet CA."},
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/mongo/cert", Group: gCerts, Handler: m((*App).handleMongoCertInfo),
			Summary: "A MongoDB node's server and cluster certificates."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/mongo/cert", Group: gCerts, Handler: m((*App).handleMongoCertGenerate),
			Summary: "Reissue a MongoDB node's certificates from the Intranet CA."},

		// --- SeaweedFS ----------------------------------------------------------
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/seaweed/objects", Group: gSeaweed, Handler: m((*App).handleSeaweedObjects),
			Summary: "Browse the objects in a SeaweedFS S3 bucket, read-only — usually to confirm a backup landed."},

		// --- OpenBao ------------------------------------------------------------
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/openbao/status", Group: gOpenBao, Handler: m((*App).handleOpenBaoStatus),
			Summary: "Whether the OpenBao node is initialised and sealed, and its key thresholds."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/openbao/unseal", Group: gOpenBao, Handler: m((*App).handleOpenBaoUnseal),
			Summary: "Unseal the OpenBao node with its stored unseal keys."},

		// --- clusters (frames) --------------------------------------------------
		{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/pmm", Group: gClusters, Handler: m((*App).handlePXCFrameMonitor),
			Summary: "Add or remove PMM monitoring on every member of a cluster, after it was deployed."},
		{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/orchestrator", Group: gClusters, Handler: m((*App).handleFrameOrchestrator),
			Summary: "Add or remove Orchestrator monitoring on a MySQL replication cluster, after it was deployed."},
		{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/patroni/backup", Group: gClusters, Handler: m((*App).handlePatroniBackup),
			Summary: "Run an on-demand pgBackRest backup on a Patroni cluster."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/pg/backup", Group: gClusters, Handler: m((*App).handlePGBackup),
			Summary: "Run an on-demand pgBackRest backup on a standalone PostgreSQL node."},
		{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/pbm/backup", Group: gClusters, Handler: m((*App).handleMongoPBMBackup),
			Summary: "Run an on-demand PBM backup on a MongoDB replica set or sharded cluster."},
		{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/barman/backup", Group: gClusters, Handler: m((*App).handleRepmgrBackup),
			Summary: "Run an on-demand Barman cloud backup on a repmgr cluster's primary."},

		// --- Kubernetes frames --------------------------------------------------
		{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/kubeconfig", Group: gK3D, Handler: m((*App).handleK3DKubeconfig),
			Summary: "An admin kubeconfig for a K3D cluster frame, ready to paste into `kubectl`."},
		{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/users", Group: gK3D, Handler: m((*App).handleK3DUsers),
			Summary: "The Kubernetes RBAC users created on this frame for testing."},
		{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/users", Group: gK3D, Handler: m((*App).handleK3DUserCreate),
			Summary: "Create a Kubernetes RBAC user on the frame with a chosen role."},
		{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/users/delete", Group: gK3D, Handler: m((*App).handleK3DUserDelete),
			Summary: "Delete a Kubernetes RBAC user and its bindings."},
		{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/users/{username}/kubeconfig", Group: gK3D, Handler: m((*App).handleK3DUserKubeconfig),
			Summary: "A kubeconfig scoped to one RBAC user, for testing what that role can actually do."},

		// --- Operator Debugger --------------------------------------------------
		{Method: "GET", Path: "/api/k3d/debug/targets", Group: gDebug, Handler: m((*App).handleK3DDebugTargets),
			Summary: "The K3D frames whose operator is running under Delve, and which operator each runs."},
		{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/debug/sources", Group: gDebug, Handler: m((*App).handleK3DDebugSources),
			Summary: "The operator's own source tree, as the debugger sees it."},
		{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/debug/source", Group: gDebug, Handler: m((*App).handleK3DDebugSource),
			Summary: "One source file from the operator, for setting a breakpoint in it."},
		{Method: "GET", Path: "/api/stacks/{id}/frames/{fid}/k3d/debug/ws", Group: gDebug, Media: mediaWebSocket, Handler: m((*App).handleK3DDebugWS),
			Summary: "The live debug session: breakpoints, stepping, the call stack, locals and expression evaluation."},
		{Method: "POST", Path: "/api/stacks/{id}/frames/{fid}/k3d/debug/reconcile", Group: gDebug, Handler: m((*App).handleK3DDebugReconcile),
			Summary: "Annotate the custom resource to force a reconcile, so a breakpoint in `Reconcile` is actually reached."},

		// --- Core Dump Analyzer -------------------------------------------------
		{Method: "GET", Path: "/api/gdb/targets", Group: gGDB, Handler: m((*App).handleGDBTargets),
			Summary: "The Linux Client nodes deployed as core-dump analysis hosts."},
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/gdb/cores", Group: gGDB, Handler: m((*App).handleGDBCores),
			Summary: "What is in the mounted core directory, and whether the symbols and libraries match it."},
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/gdb/ws", Group: gGDB, Media: mediaWebSocket, Handler: m((*App).handleGDBWS),
			Summary: "The live gdb session: the diagnosis, threads, backtrace, frame arguments and expressions."},

		// --- All in One ---------------------------------------------------------
		// Every action execs the container's own `aioctl`, so the UI and the CLI
		// cannot diverge.
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/aio/instances", Group: gAIO, Handler: m((*App).handleAIOInstances),
			Summary: "Every database instance inside an All-in-One node, with its engine, version, port and state."},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/aio/instances/{inst}/start", Group: gAIO,
			Summary: "Start one instance inside an All-in-One node.",
			Handler: func(a *App) http.HandlerFunc { return a.handleAIOInstanceAction("start") }},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/aio/instances/{inst}/stop", Group: gAIO,
			Summary: "Stop one instance inside an All-in-One node.",
			Handler: func(a *App) http.HandlerFunc { return a.handleAIOInstanceAction("stop") }},
		{Method: "POST", Path: "/api/stacks/{id}/nodes/{nid}/aio/instances/{inst}/restart", Group: gAIO,
			Summary: "Restart one instance inside an All-in-One node.",
			Handler: func(a *App) http.HandlerFunc { return a.handleAIOInstanceAction("restart") }},
		{Method: "GET", Path: "/api/stacks/{id}/nodes/{nid}/aio/instances/{inst}/logs", Group: gAIO, Handler: m((*App).handleAIOInstanceLogs),
			Summary: "The tail of one All-in-One instance's log."},

		// --- API tokens ---------------------------------------------------------
		{Method: "GET", Path: "/api/tokens", Group: gTokens, Handler: m((*App).handleListTokens),
			Summary: "Your API tokens, with what each may do and when it expires. Never the secret."},
		{Method: "POST", Path: "/api/tokens", Group: gTokens, NoToken: true, Handler: m((*App).handleCreateToken),
			Summary: "Create an API token and return its secret, once. Requires a password sign-in, not a token."},
		{Method: "DELETE", Path: "/api/tokens/{id}", Group: gTokens, Handler: m((*App).handleRevokeToken),
			Summary: "Revoke one of your own tokens. It stops working immediately and stays listed as revoked."},
		{Method: "GET", Path: "/api/admin/tokens", Group: gTokens, Auth: authAdmin, Handler: m((*App).handleAdminListTokens),
			Summary: "Every API token on the instance, with its owner. Active ones first."},
		{Method: "DELETE", Path: "/api/admin/tokens/{id}", Group: gTokens, Auth: authAdmin, Handler: m((*App).handleAdminRevokeToken),
			Summary: "Revoke anyone's token, and tell them it happened."},

		// --- What's new ---------------------------------------------------------
		{Method: "GET", Path: "/api/whatsnew", Group: gPrefs, Handler: m((*App).handleWhatsNew),
			Summary: "This build's release notes, and whether the caller has read the ones for it yet."},
		{Method: "POST", Path: "/api/whatsnew/seen", Group: gPrefs, Handler: m((*App).handleWhatsNewSeen),
			Summary: "Record that the caller has read this build's release notes, so the dialog stops opening."},

		// --- API metadata -------------------------------------------------------
		{Method: "GET", Path: "/api/cli/download", Group: gMeta, Media: mediaDownload, Handler: m((*App).handleCLIDownload),
			Summary: "Download the dbcanvas-cli binary for an ?os= and ?arch=, as shipped in this image."},
		{Method: "GET", Path: "/api/meta/endpoints", Group: gMeta, Handler: m((*App).handleAPIEndpoints),
			Summary: "This catalogue: every endpoint, its group, what it does, and the access it needs."},
		{Method: "GET", Path: "/api/meta/openapi.json", Group: gMeta, Handler: m((*App).handleAPIOpenAPI),
			Summary: "The same surface as an OpenAPI 3.1 document, for generated clients and API tools."},
	}
}

// registerRoutes installs every route on the mux, wrapping each in the two things
// that are decided here rather than in the handler: requireAdmin, which is the one
// pre-existing wrapper, and requireScope, which is a no-op for a cookie session and
// enforces a bearer token's scope against this particular route. Everything else —
// who the caller is, and whether the stack is theirs — stays inside the handlers,
// where it already was.
func (a *App) registerRoutes(mux *http.ServeMux) {
	for _, rt := range apiRoutes() {
		h := rt.Handler(a)
		if rt.Auth == authAdmin {
			h = a.requireAdmin(h)
		}
		mux.HandleFunc(rt.Pattern(), a.requireScope(rt, h))
	}
}
