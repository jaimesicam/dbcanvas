# syntax=docker/dockerfile:1
#
# MClusterAdmin — a third-party MongoDB administration panel (MIT):
# https://github.com/PrzemekMalkowski/mclusteradmin
#
# Built from source at a pinned tag rather than pulled, because upstream publishes
# no image. The source is cloned here rather than vendored into this repo: it is
# somebody else's code, and a pinned tag says exactly whose and which.
#
# Upstream ships a Dockerfile of its own and this is deliberately not it. That one
# hardcodes GOARCH=amd64, which is silently wrong on the half of this project's
# installations that target arm64 — you would get an image whose manifest says
# arm64 wrapped around an amd64 binary, and the container would die on exec with
# nothing useful in its log. Here the build stage runs on the BUILD host's
# architecture and cross-compiles to TARGETARCH, which is both correct on either
# platform and faster than building under emulation.
#
# The runtime stage mirrors upstream's: scratch, the static binary, the CA bundle
# (the panel can be pointed at a TLS/Atlas cluster), and index.html at
# /data/templates, which is where the binary looks for it relative to WORKDIR.
# Nothing else — there is no shell in here, so nodeversion.go reads this node's
# version from the image tag rather than by exec'ing into it.

ARG MCA_VERSION=v0.3.7

# BUILDPLATFORM and TARGETARCH are BuildKit's own build arguments, and the legacy
# builder — a Docker install with no buildx plugin, or DOCKER_BUILDKIT=0 — sets
# neither: the FROM below resolves to an empty platform and the build dies before its
# first step ("failed to parse platform : "" is an invalid OS component"), and
# GOARCH would be empty. images/service.sh passes both by hand whenever the build is
# not a BuildKit one — the target platform there, since a legacy build cannot split
# build from target.
#
# Declared with NO default on purpose: a default here would win over the value
# BuildKit supplies, and the build stage would be pulled for the wrong architecture.
ARG BUILDPLATFORM

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG MCA_VERSION
ARG TARGETARCH
RUN apk add --no-cache git
WORKDIR /src
RUN git clone --depth 1 --branch "${MCA_VERSION}" \
      https://github.com/PrzemekMalkowski/mclusteradmin.git .
RUN go mod download
RUN CGO_ENABLED=0 GOOS=linux GOARCH="${TARGETARCH}" \
      go build -trimpath -ldflags="-s -w" -o /mca .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /mca /mca
COPY --from=build /src/templates/index.html /data/templates/index.html
WORKDIR /data
EXPOSE 8787
ENTRYPOINT ["/mca"]
