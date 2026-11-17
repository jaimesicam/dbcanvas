# syntax=docker/dockerfile:1
# k8scollector.Dockerfile — the container a K3D "Diagnostics" capture runs in.
#
# pt-k8s-debug-collector reaches the cluster over a kubeconfig, so it does not have
# to live on the k3s node — and should not. rancher/k3s is a minimal busybox image
# with no package manager, and the collector's most useful output is the per-pod
# database summary it makes by port-forwarding into a database pod and running
# pt-mysql-summary / pt-mongodb-summary / pg_gather against it. Those need real
# clients. So a capture runs here instead: one throwaway container on the stack
# network, handed the cluster's kubeconfig, removed when it is done.
#
# Built for each architecture, from percona-toolkit's source. Percona's apt repo
# publishes the percona-toolkit package for amd64 only (its arm64 suite has no
# such package, and Debian's own percona-toolkit 3.2.1 predates the collector), so
# the tools this image is for are built here instead: pt-k8s-debug-collector and
# pt-mongodb-summary are Go programs, cross-compiled at native speed for the
# target architecture, and pt-mysql-summary is a shell script that needs the
# MySQL client, which Debian has for both. The collector itself runs openssl
# locally and psql inside the database pods; nothing else from the toolkit.
#
# TOOLKIT_VERSION pins the percona-toolkit release, so a rebuild is reproducible.
#
# Built by `make k8scollector-image` and tagged dbcanvas-k8scollector:debian-12-<arch>
# to match k8sCollectorImage() in app/k8sdiag.go — that is what a capture asks
# Docker for. Every COPY is --from a stage: the build context is this file alone,
# which is what lets DBCanvas build the image from its web interface
# (app/extraimages.go).
ARG TOOLKIT_VERSION=3.7.2

FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS tools
ARG TOOLKIT_VERSION
ARG TARGETOS TARGETARCH
RUN git clone --depth 1 --branch "v${TOOLKIT_VERSION}" https://github.com/percona/percona-toolkit /src
WORKDIR /src
RUN set -eux; \
    mkdir -p /out; \
    for t in pt-k8s-debug-collector pt-mongodb-summary; do \
      CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" go build -trimpath \
        -ldflags "-s -w -X main.Version=${TOOLKIT_VERSION} -X main.Build=v${TOOLKIT_VERSION} -X main.GoVersion=$(go env GOVERSION)" \
        -o "/out/$t" "./src/go/$t"; \
    done; \
    install -m 0755 bin/pt-mysql-summary /out/pt-mysql-summary

FROM debian:12-slim
ARG TOOLKIT_VERSION
ARG TARGETARCH
ENV DEBIAN_FRONTEND=noninteractive

# The clients the summaries connect with, perl for pt-mysql-summary, and openssl,
# which the collector runs on each TLS secret it describes.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl procps tar gzip openssl perl \
      default-mysql-client postgresql-client \
 && rm -rf /var/lib/apt/lists/*

COPY --from=tools /out/ /usr/local/bin/
COPY --from=tools /src/COPYING /usr/share/doc/percona-toolkit/COPYING

# kubectl, for the person debugging a capture from inside this container, and for
# percona-toolkit releases whose collector shells out to it (3.6.0 did: `get
# namespaces: error: exec: "kubectl": executable file not found in $PATH`). Pinned
# rather than "stable" so a rebuild is reproducible; kubectl is skew-tolerant across
# a couple of minor versions either way, and the k3s catalog in versions.yaml
# currently tops out at v1.36.
ARG KUBECTL_VERSION=v1.34.1
RUN curl -fsSL --retry 5 --retry-all-errors --retry-delay 3 -o /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl" \
 && chmod 0755 /usr/local/bin/kubectl \
 && kubectl version --client=true >/dev/null

# What the image is for, asserted rather than assumed: a build that produced no
# working collector fails here instead of shipping an image that has none.
RUN pt-k8s-debug-collector --version \
 && pt-mongodb-summary --version \
 && command -v pt-mysql-summary

# A capture writes cluster-dump.tar.gz into the working directory; give it one of
# its own rather than /.
WORKDIR /capture

# The capture always supplies its own command. This only keeps the container alive
# if something ever starts it without one.
CMD ["sleep", "infinity"]
