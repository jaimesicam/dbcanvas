package main

import (
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed all:web/dist
var embeddedFS embed.FS

// App holds shared dependencies for the HTTP handlers.
type App struct {
	store  *Store
	docker *Docker
	// barriers holds the per-stack replication barrier for an in-flight deploy
	// (stackID -> *deployBarrier). See replication.go.
	barriers sync.Map
	// captures holds the state of in-flight/completed on-node diagnostic captures
	// (pg_gather / pt-stalk), keyed by stack/node/kind. See diag.go.
	captures sync.Map
	// versionProbes holds the in-flight "what version actually got deployed" probes
	// (nodeID -> true), so a polling UI cannot pile them up. See nodeversion.go.
	versionProbes sync.Map
	// deploys holds the in-flight provisioning run per stack
	// (stackID -> *deployRun), so destroy can cancel it and wait for the
	// provisioners to exit before removing containers. See deployrun.go.
	deploys sync.Map
}

func main() {
	// Health-check mode for the container HEALTHCHECK (distroless has no shell).
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck())
	}

	dbPath := envOr("DB_PATH", "dbcanvas.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		log.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	app := &App{store: store, docker: NewDocker(envOr("DOCKER_SOCK", "/var/run/docker.sock"))}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/setup/status", app.handleStatus)
	mux.HandleFunc("POST /api/setup", app.handleSetup)
	mux.HandleFunc("POST /api/auth/register", app.handleRegister)
	mux.HandleFunc("POST /api/auth/login", app.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", app.handleLogout)
	mux.HandleFunc("GET /api/me", app.handleMe)
	mux.HandleFunc("GET /api/me/settings", app.handleGetSettings)
	mux.HandleFunc("PUT /api/me/settings", app.handleUpdateSettings)

	mux.HandleFunc("GET /api/users", app.requireAdmin(app.handleListUsers))
	mux.HandleFunc("POST /api/users/{id}/approve", app.requireAdmin(app.handleUserStatus(StatusApproved)))
	mux.HandleFunc("POST /api/users/{id}/reject", app.requireAdmin(app.handleUserStatus(StatusRejected)))
	mux.HandleFunc("POST /api/users/{id}/disable", app.requireAdmin(app.handleUserStatus(StatusDisabled)))
	mux.HandleFunc("DELETE /api/users/{id}", app.requireAdmin(app.handleDeleteUser))

	mux.HandleFunc("GET /api/catalog/pmm", app.handlePMMCatalog)
	mux.HandleFunc("GET /api/catalog/pxc", app.handlePXCCatalog)
	mux.HandleFunc("GET /api/catalog/proxysql", app.handleProxySQLCatalog)
	mux.HandleFunc("GET /api/catalog/ps", app.handlePSCatalog)
	mux.HandleFunc("GET /api/catalog/psmdb", app.handlePSMDBCatalog)
	mux.HandleFunc("GET /api/catalog/ppg", app.handlePPGCatalog)
	mux.HandleFunc("GET /api/catalog/spock", app.handleSpockCatalog)
	mux.HandleFunc("GET /api/catalog/images", app.handleImagesCatalog)
	mux.HandleFunc("GET /api/catalog/pdps", app.handlePDPSCatalog)

	mux.HandleFunc("GET /api/stacks", app.handleListStacks)
	mux.HandleFunc("POST /api/stacks", app.handleCreateStack)
	mux.HandleFunc("GET /api/stacks/{id}", app.handleGetStack)
	mux.HandleFunc("PUT /api/stacks/{id}", app.handleUpdateStack)
	mux.HandleFunc("DELETE /api/stacks/{id}", app.handleDeleteStack)
	mux.HandleFunc("POST /api/stacks/{id}/validate", app.handleValidateStack)
	mux.HandleFunc("POST /api/stacks/{id}/deploy", app.handleDeployStack)
	mux.HandleFunc("POST /api/stacks/{id}/destroy", app.handleDestroyStack)
	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}", app.handleGetNode)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/start", app.handleNodeAction("start"))
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/stop", app.handleNodeAction("stop"))
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/restart", app.handleNodeAction("restart"))

	// On-node diagnostic captures. pg_gather (PostgreSQL) + pt-stalk (MySQL family).
	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}/pggather", app.handlePGGatherStatus)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/pggather", app.handlePGGatherStart)
	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}/pggather/download", app.handlePGGatherDownload)
	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}/ptstalk", app.handlePTStalkStatus)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/ptstalk", app.handlePTStalkStart)
	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}/ptstalk/download", app.handlePTStalkDownload)

	// Visual Summary — parse a pt-stalk archive into timeline charts.
	mux.HandleFunc("POST /api/visualsummary/upload", app.handleVisualUpload)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/visualsummary", app.handleVisualNode)

	// Samba AD DC node management — LDAP users/groups, Kerberos principals/keytabs, TLS.
	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}/samba/users", app.handleSambaUsers)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/samba/users", app.handleSambaUserCreate)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/samba/users/update", app.handleSambaUserUpdate)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/samba/users/password", app.handleSambaUserPassword)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/samba/users/delete", app.handleSambaUserDelete)
	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}/samba/groups", app.handleSambaGroups)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/samba/groups", app.handleSambaGroupCreate)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/samba/groups/members", app.handleSambaGroupMembers)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/samba/groups/delete", app.handleSambaGroupDelete)
	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}/samba/krb5", app.handleSambaKrb5)
	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}/samba/targets", app.handleSambaTargets)
	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}/samba/principals", app.handleSambaPrincipals)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/samba/principals", app.handleSambaPrincipalCreate)
	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}/samba/keytab", app.handleSambaKeytab)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/samba/cert", app.handleSambaCert)

	// Data Generator — introspect + generate test data for PostgreSQL stack tables.
	mux.HandleFunc("GET /api/datagen/connections", app.handleDataGenConnections)
	mux.HandleFunc("GET /api/datagen/stacks/{id}/nodes/{nid}/databases", app.handleDataGenDatabases)
	mux.HandleFunc("GET /api/datagen/stacks/{id}/nodes/{nid}/tables", app.handleDataGenTables)
	mux.HandleFunc("GET /api/datagen/stacks/{id}/nodes/{nid}/columns", app.handleDataGenColumns)
	mux.HandleFunc("POST /api/datagen/stacks/{id}/nodes/{nid}/preview", app.handleDataGenPreview)
	mux.HandleFunc("POST /api/datagen/stacks/{id}/nodes/{nid}/generate", app.handleDataGenGenerate)
	mux.HandleFunc("GET /api/datagen/jobs/{job}", app.handleDataGenJob)
	mux.HandleFunc("POST /api/datagen/jobs/{job}/cancel", app.handleDataGenCancel)

	mux.HandleFunc("GET /api/queryrun/targets", app.handleQueryRunTargets)
	mux.HandleFunc("POST /api/queryrun/runs", app.handleQueryRunStart)
	mux.HandleFunc("GET /api/queryrun/runs/{id}", app.handleQueryRunStatus)
	mux.HandleFunc("POST /api/queryrun/runs/{id}/stop", app.handleQueryRunStop)
	mux.HandleFunc("GET /api/queryrun/history", app.handleQueryRunHistory)

	mux.HandleFunc("GET /api/benchmark/targets", app.handleBenchTargets)
	mux.HandleFunc("POST /api/benchmark/runs", app.handleBenchStart)
	mux.HandleFunc("GET /api/benchmark/runs/{id}", app.handleBenchStatus)
	mux.HandleFunc("POST /api/benchmark/runs/{id}/stop", app.handleBenchStop)
	mux.HandleFunc("GET /api/benchmark/history", app.handleBenchHistory)

	// Dashboard — cheap summary + focus-gated live OS stats.
	mux.HandleFunc("GET /api/dashboard/summary", app.handleDashboardSummary)
	mux.HandleFunc("GET /api/dashboard/stats", app.handleDashboardStats)

	// Notifications — bell + live SSE stream.
	mux.HandleFunc("GET /api/notifications", app.handleListNotifications)
	mux.HandleFunc("GET /api/notifications/stream", app.handleNotifStream)
	mux.HandleFunc("POST /api/notifications/read-all", app.handleMarkAllRead)
	mux.HandleFunc("POST /api/notifications/{id}/read", app.handleMarkNotificationRead)

	// Intranet node management (Phase 3) — all via docker exec into the container.
	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}/email/users", app.handleEmailList)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/email/users", app.emailMutate(emailAddScript, true))
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/email/users/password", app.emailMutate(emailPasswordScript, true))
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/email/users/delete", app.emailMutate(emailDeleteScript, false))

	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}/ldap/users", app.handleLdapUsers)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/ldap/users", app.ldapUserMutate(ldapUserCreateScript, false))
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/ldap/users/update", app.ldapUserMutate(ldapUserUpdateScript, false))
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/ldap/users/password", app.ldapUserMutate(ldapUserPasswordScript, true))
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/ldap/users/delete", app.ldapUserMutate(ldapUserDeleteScript, false))

	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}/ldap/groups", app.handleLdapGroups)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/ldap/groups", app.ldapGroupMutate(ldapGroupCreateScript, false))
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/ldap/groups/members", app.ldapGroupMutate(ldapGroupMembersScript, true))
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/ldap/groups/delete", app.ldapGroupMutate(ldapGroupDeleteScript, false))

	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}/cert", app.handleCertInfo)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/cert", app.handleCertGenerate)

	// Intranet CA — issue X.509 client certificates for MySQL/PostgreSQL/MongoDB users.
	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}/dbcerts", app.handleDBCertList)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/dbcerts", app.handleDBCertGenerate)
	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}/dbcerts/{user}", app.handleDBCertGet)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/dbcerts/delete", app.handleDBCertDelete)

	// PMM node management.
	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}/openbao/status", app.handleOpenBaoStatus)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/openbao/unseal", app.handleOpenBaoUnseal)

	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}/pmm/cert", app.handlePMMCertInfo)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/pmm/cert", app.handlePMMCertGenerate)

	// PXC node management.
	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}/pxc/cert", app.handlePXCCertInfo)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/pxc/cert", app.handlePXCCertGenerate)

	// PostgreSQL-family node management (standalone PostgreSQL, Patroni, repmgr, Spock).
	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}/pg/cert", app.handlePGCertInfo)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/pg/cert", app.handlePGCertGenerate)

	// MongoDB node management.
	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}/mongo/cert", app.handleMongoCertInfo)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/mongo/cert", app.handleMongoCertGenerate)

	// PXC cluster (frame) management — toggle PMM monitoring post-deploy.
	mux.HandleFunc("POST /api/stacks/{id}/frames/{fid}/pmm", app.handlePXCFrameMonitor)

	// Patroni cluster (frame) management — run an on-demand pgBackRest backup.
	mux.HandleFunc("POST /api/stacks/{id}/frames/{fid}/patroni/backup", app.handlePatroniBackup)

	// Standalone PostgreSQL node — run an on-demand pgBackRest backup.
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/pg/backup", app.handlePGBackup)

	// PS MongoDB cluster/replica-set (frame) — run an on-demand PBM backup.
	mux.HandleFunc("POST /api/stacks/{id}/frames/{fid}/pbm/backup", app.handleMongoPBMBackup)

	// repmgr cluster (frame) — run an on-demand Barman cloud backup on the primary.
	mux.HandleFunc("POST /api/stacks/{id}/frames/{fid}/barman/backup", app.handleRepmgrBackup)

	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}/term", app.handleNodeTerminal)

	app.startReaper()

	mux.Handle("/", spaHandler())

	host := envOr("APP_HOST", "127.0.0.1")
	port := envOr("APP_PORT", "8080")
	addr := net.JoinHostPort(host, port)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("DBCanvas listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

// spaHandler serves embedded static files, falling back to index.html for
// client-side routes.
func spaHandler() http.Handler {
	dist, err := fs.Sub(embeddedFS, "web/dist")
	if err != nil {
		log.Fatalf("failed to locate embedded SPA: %v", err)
	}
	fileServer := http.FileServer(http.FS(dist))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if f, err := dist.Open(p); err == nil {
			f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}
		// Unknown path → serve the SPA entrypoint for client routing.
		index, err := dist.Open("index.html")
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		defer index.Close()
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.Copy(w, index)
	})
}

// healthcheck performs GET /api/setup/status against localhost and returns an
// exit code.
func healthcheck() int {
	port := envOr("APP_PORT", "8080")
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/api/setup/status")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// deployTimeout is how long a provisioner waits for a dependency (a cluster,
// node, or shared service) to become ready before giving up. Configurable via
// DEPLOYMENT_TIMEOUT (in minutes); defaults to 60. Large stacks with many
// containers routinely need well over the old fixed 15m before everything is up.
func deployTimeout() time.Duration {
	if v := strings.TrimSpace(os.Getenv("DEPLOYMENT_TIMEOUT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return 60 * time.Minute
}

// --- JSON helpers ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if v != nil {
		json.NewEncoder(w).Encode(v)
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decode(r *http.Request, v any) error {
	defer io.Copy(io.Discard, r.Body)
	return json.NewDecoder(r.Body).Decode(v)
}
