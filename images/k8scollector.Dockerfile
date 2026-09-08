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
# linux/amd64 ONLY, and deliberately so. Percona's apt repo publishes
# percona-toolkit — the package that carries pt-k8s-debug-collector — for amd64
# alone; its arm64 suite holds only xtrabackup and libdbd-mysql-perl, so on arm64
# apt silently falls back to Debian's percona-toolkit 3.2.1, which predates the
# collector and does not contain it. Verified against
# repo.percona.com/tools/apt/dists/bookworm: amd64 has percona-toolkit 3.6.0-1
# (./usr/bin/pt-k8s-debug-collector, 14 MB), arm64 has no such package. This
# matches app/docker.go's platformAMD64 pin, which exists for exactly this case,
# so on an arm64 host the capture runs under emulation — slow, but it is a
# diagnostic capture rather than anything on a hot path.
#
# The platform is passed by images/service.sh rather than pinned in FROM (buildkit
# rejects a constant there). What actually guarantees a usable image is the
# `command -v` assertion at the end of the RUN: build this on arm64 and apt
# installs Debian's collector-less 3.2.1, the assertion fails, and the build stops
# — instead of producing an image that looks fine and has no collector in it.
#
# Built by `make k8scollector-image` and tagged dbcanvas-k8scollector:debian-12-amd64
# to match k8sCollectorImage() in app/k8sdiag.go — that is what a capture asks
# Docker for.
FROM debian:12-slim

ENV DEBIAN_FRONTEND=noninteractive

# percona-toolkit carries pt-k8s-debug-collector, pt-mysql-summary and
# pt-mongodb-summary. Percona's 3.6.0 outranks Debian's 3.2.1 on version, so no
# apt pinning is needed once the repo is enabled — but the build asserts the
# binary exists rather than trusting that, because the failure mode is a package
# that installs happily and is missing the one tool this image is for.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates curl gnupg procps tar gzip \
      default-mysql-client postgresql-client \
 && curl -fsSL -o /tmp/percona-release.deb \
      https://repo.percona.com/apt/percona-release_latest.generic_all.deb \
 && apt-get install -y --no-install-recommends /tmp/percona-release.deb \
 && percona-release enable-only tools release \
 && apt-get update \
 && apt-get install -y --no-install-recommends percona-toolkit \
 && rm -rf /var/lib/apt/lists/* /tmp/percona-release.deb \
 && command -v pt-k8s-debug-collector \
 && command -v pt-mysql-summary \
 && command -v pt-mongodb-summary

# pt-k8s-debug-collector shells out to kubectl rather than talking to the API server
# through client-go — 3.6.0 fails with `get namespaces: error: exec: "kubectl":
# executable file not found in $PATH` without it, which is not something the tool's
# documentation mentions. Pinned rather than "stable" so an image rebuild is
# reproducible; kubectl is skew-tolerant across a couple of minor versions either
# way, and the k3s catalog in versions.yaml currently tops out at v1.36.
ARG KUBECTL_VERSION=v1.34.1
RUN curl -fsSL -o /usr/local/bin/kubectl       "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl"  && chmod 0755 /usr/local/bin/kubectl  && kubectl version --client=true >/dev/null

# A capture writes cluster-dump.tar.gz into the working directory; give it one of
# its own rather than /.
WORKDIR /capture

# The capture always supplies its own command. This only keeps the container alive
# if something ever starts it without one.
CMD ["sleep", "infinity"]
