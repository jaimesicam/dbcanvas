# SCAFFOLD.md — Project Blueprint

This file is a complete, self-contained specification for an AI coding tool
(Claude Code, Cursor, Codex, etc.) to **rebuild this project from scratch with
100% functional parity**. Build everything described here, in the structure
described here, with the exact contracts, tokens, and algorithms given.

> If a detail isn't pinned down below, prefer the simplest implementation that
> satisfies the described behavior. Do **not** add libraries beyond those listed.

---

## 0. Naming substitution (do this first)

This project was authored under the name **`richui`**. When rebuilding, derive
the app name from the **name of the directory that contains this `SCAFFOLD.md`**:

- `APP_SLUG` = the directory's basename, lowercased, non-alphanumerics → `-`
  (e.g. directory `My App/` → `my-app`). Used for: Go module path,
  `package.json` "name", Docker image name, the compiled binary name, SQLite
  filename, cookie name (hyphens → underscores), `localStorage` key.
- `APP_NAME` = a human display name (Title Case of `APP_SLUG`, or keep the
  directory's original casing). Used in UI brand text and the document title.
- `APP_GLYPH` = first character of `APP_NAME`, uppercased. Used as the square
  logo letter.

Then do a **global, case-aware find/replace across the entire generated tree**.
This is a blanket rule, **not** an allowlist — replace *every* occurrence, in
code, comments, configs, and docs:

| Token (case-sensitive) | Replace with | Transform |
| ---------------------- | ------------ | --------- |
| `richui`               | `APP_SLUG`   | lowercase slug |
| `RichUI`               | `APP_NAME`   | display name |
| `R` **as the logo glyph only** | `APP_GLYPH` | first letter of `APP_NAME`, uppercased |

After replacing, **grep the whole project for `richui` / `RichUI` (case-insensitive)
and confirm zero matches** (except this file). For reference, the original name
appears in all of these places — none may be missed:

- **Identifiers / slug** (`richui` → `APP_SLUG`): `go.mod` module; `package.json`
  `"name"`; compose `image:`; **the compiled binary name** in `app/Dockerfile`
  (`go build -o /richui`, `COPY /richui`, `ENTRYPOINT ["/richui"]`); the SQLite
  filename `richui.db` (Dockerfile `DB_PATH`, compose `DB_PATH`, `main.go` default,
  README dev command); the session cookie `richui_session` (`auth.go`, with
  `-`→`_`); the `localStorage` key `richui-theme` (`theme/ThemeProvider.jsx`); the
  seed email domain `@richui.dev` (`pages/DataTable.jsx`).
- **Display brand** (`RichUI` → `APP_NAME`): `index.html` `<title>`; sidebar brand
  (`App.jsx`); both auth-screen titles (`auth/AuthScreens.jsx`); the `main.go` log
  line `"RichUI listening …"`; the `Makefile` echo; `README.md`.
- **Logo glyph** (`R` → `APP_GLYPH`): the square logo in the sidebar and on the
  auth screens.

> **Consistency invariant:** the binary name must be identical in `Dockerfile`
> (`-o`, `COPY`, `ENTRYPOINT`) **and** the compose `healthcheck`
> (`["CMD","/<APP_SLUG>","-healthcheck"]`). If these drift, the container health
> check fails. Nothing else in the app depends on the literal name — it is purely
> identifiers and cosmetics — so a clean global replace is safe.

Everything below shows the **original** literals; apply the substitution as you write.

---

## 0.5 Versioning policy (read before installing anything)

**Goal: maximize new features and bug fixes.** This project should be rebuilt on
the **newest stable releases**, not the versions that happened to exist when this
file was written. Freshness is intentionally prioritized over byte-for-byte
reproducibility.

**Do this at generation time:**
1. For every dependency and base image, **resolve to the latest stable release**,
   bumping the **major** version if a newer stable major exists. Concretely:
   - npm: install with `@latest` (e.g. `npm install react@latest react-dom@latest`
     and `-D vite@latest @vitejs/plugin-react@latest tailwindcss@latest
     @tailwindcss/vite@latest`) so the manifest captures current majors.
   - Go: `go get <module>@latest` for each require, set the `go` directive to the
     installed toolchain's version, then `go mod tidy`.
   - Docker: use current tags — Node **current LTS** (`node:<lts>-alpine`), the
     latest `golang:<minor>-alpine`, and `gcr.io/distroless/static-debian12:latest`.
2. **Then build and run the acceptance checklist (§11).** A newer major may
   change an API — adapt the code to the new version rather than pinning back.
   (Likely watch-items: a React major's StrictMode/ref/effect timing — this app
   leans on pointer events + effects in the node editor; a Vite or Tailwind major's
   config surface.)
3. **Commit lockfiles in the *generated* project** (`package-lock.json`,
   `go.sum`) so that one instance is reproducible — but **do not** treat this
   scaffold's version numbers as targets.

**Version numbers shown later in this file are illustrative floors, not pins.**
Read every `^x.y.z`, `go 1.x`, and image tag below as **"≥ this major, but install
the newest stable."** The only hard rules: stay on these **major lines or newer**
— React ≥18, Vite ≥6, Tailwind ≥4 (CSS-first, no `tailwind.config.js`), Go ≥1.23,
Node ≥ current LTS — and **never use pre-releases** (no alpha/beta/RC, no
non-LTS Node). Newer stable is always preferred.

---

## 1. What this is

A **professional single-page web app** that doubles as a UI-interaction lab,
served by a Go backend that adds **authentication + user management**. It ships
as **one small (~22 MB) Docker container**: the Go binary serves the embedded
React SPA *and* the JSON API on one port and stores users/sessions in SQLite on
a persistent volume. No separate web/api/db containers.

```
browser ──HTTP──> Go binary (:APP_PORT)
                  ├─ serves embedded React SPA (//go:embed)
                  └─ /api/*  ──> SQLite (/data, Docker volume)
```

Run model: `make compose` → build image → start container → open
`http://localhost:8080`. First visit asks you to create an administrator.

---

## 2. Tech stack (latest stable of each — see §0.5; add nothing else)

> Install the **newest stable** of every item below. The versions/tags named are
> **floors**, not pins.

**Frontend**
- React + ReactDOM (**≥18**; no router — custom hash navigation)
- Vite (**≥6**, `@vitejs/plugin-react`)
- Tailwind CSS (**≥4**) via `@tailwindcss/vite` (CSS-first config; no `tailwind.config.js`)
- **Zero** UI/icon/graph/state libraries — icons are hand-written inline SVG,
  the node editor and all widgets are built from scratch.

**Backend**
- Go (**≥1.23**, built with the latest `golang:<minor>-alpine`)
- `modernc.org/sqlite` — **pure-Go** SQLite (no CGO; enables a static binary)
- `golang.org/x/crypto/bcrypt`
- Standard library `net/http` with Go 1.22+ method+wildcard mux patterns; SPA
  embedded with `//go:embed`.

**Runtime / infra**
- Final image: `gcr.io/distroless/static-debian12` (single static binary)
- Build SPA with Node **current LTS** (`node:<lts>-alpine`)
- `docker compose` (one service), named volume for `/data`
- `Makefile` front door, `.env` for config.

---

## 3. Final directory tree

```
.
├── .env.example
├── .gitignore
├── Makefile
├── README.md
├── SCAFFOLD.md
├── docker-compose.yml
└── app/
    ├── .dockerignore
    ├── Dockerfile
    ├── go.mod
    ├── go.sum                 # generated by `go mod tidy`
    ├── main.go                # server, embedded SPA, routing, healthcheck flag
    ├── store.go               # SQLite store: users + sessions
    ├── auth.go                # setup/register/login/logout/sessions (bcrypt cookies)
    ├── users.go               # admin user-management handlers
    └── web/                   # React + Vite frontend (built & embedded at image build)
        ├── .dockerignore
        ├── index.html
        ├── package.json
        ├── package-lock.json  # generated by `npm install`
        ├── vite.config.js
        └── src/
            ├── main.jsx        # React root → ThemeProvider → AuthProvider → Root
            ├── Root.jsx        # auth gate: loading | setup | anon | authed
            ├── App.jsx         # authed app shell (sidebar, topbar, palette, account menu)
            ├── index.css       # Tailwind import + theme tokens + keyframes
            ├── auth/
            │   ├── AuthProvider.jsx   # context: phase, user, setup/login/register/logout
            │   └── AuthScreens.jsx    # Splash, SetupScreen, AuthScreen
            ├── theme/
            │   └── ThemeProvider.jsx  # theme context + THEMES list
            ├── lib/
            │   └── api.js             # fetch wrapper + endpoints
            ├── components/
            │   ├── Icons.jsx          # inline-SVG icon set
            │   └── ui.jsx             # Card, Button, Badge, Toggle, Field, inputCls
            └── pages/
                ├── Dashboard.jsx
                ├── Controls.jsx
                ├── NodeEditorFrames.jsx
                ├── DataTable.jsx
                ├── Kanban.jsx
                └── ManageUsers.jsx
```

---

## 4. Root files (exact contents)

### `.env.example`
```ini
# Host interface the application is reachable on. Default: 127.0.0.1 (private —
# this machine only).
# - 127.0.0.1 (or another IPv4, e.g. 192.168.1.10) restricts access to that
#   interface. In Docker this binds the published port to that host interface;
#   the container itself always listens on all interfaces internally.
# - 0.0.0.0 exposes the app on all interfaces (e.g. your LAN).
APP_HOST=127.0.0.1

# Port the application listens on (host + container). Default: 8080
APP_PORT=8080

# Target platform for the Docker image build. Default: linux/amd64
DOCKER_PLATFORM=linux/amd64
```

### `.gitignore`
```gitignore
# Environment / secrets
.env

# Node
node_modules/
dist/
*.log

# Go
/app/*.db
/app/*.db-*

# OS
.DS_Store
```

### `docker-compose.yml`
```yaml
services:
  app:
    build:
      context: ./app
      dockerfile: Dockerfile
    image: richui:latest
    platform: ${DOCKER_PLATFORM:-linux/amd64}
    environment:
      # The container always listens on all interfaces internally (the
      # Dockerfile sets APP_HOST=0.0.0.0); host-side exposure is controlled by
      # the publish binding below, not by this value.
      APP_PORT: ${APP_PORT:-8080}
      DB_PATH: /data/richui.db
    ports:
      # Publish on APP_HOST (default 127.0.0.1 = private). Set APP_HOST=0.0.0.0
      # in .env to expose on all host interfaces.
      - "${APP_HOST:-127.0.0.1}:${APP_PORT:-8080}:${APP_PORT:-8080}"
    volumes:
      - app-data:/data            # SQLite database persists across rebuilds
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "/richui", "-healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 5s

volumes:
  app-data:
```

### `Makefile`
```make
SHELL := /bin/bash

# Load APP_PORT for echoing the URL (falls back to 8080).
APP_PORT ?= $(shell test -f .env && grep -E '^APP_PORT=' .env | cut -d= -f2 || echo 8080)

.PHONY: compose env build up down logs restart clean

## compose: create .env if needed, then build and start the stack
compose: env
	docker compose up --build -d
	@echo ""
	@echo "  richui is up → http://localhost:$(APP_PORT)"
	@echo "  View logs:    make logs"
	@echo "  Stop:         make down"

## env: materialize .env from .env.example (only if missing)
env:
	@test -f .env || { cp .env.example .env && echo "Created .env from .env.example"; }

## build: build the image only
build: env
	docker compose build

## up: start containers (no rebuild)
up: env
	docker compose up -d

## down: stop and remove containers
down:
	docker compose down

## restart: recreate the stack
restart: down compose

## logs: follow application logs
logs:
	docker compose logs -f

## clean: stop stack and remove the built image
clean:
	docker compose down --rmi local --remove-orphans
```

---

## 5. `app/` — Docker & Go backend

### `app/Dockerfile`
```dockerfile
# syntax=docker/dockerfile:1

# ---- stage 1: build the React SPA ----
FROM node:22-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build      # → /web/dist

# ---- stage 2: build the Go server with the SPA embedded ----
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY --from=web /web/dist ./web/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /richui .

# ---- stage 3: minimal runtime (single static binary) ----
FROM gcr.io/distroless/static-debian12 AS runtime
COPY --from=build /richui /richui
ENV APP_HOST=0.0.0.0
ENV APP_PORT=8080
ENV DB_PATH=/data/richui.db
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/richui"]
```

### `app/.dockerignore`
```
web/node_modules
web/dist
**/*.log
.env
```

### `app/web/.dockerignore`
```
node_modules
dist
```

### `app/go.mod`
> Per §0.5, don't copy these versions — fetch latest:
> `go get golang.org/x/crypto@latest modernc.org/sqlite@latest`, set the `go`
> directive to your installed toolchain, then `go mod tidy`.
```
module richui

go 1.23                      # floor — use your installed toolchain's version

require (
	golang.org/x/crypto v0.53.0    # floor — resolve to latest
	modernc.org/sqlite v1.52.0     # floor — resolve to latest
)
```
> `go mod tidy` populates indirect deps + `go.sum`. (modernc pulls
> `go-humanize`, `uuid`, `go-isatty`, `go-strftime`, `bigfft`, `x/sys`,
> `modernc.org/{libc,mathutil,memory}` as indirect.)

### Backend behavior — file by file

**`store.go` — SQLite store.** Open with `sql.Open("sqlite", path)` (import
`_ "modernc.org/sqlite"`). `SetMaxOpenConns(1)`; on open run pragmas
`journal_mode=WAL`, `foreign_keys=ON`, `busy_timeout=5000`, then create schema:

```sql
CREATE TABLE IF NOT EXISTS users (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  username      TEXT UNIQUE NOT NULL,
  password_hash TEXT NOT NULL,
  role          TEXT NOT NULL,      -- 'admin' | 'user'
  status        TEXT NOT NULL,      -- 'pending' | 'approved' | 'rejected' | 'disabled'
  created_at    TEXT NOT NULL,      -- RFC3339
  approved_at   TEXT                -- RFC3339, nullable
);
CREATE TABLE IF NOT EXISTS sessions (
  token      TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  expires_at TEXT NOT NULL          -- RFC3339
);
```

`User` JSON shape: `{id, username, role, status, createdAt, approvedAt?}` (omit
`approvedAt` when null; never serialize the hash). Constants: roles
`admin`/`user`; statuses `approved`/`pending`/`rejected`/`disabled`. Store methods:
`CountUsers`, `CreateUser(username,hash,role,status)` (sets `approved_at=now`
when status is approved; maps a UNIQUE violation to a sentinel `ErrUserExists`),
`GetUser(id)`, `CredByUsername` (returns user **plus** hash), `ListUsers`
(pending first, then newest first), `SetStatus(id,status)` (sets `approved_at`
when approving), `DeleteUser`, `CreateSession`, `SessionUser(token)` (returns
user for a valid, unexpired token; deletes expired), `DeleteSession`,
`DeleteUserSessions(userID)`.

**`auth.go` — auth + sessions.**
- Cookie name `richui_session`. `issueSession`: 32 random bytes hex token →
  `CreateSession` with TTL → set cookie `{HttpOnly, SameSite=Lax,
  Secure: r.TLS != nil, Path:/, MaxAge/Expires = TTL}`. TTL = **7 days**.
- `currentUser(r)` reads the cookie → `SessionUser`.
- `requireAdmin(next)` middleware → 401 if no user, 403 if not admin.
- Password rules (`credentials.validate`): username 3–32 chars, password ≥ 8.
- bcrypt `DefaultCost` for hashing/compare.

**`users.go` — admin handlers.** `handleListUsers`; `handleUserStatus(status)`
returns a handler that parses `{id}`, **refuses to change your own status**
unless approving, calls `SetStatus`, and **revokes that user's sessions** when
the new status is `disabled`/`rejected`; `handleDeleteUser` refuses to delete
your own account. `pathID` parses `r.PathValue("id")`.

**`main.go` — server.** `//go:embed all:web/dist` → serve via `fs.Sub`. Read
env `APP_HOST` (default `127.0.0.1`), `APP_PORT` (default 8080) and `DB_PATH`
(default `richui.db`). Listen on `net.JoinHostPort(APP_HOST, APP_PORT)` — the
default `127.0.0.1` keeps a bare `go run` private to the machine; an IPv4
address binds that interface only, `0.0.0.0` binds all interfaces. (In the
container the Dockerfile sets `APP_HOST=0.0.0.0` so the published port is
reachable; host-side privacy there is handled by the compose publish binding.)
If
`os.Args[1] == "-healthcheck"`, GET `/api/setup/status` on localhost and exit
0/1 (used by the container HEALTHCHECK — distroless has no shell). Routes (Go
1.22 mux):

```
GET  /api/setup/status      handleStatus
POST /api/setup             handleSetup
POST /api/auth/register     handleRegister
POST /api/auth/login        handleLogin
POST /api/auth/logout       handleLogout
GET  /api/me                handleMe
GET    /api/users           requireAdmin(handleListUsers)
POST   /api/users/{id}/approve   requireAdmin(handleUserStatus("approved"))
POST   /api/users/{id}/reject    requireAdmin(handleUserStatus("rejected"))
POST   /api/users/{id}/disable   requireAdmin(handleUserStatus("disabled"))
DELETE /api/users/{id}           requireAdmin(handleDeleteUser)
"/"  → SPA handler (serve file if it exists, else index.html for client routes)
```

JSON helpers: `writeJSON(w,code,v)`, `writeErr(w,code,msg)` → `{"error": msg}`,
`decode(r,&v)`.

### API contract (the SPA depends on exactly this)

| Method/Path | Auth | Body | Behavior |
|---|---|---|---|
| `GET /api/setup/status` | — | — | `{initialized: bool, authenticated: bool, user: User|null}`. `initialized` = (user count > 0). |
| `POST /api/setup` | only when 0 users | `{username,password}` | Creates first user as `admin`/`approved`, issues session, 201 `User`. 409 if already initialized. |
| `POST /api/auth/register` | — | `{username,password}` | Creates `user`/`pending`. 201 `{status:"pending", message}`. 409 on dup username. |
| `POST /api/auth/login` | — | `{username,password}` | 401 invalid creds; 403 if pending ("awaiting approval") / rejected / disabled; else issue session, 200 `User`. |
| `POST /api/auth/logout` | — | — | Delete session + clear cookie, 200. |
| `GET /api/me` | session | — | 200 `User` or 401. |
| `GET /api/users` | admin | — | 200 `User[]` (pending first). |
| `POST /api/users/{id}/approve\|reject\|disable` | admin | — | 200 updated `User`. 400 if changing **own** status (non-approve). Revokes sessions on disable/reject. |
| `DELETE /api/users/{id}` | admin | — | 200 `{status:"deleted"}`. 400 if **own** id. |

**Security invariants:** bcrypt hashing; httpOnly session cookie (no tokens in
JS); setup self-locks once any user exists; every admin route enforced
server-side (hidden menu is convenience only); admin can't disable/delete self.

---

## 6. `app/web/` — frontend

### `index.html`
Standard Vite root: `<div id="root">`, `<script type="module" src="/src/main.jsx">`,
`<title>RichUI — Interaction Lab</title>`.

### `package.json`
> Per §0.5, don't hand-copy these ranges — let the installer write current ones:
> `npm install react@latest react-dom@latest` and
> `npm install -D vite@latest @vitejs/plugin-react@latest tailwindcss@latest @tailwindcss/vite@latest`.
> The ranges below are **floors** for reference only.
```json
{
  "name": "richui",
  "private": true,
  "version": "0.1.0",
  "type": "module",
  "scripts": { "dev": "vite", "build": "vite build", "preview": "vite preview" },
  "dependencies": { "react": "^18.3.1", "react-dom": "^18.3.1" },
  "devDependencies": {
    "@tailwindcss/vite": "^4.1.3",
    "@vitejs/plugin-react": "^4.3.4",
    "tailwindcss": "^4.1.3",
    "vite": "^6.0.7"
  }
}
```

### `vite.config.js`
React + Tailwind plugins; `server: { host: true, port: 5173, proxy: { '/api':
'http://localhost:8080' } }`. (Dev proxy lets `npm run dev` reach a locally-run
`go run .`; in the container the Go binary serves both, so no proxy is used.)

### `src/index.css` — Tailwind v4 + theme tokens (use verbatim)

This is **critical** to visual parity. Import Tailwind, expose semantic vars to
utilities via `@theme inline`, then define six themes by redefining the vars on
`[data-theme]`. Reproduce exactly:

```css
@import "tailwindcss";

@theme inline {
  --color-bg: var(--bg);
  --color-surface: var(--surface);
  --color-surface2: var(--surface2);
  --color-border: var(--border);
  --color-fg: var(--fg);
  --color-muted: var(--muted);
  --color-primary: var(--primary);
  --color-primary-fg: var(--primary-fg);
  --color-accent: var(--accent);
  --color-success: var(--success);
  --color-warning: var(--warning);
  --color-danger: var(--danger);
  --color-grid: var(--grid);
  --font-sans: "Inter", ui-sans-serif, system-ui, sans-serif;
  --font-mono: "JetBrains Mono", ui-monospace, "SFMono-Regular", monospace;
}

:root, [data-theme="light"] {
  --bg:#f4f6fb; --surface:#ffffff; --surface2:#eef1f7; --border:#d8dee9;
  --fg:#1b2233; --muted:#65728c; --primary:#4f46e5; --primary-fg:#ffffff;
  --accent:#0ea5e9; --success:#16a34a; --warning:#d97706; --danger:#dc2626; --grid:#dde3ee;
}
[data-theme="dark"] {
  --bg:#0e1117; --surface:#161b24; --surface2:#1f2632; --border:#2b3340;
  --fg:#e6eaf2; --muted:#93a0b5; --primary:#6366f1; --primary-fg:#ffffff;
  --accent:#22d3ee; --success:#22c55e; --warning:#f59e0b; --danger:#ef4444; --grid:#1c2430;
}
[data-theme="midnight"] {
  --bg:#070b1a; --surface:#0e1530; --surface2:#16204a; --border:#243056;
  --fg:#dde6ff; --muted:#8094c8; --primary:#7c5cff; --primary-fg:#ffffff;
  --accent:#2dd4bf; --success:#34d399; --warning:#fbbf24; --danger:#fb7185; --grid:#131c3d;
}
[data-theme="solarized"] {
  --bg:#fdf6e3; --surface:#fbf1d3; --surface2:#f4e9c8; --border:#e6dab4;
  --fg:#586e75; --muted:#93a1a1; --primary:#268bd2; --primary-fg:#fdf6e3;
  --accent:#2aa198; --success:#859900; --warning:#b58900; --danger:#dc322f; --grid:#eee3c2;
}
[data-theme="synthwave"] {
  --bg:#1a1030; --surface:#241546; --surface2:#321a63; --border:#46248c;
  --fg:#ffe9ff; --muted:#b88fd6; --primary:#ff2e97; --primary-fg:#ffffff;
  --accent:#00e5ff; --success:#2bff88; --warning:#ffcf3f; --danger:#ff5c5c; --grid:#2c1857;
}
[data-theme="forest"] {
  --bg:#0c1410; --surface:#122019; --surface2:#1a2e23; --border:#244234;
  --fg:#e3f3e8; --muted:#8fb39b; --primary:#2fae66; --primary-fg:#06170d;
  --accent:#7dd3a8; --success:#4ade80; --warning:#eab308; --danger:#f87171; --grid:#163025;
}

html, body, #root { height: 100%; }
body { margin:0; background:var(--bg); color:var(--fg); font-family:var(--font-sans); -webkit-font-smoothing:antialiased; }
* { border-color: var(--border); }

/* slim themed scrollbars (::-webkit-scrollbar track transparent, thumb var(--surface2) w/ 2px var(--bg) border, hover var(--muted)) */

@keyframes fade-in { from { opacity:0; transform:translateY(6px) } to { opacity:1; transform:translateY(0) } }
.animate-fade-in { animation: fade-in .25s ease both; }
@keyframes pulse-ring {
  0% { box-shadow: 0 0 0 0 color-mix(in srgb, var(--primary) 55%, transparent); }
  70% { box-shadow: 0 0 0 10px transparent; }
  100% { box-shadow: 0 0 0 0 transparent; }
}
.pulse-ring { animation: pulse-ring 1.8s infinite; }
```

Because of `@theme inline`, Tailwind utilities like `bg-bg`, `bg-surface`,
`text-fg`, `text-muted`, `border-border`, `bg-primary`, `text-primary`,
`bg-accent`, `text-success/warning/danger`, and `var(--grid)` all resolve to the
active theme's variables. Use these utility names throughout the UI; also
`bg-primary/15`, `text-danger`, etc. work (opacity modifiers).

### `src/main.jsx`
Render `<React.StrictMode><ThemeProvider><AuthProvider><Root/></AuthProvider></ThemeProvider></React.StrictMode>`
into `#root`; import `./index.css`.

### `src/theme/ThemeProvider.jsx`
Export `THEMES` array of `{id,label,swatch}`:
`light #4f46e5`, `dark #6366f1`, `midnight #7c5cff`, `solarized #268bd2`,
`synthwave #ff2e97`, `forest #2fae66`. Context holds `{theme,setTheme,themes}`.
On mount/change: `document.documentElement.setAttribute('data-theme', theme)` and
persist to `localStorage['richui-theme']`; default `dark`. `useTheme()` hook.

### `src/lib/api.js`
`fetch` wrapper (same-origin, `Content-Type: application/json`, cookies ride
automatically). Throws `Error` with `.status` on non-2xx (`error` field as
message). Methods: `status()`, `setup(u,p)`, `register(u,p)`, `login(u,p)`,
`logout()`, `me()`, `listUsers()`, `setUserStatus(id,action)` →
`POST /api/users/{id}/{action}`, `deleteUser(id)`.

### `src/auth/AuthProvider.jsx`
Context with `phase ∈ {loading, setup, anon, authed}` + `user`. On mount call
`api.status()`: `!initialized` → `setup`; `authenticated` → `authed` (+user);
else `anon`; on error → `anon`. Exposes `refresh()`, and async `setup/login`
(call api then `refresh`), `register` (returns api result, no refresh), `logout`
(api then refresh). `useAuth()` hook.

### `src/Root.jsx`
Gate by phase: `loading`→`<Splash/>`, `setup`→`<SetupScreen/>`,
`anon`→`<AuthScreen/>`, else `<App/>`.

### `src/auth/AuthScreens.jsx`
- `Splash`: centered spinner.
- Shared `Shell`: centered card on `bg-bg`, brand glyph square (`bg-primary`,
  letter `R`), title/subtitle, and a **corner row of 6 theme swatch buttons**
  (click to switch theme live).
- `SetupScreen`: "Welcome to RichUI" / "Create the administrator account".
  Fields: username, password, confirm password. Validates match locally; calls
  `auth.setup`. Error banner (danger tones).
- `AuthScreen`: tabbed **Sign in / Register**. Sign in → `auth.login`. Register →
  `auth.register`, then show success banner with the returned message, clear
  fields, switch to Sign in. A note under Register: "New accounts require
  administrator approval before first sign-in." Error + success banners.

### `src/components/Icons.jsx`
An `Icon` object of inline-SVG components (stroke = `currentColor`, width/height
~18, `viewBox 0 0 24 24`, round caps/joins). Required keys (used across the app):
`Dashboard, Sliders, Nodes, Frame, Grid, Table, Kanban, Sun, Search, Bell, Plus,
Trash, Check, Chevron, Drag, Arrow, Both, Line, Move, Mineral, Unit, Users,
Logout`. `Arrow` = single → ; `Both` = ↔ ; `Line` = — ; `Frame` = corner-bracket
square; `Users` = two people; `Logout` = door+arrow. Each accepts a `size` prop.

### `src/components/ui.jsx`
Reusable primitives (Tailwind + theme utilities):
- `Card({title,subtitle,action,className,children})` — rounded border `bg-surface`
  with optional header.
- `Button({variant,size,...})` — variants `primary` (`bg-primary text-primary-fg`),
  `ghost`, `outline`, `danger`, `subtle`; sizes `sm/md/lg`; `active:scale-[.97]`.
- `Badge({tone,children})` — tones `muted/primary/success/warning/danger` as
  soft `bg-*/15 text-*`.
- `Toggle({checked,onChange,label})` — pill switch.
- `Field({label,hint,children})` and `inputCls` — shared input class
  (`rounded-lg border bg-bg px-3 py-2 focus:ring-2 focus:ring-primary/30`).

---

## 7. App shell — `src/App.jsx`

Authenticated layout. State: `active` page id (init from `location.hash`),
`collapsed` sidebar, `paletteOpen`. Sync `active` → `location.hash`.

**Nav model.** Base `NAV` array of `{id,label,icon,page,hint}`:
1. `dashboard` — Dashboard — "Widgets & live charts"
2. `controls` — Interactions — "Inputs, drag, gestures"
3. `nodes-frames` — **Node Editor** — "Frames + properties panel"
4. `table` — Data Table — "Sort, filter, select"
5. `kanban` — Kanban — "Drag-and-drop board"

Admin-only entry appended when `user.role === 'admin'`:
`users` — Manage Users — "Approve & manage accounts" (icon `Users`).
`const nav = isAdmin ? [...NAV, ADMIN_NAV] : NAV`. Current page =
`nav.find(active) ?? nav[0]`.

**Sidebar** (`w-60`, collapses to `w-[68px]`): brand (glyph square + "RichUI /
Interaction Lab"), nav buttons (active = `bg-primary/15 text-primary`), collapse
toggle at bottom.

**Topbar** (`h-14`): page title+hint, a Search button (opens palette, shows
`⌘K`), `ThemePicker` (dropdown of the 6 `THEMES` with swatches + check on
active), a Bell with a dot, and `AccountMenu`.

**`AccountMenu`**: avatar circle with `user.username` initials (2 chars). Dropdown
shows username + role `Badge` and a **Sign out** button (`auth.logout`).

**Command palette** (`⌘/Ctrl-K` toggles; `Esc` closes): modal with a search
input filtering `nav` by label+hint; Enter/click navigates. Receives `items={nav}`.

**Main**: renders the active page; wrap in `<div key={active} className="animate-fade-in">`.

---

## 8. Pages

### `Dashboard.jsx`
Grid of widgets, all themed, all local/synthetic data:
- 4 stat cards (value + delta `Badge`).
- **Live line chart** (`requests/sec`): SVG path over ~40 points, updates every
  ~1.1s (shift array, append clamped random); gradient area fill; "● streaming" badge.
- **Bar chart** by weekday (hover highlight).
- **Service health** rows: name + % with colored progress bars.
- **Completion ring**: SVG donut at 72% with legend.
- **Activity feed**: avatars + text + relative time.

### `Controls.jsx`
A responsive grid of demo cards, each a distinct interaction (all local state):
form controls with inline validation; range sliders with **live preview box**
(scale/opacity reacts); segmented controls + checkboxes; **tag input** (Enter to
add, Backspace to remove, ✕ chips); **drag-to-reorder** list (HTML5 DnD);
**file dropzone** (dragover highlight, lists dropped files, nothing uploads);
**accordion** (animated grid-rows expand); **stepper + async button** (spinner
for ~1.4s); **star rating** (hover preview) + a pure-CSS **tooltip**.

### `DataTable.jsx`
Client-side table over ~18 synthetic people (`{id,name,email,role,status,score}`):
column **sort** (name/role/status/score, toggle dir), **search** box, **role**
filter `select`, **row selection** with checkboxes + select-all-on-page,
**pagination** (page size 7) with numbered pages, status `Badge`s, a score
mini-bar. Selecting rows shows a count + Clear action.

### `Kanban.jsx`
Four columns (`backlog/todo/doing/done`) over ~8 seed cards. **HTML5 drag and
drop**: dragging a card sets a ref id; dropping on a column updates the card's
`col`; the hovered column highlights; per-column count + story-point totals;
empty columns show a "Drop here" placeholder.

### `ManageUsers.jsx` (admin)
Loads `api.listUsers()`. Table: avatar+username (marks "(you)"), role `Badge`,
status `Badge` (`approved`→success, `pending`→warning, `rejected`→danger,
`disabled`→muted), registered date, and **per-row actions** by status:
- `pending` → **Approve** / **Reject**
- `approved` (not you) → **Disable**
- `disabled`/`rejected` → **Re-approve**
- any (not you) → **Delete** (trash)

Header shows count of pending ("N awaiting approval") + Refresh. Actions call
`api.setUserStatus`/`deleteUser` then reload. Errors shown in a banner. Pending
rows tinted (`bg-warning/5`).

---

## 9. Node Editor — `src/pages/NodeEditorFrames.jsx` (the centerpiece)

A from-scratch graph editor (no library). It must feel precise, not "clunky".

### Entities & state
- `nodes`: `{id, x, y, label, sub, color, frame: frameId|null}`. Fixed size
  `NODE_W=168`, `NODE_H=80`.
- `frames`: `{id, x, y, w, h, label, color}` — resizable group containers.
- `edges`: `{id, from:{node:id, port}, to:{node:id, port}, type}` where
  `type ∈ {directional, none, bidirectional}`. **`from.node`/`to.node` hold the
  id of a node OR a frame** (endpoints are generic).
- `PORTS = ['top','right','bottom','left']`;
  `PORT_DIR = {top:[0,-1], right:[1,0], bottom:[0,1], left:[-1,0]}`.
- `view = {x, y, z}` pan+zoom; `drag` (current interaction); `selected`
  `{kind:'node'|'frame'|'edge', id}`; `menu` (context menu) `{x,y,kind,id}`.
- `uid(prefix)` from an incrementing counter. Link types with icons:
  Directional (Arrow), Plain line (Line), Bidirectional (Both). `PALETTE` of 6 hex colors.
- Seed: 1 frame "Processing stage", 4 nodes (two inside the frame), 4 edges —
  including **one frame→node edge** to demonstrate frame ports.

### Geometry (exact)
```js
// rectangle for any endpoint id (node uses NODE_W/H, frame uses its w/h)
rectOf(id) -> {x,y,w,h} | null

// port anchor on a rect
portPoint({x,y,w,h}, port):
  top:    {x:x+w/2, y}
  right:  {x:x+w,   y:y+h/2}
  bottom: {x:x+w/2, y:y+h}
  left:   {x,       y:y+h/2}

// cubic bezier that LEAVES/ENTERS each port perpendicular to the edge,
// anchored to the EXACT chosen ports (never re-routed to nearest side)
edgePath(p0, port0, p1, port1):
  d0 = PORT_DIR[port0]; d1 = PORT_DIR[port1]
  k  = clamp(dist(p0,p1)/2, 40, 170)
  c0 = p0 + d0*k;  c1 = p1 + d1*k
  return `M p0 C c0 c1 p1`
```

### Coordinate transform
The world is rendered inside a div with
`transform: translate(view.x, view.y) scale(view.z)`. `screenToWorld(cx,cy) =
((client - wrapRect.origin) - view.xy) / view.z`. Background is a dotted grid
(`radial-gradient`) whose size/position track `view`.

### Interactions
- **Pan**: pointerdown on empty canvas (left button only) → `drag.kind='pan'`;
  move updates `view.x/y`. **Zoom**: wheel, cursor-anchored, clamp `z ∈ [0.35, 2.2]`.
- **Move node**: pointerdown on a node body (left only) → drag; while dragging
  compute the frame under the node's center (`frameAt`) and highlight it; on drop
  set `node.frame` to that frame id (or null). → **drag-and-drop grouping.**
- **Move frame**: pointerdown on frame body → drag the frame **and all member
  nodes together** (capture each child's offset at drag start).
- **Resize frame**: small bottom-right handle → `drag.kind='resize'`
  (min 160×110).
- **Connect**: pointerdown on a **port** (`startConnect(e, ownerId, port)`,
  stopPropagation) → `drag.kind='connect'`; live dashed bezier follows the
  cursor; `hitPort(world, excludeId)` snaps to the nearest port (within 26px) of
  any node **or frame** (excluding the source); on drop, if a target port exists
  and the pair isn't a duplicate, create an edge with the toolbar's current
  `linkType` and select it.
- All drag handlers ignore non-left buttons so **right-click** is free for menus.
- Keyboard: `Delete`/`Backspace` removes the `selected` item (ignored while
  typing in an input).

### Ports — shared `PortHandles` component
Both nodes **and** frames render the **same 4 ports** via
`<PortHandles ownerId drag onStart={startConnect}/>`: small circles positioned
at the rect's edge midpoints (`-top-2 left-1/2`, `-right-2 top-1/2`, etc.),
hidden until the parent is hovered (`opacity-0 group-hover:opacity-100`; parents
have the `group` class), shown during any connect, and enlarged + `pulse-ring`
when they're the current snap target. So **any endpoint combination works**:
node↔node, node↔frame, frame↔frame.

### Rendering order (z within the world layer)
1. **Frames** (dashed border, tinted `color-mix` fill, title bar, resize handle,
   ports). Highlight when selected or when a node is being dropped into them.
2. **SVG edge layer** (`overflow-visible`): one arrow `<marker>` reused, fill
   `context-stroke` so each arrow matches its line color. Per edge: a fat
   transparent hit-path (16px) for easy selection + the visible path.
   `markerEnd` when `type≠none`; `markerStart` also when `bidirectional`.
   Selected edge = `var(--primary)` + thicker. Plus the live connect preview path.
3. **Nodes** (rounded card: color bar, label, sub, ports).

### Selection → Properties panel (right, `w-72`, always visible)
`<Properties>` renders by `selected.kind`:
- **Node**: edit `label`, `sub`, color (swatches + native color input), X/Y
  number fields, "Member of frame" `select` (none / each frame — another way to
  set membership), a note about the 4 ports, **Delete node**.
- **Frame**: edit title, tint color, X/Y, W/H, a live "contains N nodes" note,
  **Delete frame**.
- **Link**: shows `from→to` endpoints, the three style options (radio-like
  buttons), **Delete link**.
- **Nothing selected**: hint to select something.
All edits patch state live; the canvas reflects changes immediately and vice-versa.

### Right-click context menu
Right-clicking a node or frame `openMenu(e,kind,id)` (preventDefault,
stopPropagation, select it, set `menu` at cursor). A floating `ContextMenu`
(viewport-clamped, with a full-screen backdrop that closes on click/right-click,
and `Esc` to close) lists actions:
- **Node**: Duplicate (offset copy, same frame) · Disconnect links · Remove from
  frame *(only if in a frame)* · — · **Delete node**.
- **Frame**: Add node here (spawns a node centered inside, as a member) ·
  Disconnect links · — · **Delete frame**.
Right-clicking empty canvas suppresses the browser menu and closes any open menu.

### Delete semantics
Deleting a **node** removes its edges. Deleting a **frame** keeps its member
nodes (releases membership) but removes edges attached to the frame. Deleting an
**edge** removes just that edge.

### Toolbar
`+ Node`, `Frame`, a New-link type selector (Directional / Plain line /
Bidirectional), a live count "`F` frames · `N` nodes · `E` links", and **Reset
view**. A bottom-left hint overlay summarizes the gestures.

---

## 10. Local development (no Docker)

Two terminals: (1) `cd app && APP_PORT=8080 DB_PATH=./richui.db go run .`,
(2) `cd app/web && npm install && npm run dev` (Vite proxies `/api` → :8080).
In the container, the Go binary serves both on `APP_PORT`. The Go server binds
`APP_HOST` (default `127.0.0.1`), so a bare `go run` is private to your machine;
prefix `APP_HOST=0.0.0.0` to expose it on your network.

---

## 11. Acceptance checklist (verify parity)

Backend (curl):
- [ ] Fresh `GET /api/setup/status` → `initialized:false`.
- [ ] `POST /api/setup` creates admin (201), sets a session cookie; a second
      call returns 409.
- [ ] `POST /api/auth/register` → pending; that user's login → 403 until approved.
- [ ] Admin `GET /api/users` lists pending first; approve → user login 200.
- [ ] Non-admin → `/api/users` 403; admin self-delete → 400; wrong password → 401.
- [ ] Container `HEALTHCHECK` (`/<bin> -healthcheck`) passes; data survives a
      container restart (named volume).
- [ ] Image is a single static binary on distroless (~20–25 MB).

Frontend:
- [ ] First visit shows the **Setup** screen; after setup, **Sign in/Register**;
      after login, the app shell. Admin sees **Manage Users**; users don't.
- [ ] All **6 themes** switch instantly (persisted) and restyle every page
      including SVG/canvas via the CSS variables.
- [ ] `⌘/Ctrl-K` palette; sidebar collapse; account menu sign-out.
- [ ] Node Editor: nodes and frames each expose **4 ports**; links anchor to the
      exact chosen ports with a perpendicular bezier and **don't re-route**;
      three arrow styles; drag node **into a frame** to group; dragging a frame
      moves its nodes; resize frames; selection drives the **properties panel**;
      **right-click** menus on node and frame; pan/zoom; delete via key/menu/panel.
- [ ] Dashboard live chart animates; Controls demos all interact; DataTable
      sorts/filters/paginates/selects; Kanban drag-and-drop moves cards.

Infra:
- [ ] `make compose` builds and starts one container; `.env` auto-created from
      `.env.example`; `APP_HOST` (default `127.0.0.1` — private; published port
      bound to that interface), `APP_PORT` and `DOCKER_PLATFORM` honored.
- [ ] Repo root stays orchestration-only; all app code under `app/`.
- [ ] Every original `richui`/`RichUI` literal replaced per §0.
- [ ] Every dependency + base image resolved to **latest stable** per §0.5 (no
      alpha/beta/RC, Node on current LTS); build + above checks pass on them, and
      the generated project committed its lockfiles.
```
