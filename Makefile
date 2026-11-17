SHELL := /bin/bash

# The release this tree is. Stamped into the binary so it can say what it is, and so the
# What's New dialog knows whether the owner has seen these notes.
VERSION ?= $(shell cat VERSION 2>/dev/null || echo dev)

.PHONY: tag release-notes github-release pull-images publish-images adopt-images images versions smoke supportsim-image stocksim-image ledgersim-image perfsim-image pagerpanic-image intranet-image vnc-image kubeadm-image relaybin catalog dbcanvas bundle

## tag: create the annotated tag v$(VERSION) on HEAD, the commit this release is built from.
## Refuses a dirty tree or a tag that already exists. Local only; github-release publishes.
tag:
	@bash scripts/release.sh tag $(VERSION)

## release-notes: print this release's notes (CHANGELOG.md's "## $(VERSION)" section).
release-notes:
	@bash scripts/release.sh notes $(VERSION)

## github-release: push tag v$(VERSION). The release workflow (.github/workflows/release.yml)
## then builds dbcanvas-cli, signs it with Sigstore and creates the GitHub release from the notes.
## This publishes — the repository is public — so it is never run by another target.
github-release:
	@bash scripts/release.sh publish $(VERSION)

## pull-images: pull this release's node images (the systemd bases, the images baked on them
## and the demo applications) from
## IMAGE_REGISTRY (default ghcr.io/jaimesicam) instead of building them, and write images.yaml.
pull-images:
	@bash scripts/registry.sh pull

## publish-images: push this release's locally built node images to IMAGE_REGISTRY. This
## publishes, so no other target runs it. One run is one architecture of every image, so a
## release is two runs, in either order, from one host or two:
##   DOCKER_PLATFORM=linux/amd64 make images publish-images
##   DOCKER_PLATFORM=linux/arm64 make images publish-images
## A DOCKER_PLATFORM given on the command line sets the Kubeadm node image's architecture too.
## The demo applications are pushed per architecture and joined into one multi-architecture
## tag after each push (needs docker buildx).
publish-images:
	@bash scripts/registry.sh push

## adopt-images: give the images this host already built the current release's tag, instead
## of rebuilding them. Every image is tagged with its release (dbcanvas-systemd:…-amd64-v0.0.14)
## so installations sharing a Docker daemon cannot overwrite each other's; images built before
## that have no release in their tag. Run once after upgrading when they were built from this
## checkout. Nothing is rebuilt or removed.
adopt-images:
	@bash images/adopt.sh

## smoke: render the React components off-browser and fail on any render error,
## and check the canvas can actually reach every link target the backend accepts.
## Then mount every heavy page in headless Chrome, which is the half the off-browser
## pass cannot see: effects do not run under SSR, and an effect that throws blanks
## the page. Skipped with a notice where there is no Chrome — the off-browser pass
## is the hard gate.
smoke:
	cd app/web && npm run smoke && npm run smoke:browser

