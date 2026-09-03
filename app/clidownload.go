package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// clidownload.go — hand out the `dbcanvas-cli` binary the app image was built with.
//
// This exists for one audience: somebody using a DBCanvas that runs on a shared
// server, who has no checkout to run `make cli` in. Telling them to go and clone the
// repository to get a client for the thing they are already logged into would be a
// poor answer, so the image carries the binaries and the API page links to them.
//
// The Dockerfile builds them into /usr/local/share/dbcanvas/cli/. A local
// development server has no such directory, and this endpoint then says so rather
// than 404ing mysteriously — `make cli` is the answer there.

// cliDir is where the image keeps the cross-compiled binaries. Overridable so a
// developer can point it at ../dist and try the download path.
func cliDir() string {
	if v := strings.TrimSpace(os.Getenv("CLI_DIR")); v != "" {
		return v
	}
	return "/usr/local/share/dbcanvas/cli"
}

// cliPlatforms is what the Dockerfile builds, and therefore what the API page may
// offer. A closed list, not a directory listing: the os/arch pair comes from the
// query string, and resolving user input against a fixed table is what keeps it from
// becoming a path.
var cliPlatforms = []struct{ OS, Arch string }{
	{"linux", "amd64"},
	{"linux", "arm64"},
	{"darwin", "amd64"},
	{"darwin", "arm64"},
	{"windows", "amd64"},
}

// cliBinaryName is the file name for a platform, matching what `make cli` produces.
func cliBinaryName(goos, arch string) string {
	name := "dbcanvas-cli_" + goos + "_" + arch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

func (a *App) handleCLIDownload(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentUser(r); !ok {
		writeErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	goos := r.URL.Query().Get("os")
	arch := r.URL.Query().Get("arch")
	if arch == "" {
		arch = "amd64"
	}
	known := false
	for _, p := range cliPlatforms {
		if p.OS == goos && p.Arch == arch {
			known = true
			break
		}
	}
	if !known {
		// Naming what is available beats "bad request": the caller is usually a
		// person editing a URL by hand.
		var names []string
		for _, p := range cliPlatforms {
			names = append(names, p.OS+"/"+p.Arch)
		}
		writeErr(w, http.StatusBadRequest,
			"unknown platform; this installation has "+strings.Join(names, ", "))
		return
	}

	name := cliBinaryName(goos, arch)
	path := filepath.Join(cliDir(), name)
	f, err := os.Open(path)
	if err != nil {
		writeErr(w, http.StatusNotFound,
			"this installation does not ship the CLI binaries — build one with `make cli` from a checkout")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		writeErr(w, http.StatusNotFound, "the CLI binary for that platform is not available here")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	// ServeContent rather than io.Copy: it handles Range requests and conditional
	// GETs, which matters for a 15 MB file on a slow link.
	http.ServeContent(w, r, name, st.ModTime(), f)
}
