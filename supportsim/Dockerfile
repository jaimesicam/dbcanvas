# syntax=docker/dockerfile:1

# ---- build: the Go server with the dashboard embedded, plus the model weights ----
FROM golang:1.26-alpine AS build
RUN apk add --no-cache curl
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
# The weights (~90 MB) are fetched here, pinned and checksum-verified, before the
# source is copied — so editing the app does not re-download them.
COPY scripts ./scripts
RUN sh scripts/fetch-model.sh /model
COPY main.go ./
COPY internal ./internal
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /supportsim .
# The GPL text and the licence of every Go module linked in, staged by images/apps.sh.
COPY .dbcanvas-build/ /lic/
RUN sh /lic/go-third-party-licenses.sh "supportsim" . > /lic/THIRD_PARTY_LICENSES.txt

# ---- runtime: one static binary and the model, no shell ----
FROM gcr.io/distroless/static-debian12
COPY --from=build /supportsim /supportsim
COPY --from=build /model /model
COPY NOTICE /NOTICE
COPY --from=build /lic/LICENSE /lic/THIRD_PARTY_LICENSES.txt /usr/share/doc/supportsim/
ENV PORT=8095 MODEL_DIR=/model
EXPOSE 8095
ENTRYPOINT ["/supportsim"]
