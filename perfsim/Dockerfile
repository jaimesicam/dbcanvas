# syntax=docker/dockerfile:1

# ---- build the Go server with the static dashboard embedded ----
# The build stage runs on the building host's own architecture ($BUILDPLATFORM) and
# cross-compiles for the image's ($TARGETOS/$TARGETARCH): Go needs no emulation to target
# the other architecture, so building the amd64 image on an arm64 host (or the reverse)
# costs about as much as a native build. Only the runtime stage is the target platform.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
COPY internal ./internal
COPY web ./web
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /perfsim .
# The GPL text and the licence of every Go module linked in, staged by images/apps.sh.
COPY .dbcanvas-build/ /lic/
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH sh /lic/go-third-party-licenses.sh "perfsim" . > /lic/THIRD_PARTY_LICENSES.txt

# ---- minimal runtime (single static binary, no shell needed) ----
FROM gcr.io/distroless/static-debian12
COPY --from=build /perfsim /perfsim
COPY --from=build /lic/LICENSE /lic/THIRD_PARTY_LICENSES.txt /usr/share/doc/perfsim/
ENV PORT=8096
EXPOSE 8096
ENTRYPOINT ["/perfsim"]
