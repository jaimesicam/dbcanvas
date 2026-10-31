package main

import (
	"fmt"
	"html"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The licence texts the image ships under /usr/share/doc/dbcanvas (app/Dockerfile),
// served at /licenses/ without a session.
//
// The image carries them for whoever runs it, but two other audiences receive
// DBCanvas code without ever opening the image: every browser that loads the web
// interface gets React, xterm, rrweb and the fonts (MIT, ISC, BSD, OFL — each wants
// its notice to travel with the copy), and everyone who downloads dbcanvas-cli from
// the API page gets a GPL binary plus three BSD/ISC modules. /licenses/ is where both
// are pointed: the JS bundles' banner comment (web/vite.config.js), the Dashboard's
// Licences link, and the CLI download section all lead here. It is public because
// the login screen is a user interface too, and a notice behind a login is not one
// a recipient can read.
//
// The index page is GPLv3's "Appropriate Legal Notices" (§0, §5d): copyright, no
// warranty, the licence, and where the Corresponding Source is (§6d).

// sourceRepo is the public repository the Corresponding Source is offered from.
const sourceRepo = "https://github.com/jaimesicam/dbcanvas"

// licenseDir is where the image puts the texts. Overridable for a development
// server, which has none (the generators run at image build); /licenses/ then
// shows the notice with no files rather than failing.
func licenseDir() string { return envOr("DBCANVAS_LICENSE_DIR", "/usr/share/doc/dbcanvas") }

// licenseFiles lists the texts present, LICENSE first.
func licenseFiles(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i] == "LICENSE") != (out[j] == "LICENSE") {
			return out[i] == "LICENSE"
		}
		return out[i] < out[j]
	})
	return out
}

var licenseBlurb = map[string]string{
	"LICENSE":                      "DBCanvas itself — GNU General Public License v3.0",
	"THIRD_PARTY_NOTICES.md":       "Summary of every third-party component and its licence",
	"THIRD_PARTY_LICENSES-web.txt": "npm packages and fonts in this web interface",
	"THIRD_PARTY_LICENSES-go.txt":  "Go modules in the server and dbcanvas_reset_password",
	"THIRD_PARTY_LICENSES-cli.txt": "Go modules in dbcanvas-cli (the downloads on the API page)",
	"k3d.LICENSE":                  "k3d, bundled for K3D clusters (MIT)",
}

func handleLicenses(w http.ResponseWriter, r *http.Request) {
	dir := licenseDir()
	name := strings.TrimPrefix(r.URL.Path, "/licenses/")
	if name != "" {
		// Only a file the index would list: no paths, no dotfiles.
		if strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") {
			http.NotFound(w, r)
			return
		}
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			http.NotFound(w, r)
			return
		}
		// Plain text whatever the extension: LICENSE has none, and the .md is read, not rendered.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeFile(w, r, p)
		return
	}

	// The repository root, not a /tree/<version> link: releases are not tagged, and a
	// link that 404s is no offer of source at all.
	ref := sourceRepo
	var b strings.Builder
	fmt.Fprintf(&b, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>DBCanvas licences</title>
<style>
:root{color-scheme:light dark;--fg:#1f2430;--muted:#5d6475;--bg:#f7f8fb;--line:#d9dde6;--link:#4f46e5}
@media (prefers-color-scheme:dark){:root{--fg:#e6e8ef;--muted:#9aa1b2;--bg:#0d1017;--line:#262b38;--link:#a5b4fc}}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.6 system-ui,sans-serif}
main{max-width:720px;margin:0 auto;padding:40px 16px}
h1{font-size:22px;margin:0 0 4px}p{margin:0 0 14px}.m{color:var(--muted)}
a{color:var(--link)}ul{list-style:none;padding:0;margin:20px 0;border-top:1px solid var(--line)}
li{padding:10px 0;border-bottom:1px solid var(--line)}code{font-size:13px}
</style></head><body><main>
<h1>DBCanvas %s</h1>
<p class="m">Copyright © 2026 Jaime Sicam</p>
<p>DBCanvas is free software: you can redistribute it and/or modify it under the terms of the
GNU General Public License, version 3 only, as published by the Free Software Foundation.</p>
<p>It is distributed in the hope that it will be useful, but <strong>without any warranty</strong>;
without even the implied warranty of merchantability or fitness for a particular purpose.
See the licence below for details.</p>
<p>Source code: <a href="%s">%s</a></p>
<ul>`, html.EscapeString(appVersion), html.EscapeString(ref), html.EscapeString(ref))
	files := licenseFiles(dir)
	for _, f := range files {
		fmt.Fprintf(&b, `<li><a href="/licenses/%s"><code>%s</code></a><br><span class="m">%s</span></li>`,
			html.EscapeString(f), html.EscapeString(f), html.EscapeString(licenseBlurb[f]))
	}
	if len(files) == 0 {
		fmt.Fprintf(&b, `<li class="m">The licence texts are generated when the container image is built; this server was not
started from one. They are in the repository: <a href="%s/blob/main/LICENSE">LICENSE</a> and
<a href="%s/blob/main/THIRD_PARTY_NOTICES.md">THIRD_PARTY_NOTICES.md</a>.</li>`, sourceRepo, sourceRepo)
	}
	b.WriteString(`</ul><p class="m">Each third-party component remains under its own licence; none is relicensed by being included.</p></main></body></html>`)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}
