# DBCanvas — Interaction Lab

A professional single-page web app that doubles as a UI-interaction lab, served
by a Go backend that adds authentication and user management. It ships as one
small (~22 MB) Docker container: the Go binary serves the embedded React SPA
**and** the JSON API on a single port, and stores users/sessions in SQLite on a
persistent volume. No separate web/api/db containers.

```
browser ──HTTP──> Go binary (:APP_PORT)
                  ├─ serves embedded React SPA (//go:embed)
                  └─ /api/*  ──> SQLite (/data, Docker volume)
```

## Quick start (Docker)

```sh
make compose
```

This creates `.env` from `.env.example` (if missing), builds the image, and
starts one container. Then open **http://localhost:8080**. The first visit asks
you to create an administrator account.

| Command | What it does |
| --- | --- |
| `make compose` | Create `.env` if needed, build, and start the stack |
| `make build` | Build the image only |
| `make up` / `make down` | Start / stop containers (no rebuild on `up`) |
| `make restart` | Recreate the stack |
| `make logs` | Follow application logs |
| `make clean` | Stop the stack and remove the built image |

## Configuration (`.env`)

| Variable | Default | Meaning |
| --- | --- | --- |
| `APP_HOST` | `127.0.0.1` | Host interface the published port binds to. `127.0.0.1` is private to this machine; `0.0.0.0` exposes it on all interfaces. |
| `APP_PORT` | `8080` | Port the app listens on (host + container). |
| `DOCKER_PLATFORM` | `linux/amd64` | Target platform for the image build. |

The container always listens on all interfaces internally; host-side exposure is
controlled by the compose publish binding, not by `APP_HOST` inside the container.

## Local development (no Docker)

Two terminals:

```sh
# terminal 1 — Go API + SQLite
cd app && APP_PORT=8080 DB_PATH=./dbcanvas.db go run .

# terminal 2 — Vite dev server (proxies /api → :8080)
cd app/web && npm install && npm run dev
```

The Go server binds `APP_HOST` (default `127.0.0.1`), so a bare `go run` stays
private to your machine. Prefix `APP_HOST=0.0.0.0` to expose it on your network.

## What's inside

- **Dashboard** — stat cards, a live streaming line chart, bar chart, service
  health bars, a completion ring, and an activity feed.
- **Interactions** — inline validation, range sliders with live preview,
  segmented controls, tag input, drag-to-reorder, a file dropzone, accordion,
  async button, star rating, and a CSS tooltip.
- **Node Editor** — a from-scratch graph editor with resizable frames, generic
  4-port nodes and frames, perpendicular bezier links in three arrow styles,
  drag-to-group, pan/zoom, a properties panel, and right-click menus.
- **Data Table** — sort, search, role filter, row selection, and pagination over
  synthetic data.
- **Kanban** — a four-column HTML5 drag-and-drop board.
- **Manage Users** (admin) — approve, reject, disable, re-approve, and delete
  accounts.

## Tech stack

- **Frontend:** React + Vite + Tailwind CSS v4 (CSS-first). No UI/icon/graph/state
  libraries — icons and the node editor are hand-built.
- **Backend:** Go standard-library `net/http`, `modernc.org/sqlite` (pure-Go, no
  CGO), `golang.org/x/crypto/bcrypt`. The SPA is embedded with `//go:embed`.
- **Runtime:** a single static binary on `gcr.io/distroless/static-debian12`.

## Security model

- Passwords hashed with bcrypt; sessions are httpOnly cookies (no tokens in JS).
- Setup self-locks once any user exists.
- Every admin route is enforced server-side; the hidden admin menu is convenience
  only. Admins cannot disable or delete their own account.
