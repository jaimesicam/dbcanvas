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

# ---- stage 3: minimal runtime (single static binary) ----
FROM gcr.io/distroless/static-debian12 AS runtime
COPY --from=build /dbcanvas /dbcanvas
ENV APP_HOST=0.0.0.0
ENV APP_PORT=8080
ENV DB_PATH=/data/dbcanvas.db
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["/dbcanvas"]