## catalog: copy the version catalogue (versions.yaml, versions-arm64.yaml) into app/catalog,
## where the dbcanvas binary embeds it (app/catalogembed.go).
catalog:
	@cp versions.yaml versions-arm64.yaml app/catalog/ && ls -l app/catalog/*.yaml

## bundle: collect what the dbcanvas binary carries besides its code into app/bundle
## (app/bundle/README.md): the node Dockerfiles, Pager Panic's banks, the licence texts, and
## Big Hole built at its pinned commit (in a node container; rebuilt only when the pin changes).
## Needs app/web's node_modules (npm ci), Docker for Big Hole, and the network.
BIGHOLE_REF := $(shell sed -n 's/^[[:space:]]*bigHoleRef = "\(.*\)"/\1/p' app/bighole.go)
K3D_VERSION := $(shell sed -n 's/^const k3dVersion = "\(.*\)"/\1/p' app/k3dbin.go)
bundle:
	rm -rf app/bundle/images app/bundle/pagerpanic app/bundle/licenses
	mkdir -p app/bundle/images app/bundle/pagerpanic app/bundle/licenses
	cp images/*.Dockerfile app/bundle/images/
	cp pagerpanic/banks/* app/bundle/pagerpanic/
	cp LICENSE THIRD_PARTY_NOTICES.md app/bundle/licenses/
	cd app && sh scripts/go-third-party-licenses.sh "dbcanvas" . ./cmd/dbcanvas-relay > bundle/licenses/THIRD_PARTY_LICENSES-go.txt
	cd app/web && node licenses/generate.mjs > ../bundle/licenses/THIRD_PARTY_LICENSES-web.txt
	curl -fsSL -o app/bundle/licenses/k3d.LICENSE "https://raw.githubusercontent.com/k3d-io/k3d/$(K3D_VERSION)/LICENSE"
	@if [ "$$(cat app/bundle/.bighole-ref 2>/dev/null)" != "$(BIGHOLE_REF)" ] || [ ! -f app/bundle/bighole.tar.gz ]; then \
	  echo "building Big Hole $(BIGHOLE_REF)"; \
	  docker run --rm -v "$(CURDIR)/app/bundle:/out" node:20-alpine sh -ec '\
	    apk add --no-cache git >/dev/null; git clone -q https://github.com/zelmario/Big-hole.git /src; cd /src; \
	    git checkout -q $(BIGHOLE_REF); npm ci --silent >/dev/null; npm run build --silent >/dev/null; \
	    cp LICENSE dist/LICENSE.txt; tar -czf /out/bighole.tar.gz -C dist .; cp LICENSE /out/.big-hole.LICENSE' && \
	  echo "$(BIGHOLE_REF)" > app/bundle/.bighole-ref; \
	fi
	cp app/bundle/.big-hole.LICENSE app/bundle/licenses/big-hole.LICENSE
	@du -sh app/bundle

## dbcanvas: build the dbcanvas binary for this machine into dist/dbcanvas — the web app,
## the relay forwarders, the catalogue and the bundle embedded, the version stamped from VERSION.
dbcanvas: relaybin catalog
	cd app/web && npm ci && npm run build
	$(MAKE) bundle
	mkdir -p dist
	cd app && CGO_ENABLED=0 go build -trimpath -buildvcs=false \
	  -ldflags="-s -w -X main.appVersion=$(VERSION)" -o ../dist/dbcanvas .
	@ls -l dist/dbcanvas

## relaybin: build the relay forwarder (app/cmd/dbcanvas-relay) for linux/amd64 and
## linux/arm64 into app/relaybin, gzipped, where DBCanvas embeds it (app/noderelay.go).
relaybin:
	@for arch in amd64 arm64; do \
	  ( cd app && GOOS=linux GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -buildvcs=false \
	      -ldflags="-s -w" -o relaybin/dbcanvas-relay-linux-$$arch ./cmd/dbcanvas-relay ) && \
	  gzip -9f app/relaybin/dbcanvas-relay-linux-$$arch || exit 1; \
	done; ls -l app/relaybin/*.gz

## images: every image a node can need, built here — the operating-system bases (systemd
## images for each OS × the one platform this installation targets, recorded in images.yaml),
## then the images baked on top of them (Intranet, VNC desktop, K3D diagnostics collector,
## Kubeadm node; images/service.sh), then the demo applications (Support, Stock Market,
## Ledger and Performance Sim, Pager Panic; images/apps.sh).
##
## It writes images.yaml and nothing else: versions.yaml, the catalog of what those
## bases can install, is `make versions` and is left alone here.
##
## The bases come first because everything else is built on them. After that, a failure
## is reported and the rest still builds: those builds reach Percona's repositories, npm,
## GitHub and Maven Central, so they can fail for reasons that have nothing to do with this
## machine, and one missing image should not stop `make install` from ending with DBCanvas
## up. A node whose image is missing says so at Validate; retry with its own target below
## (`make vnc-image`, `make stocksim-image`, …) or the Build button DBCanvas shows there.
## `make install-pull` fetches the same images from the registry instead of building them.
images:
	bash images/build.sh
	@failed=""; \
	bash images/service.sh all || failed="$$failed node"; \
	bash images/apps.sh || failed="$$failed application"; \
	if [ -n "$$failed" ]; then \
	  echo ""; \
	  echo "  Some$$failed images did not build (listed above). DBCanvas will still start;"; \
	  echo "  only the node types that need them are affected. Retry with the image's own"; \
	  echo "  target (e.g. 'make vnc-image', 'make stocksim-image') or the Build button at Validate."; \
	fi

## intranet-image: rebuild only the pre-baked Intranet image
## (dbcanvas-intranet:oraclelinux-9-<arch>) — the systemd Oracle Linux 9 base plus
## OpenLDAP, bind, Squid, postfix/dovecot and Roundcube, so deploying an Intranet
## node is configuration only. `make images` builds this too.
intranet-image:
	bash images/service.sh intranet

## vnc-image: rebuild only the pre-baked Ubuntu VNC image
## (dbcanvas-vnc:ubuntu-24.04-<arch>) — the systemd Ubuntu 24.04 base plus the XFCE
## desktop, TigerVNC/noVNC, Firefox and the Percona clients. `make images`
## builds this too.
vnc-image:
	bash images/service.sh vnc

## k8scollector-image: rebuild only the K3D diagnostics collector image
## (dbcanvas-k8scollector:debian-12-<arch>) — Debian 12 with pt-k8s-debug-collector,
## pt-mysql-summary and pt-mongodb-summary built from percona-toolkit's source, for
## DOCKER_PLATFORM. A K3D node's Diagnostics capture runs one throwaway
## container from this image against the cluster's kubeconfig. `make images` builds this too.
k8scollector-image:
	bash images/service.sh k8scollector

## kubeadm-image: rebuild only the Kubeadm Cluster node image
## (dbcanvas-kubeadm:ubuntu-24.04-<arch>) — Ubuntu 24.04 with systemd, containerd and the
## node tools, built for K8S_PLATFORM rather than DOCKER_PLATFORM: a Kubernetes node has
## to run on the host's native architecture. A DOCKER_PLATFORM given on the command line
## overrides it, so a release run builds one architecture of everything (unless K8S_PLATFORM
## is given there too). Kubernetes itself is installed at deploy, at the
## version the frame asks for. `make images` builds this too.
kubeadm-image:
	bash images/service.sh kubeadm

## versions: probe the images in images.yaml for the versions installable on each,
## plus the PMM, operator, Helm chart and k3s catalogs → versions.yaml, or
## versions-arm64.yaml when DOCKER_PLATFORM=linux/arm64 (one catalog per platform, so a
## probe on one never overwrites the other's). SLOW (a
## container and a pile of repository queries per image — hours, not minutes), which
## is why nothing else runs it: the repo ships a versions.yaml, and `make images` does not
## overwrite it. Run this when the pickers should offer versions released since the last
## probe, or after building a new OS image; commit the result, and dbcanvas users get it
## with `dbcanvas versions update`, a build of this tree with the binary.
## ONLY=percona / ONLY=upstream re-probes one group and keeps the other as recorded.
versions:
	bash images/versions.sh

## supportsim-image: build the MongoDB Vector Search Support Desk demo app image
## (first-party Go binary + embedded frontend + the all-MiniLM-L6-v2 weights,
## downloaded at build time) — a Support Sim node needs this.
supportsim-image:
	bash images/apps.sh supportsim

## stocksim-image: build the Stock Market Sim demo app image (first-party Go
## binary + embedded static frontend, no systemd) — a Stock Market Sim node
## needs this. It speaks several database engines, and
## can also be pointed at a database outside the stack entirely.
stocksim-image:
	bash images/apps.sh stocksim

## ledgersim-image: build the Ledger Sim demo app image — the JDBC one. A shaded
## jar (HikariCP + MySQL Connector/J + MariaDB Connector/J + pgJDBC) on an Eclipse
## Temurin JRE, not a Go binary, so this target is the slow one and needs Maven
## Central rather than the Go proxy. A Ledger Sim node needs this image.
ledgersim-image:
	bash images/apps.sh ledgersim

## perfsim-image: build the Performance Sim image (first-party Go binary +
## embedded dashboard, no systemd). A Performance Sim node needs this.
perfsim-image:
	bash images/apps.sh perfsim

## pagerpanic-image: build the Pager Panic image (first-party Go binary + embedded
## game page, no systemd). A Pager Panic node needs this.
pagerpanic-image:
	bash images/apps.sh pagerpanic
