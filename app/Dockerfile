# syntax=docker/dockerfile:1
#
# Built from the REPOSITORY ROOT, not from app/: the image carries dbcanvas-cli as
# well as the server, and the CLI is a sibling Go module (cli/). Every path below is
# therefore repo-relative. docker-compose.yml sets the context accordingly; building
# by hand is `docker build -f app/Dockerfile .`.

# ---- stage 1: build the React SPA ----
FROM node:22-alpine AS web
WORKDIR /web
COPY app/web/package.json app/web/package-lock.json ./
RUN npm ci
COPY app/web/ ./
RUN npm run build      # → /web/dist

# ---- stage 2: build the Go server with the SPA embedded ----
FROM golang:1.26-alpine AS build
WORKDIR /src
ARG APP_VERSION=dev
COPY app/go.mod app/go.sum ./
RUN go mod download
COPY app/*.go ./
COPY app/cmd/ ./cmd/
COPY --from=web /web/dist ./web/dist
# APP_VERSION is stamped in rather than read at runtime: the What's New dialog and
# the API page both need to say which build this is, and a file the container could
# be missing is a worse source for that than a linker flag (see app/version.go).
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.appVersion=${APP_VERSION}" -o /dbcanvas .
# The password-reset tool. It ships in the image because the runtime is distroless: no
# shell and no sqlite3, so a forgotten admin password would otherwise need the volume
# mounted somewhere else entirely.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
      -o /dbcanvas_reset_password ./cmd/dbcanvas_reset_password

# ---- stage 2a: dbcanvas-cli, for every platform the app will offer ----
#
# These ride in the image so that somebody using a DBCanvas on a shared server can
# download a client for it without cloning the repository — which is exactly the
# audience a CLI has. Five static binaries, ~6.5 MB each — 33 MB on the image, which
# is a fair price for not telling a remote user to go and install a Go toolchain.
#
# Keep the list in step with cliPlatforms in app/clidownload.go and CLI_PLATFORMS in
# the Makefile.
FROM golang:1.26-alpine AS cli
WORKDIR /src
ARG APP_VERSION=dev
COPY cli/go.mod cli/go.sum ./
RUN go mod download
COPY cli/ ./
RUN set -eu; mkdir -p /out; \
    for p in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do \
      os="${p%/*}"; arch="${p#*/}"; out="/out/dbcanvas-cli_${os}_${arch}"; \
      [ "$os" = windows ] && out="$out.exe"; \
      GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 go build -trimpath \
        -ldflags="-s -w -X main.version=${APP_VERSION}" -o "$out" .; \
    done

# ---- stage 2b: the k3d binary (K3D cluster frame) ----
# k3d is a Docker API *client*: it asks the daemon to create the k3s containers. DBCanvas already
# holds the daemon socket, so it runs k3d itself rather than shipping a side-car container. The
# binary is static, which is why it can live in the distroless runtime (which has no shell — none
# is needed, the app execs it directly). In local development the host's k3d is used instead.
FROM alpine:3.20 AS k3d
ARG K3D_VERSION=v5.8.3
ARG TARGETARCH
RUN apk add --no-cache curl \
 && curl -fsSL -o /k3d "https://github.com/k3d-io/k3d/releases/download/${K3D_VERSION}/k3d-linux-${TARGETARCH:-amd64}" \
 && chmod 0755 /k3d \
 && /k3d version

# ---- stage 3: minimal runtime (single static binary) ----
FROM gcr.io/distroless/static-debian12 AS runtime
COPY --from=build /dbcanvas /dbcanvas
COPY --from=build /dbcanvas_reset_password /usr/local/bin/dbcanvas_reset_password
COPY --from=cli /out/ /usr/local/share/dbcanvas/cli/
COPY --from=k3d /k3d /usr/local/bin/k3d
ENV APP_HOST=0.0.0.0
ENV APP_PORT=8080
ENV DB_PATH=/data/dbcanvas.db
ENV K3D_BIN=/usr/local/bin/k3d
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/dbcanvas"]
