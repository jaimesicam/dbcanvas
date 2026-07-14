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
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /dbcanvas .

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
COPY --from=k3d /k3d /usr/local/bin/k3d
ENV APP_HOST=0.0.0.0
ENV APP_PORT=8080
ENV DB_PATH=/data/dbcanvas.db
ENV K3D_BIN=/usr/local/bin/k3d
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/dbcanvas"]
