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
	"strings"
	"time"
)

//go:embed all:web/dist
var embeddedFS embed.FS

// App holds shared dependencies for the HTTP handlers.
type App struct {
	store  *Store
	docker *Docker
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

	mux.HandleFunc("GET /api/users", app.requireAdmin(app.handleListUsers))
	mux.HandleFunc("POST /api/users/{id}/approve", app.requireAdmin(app.handleUserStatus(StatusApproved)))
	mux.HandleFunc("POST /api/users/{id}/reject", app.requireAdmin(app.handleUserStatus(StatusRejected)))
	mux.HandleFunc("POST /api/users/{id}/disable", app.requireAdmin(app.handleUserStatus(StatusDisabled)))
	mux.HandleFunc("DELETE /api/users/{id}", app.requireAdmin(app.handleDeleteUser))

	mux.HandleFunc("GET /api/catalog/pmm", app.handlePMMCatalog)

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

	// PMM node management.
	mux.HandleFunc("GET /api/stacks/{id}/nodes/{nid}/pmm/cert", app.handlePMMCertInfo)
	mux.HandleFunc("POST /api/stacks/{id}/nodes/{nid}/pmm/cert", app.handlePMMCertGenerate)

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
