SHELL := /bin/bash

# Load APP_PORT for echoing the URL (falls back to 8080).
APP_PORT ?= $(shell test -f .env && grep -E '^APP_PORT=' .env | cut -d= -f2 || echo 8080)

# The release this tree is. Stamped into both binaries so the app can say what it is
# and the What's New dialog knows whether an account has seen these notes.
VERSION ?= $(shell cat VERSION 2>/dev/null || echo dev)

# The platforms `make cli` cross-compiles for. Keep in sync with cliPlatforms in
# app/clidownload.go, which is what the API page offers for download.
CLI_PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64
# Where `make cli` installs this machine's build as dbcanvas-cli; empty = build only.
CLI_INSTALL_DIR ?= $(HOME)/.local/bin

.PHONY: doctor tag release-notes github-release pull-images publish-images install-pull install adopt-images compose env build up down logs restart rotate-key clean images versions smoke cli cli-test supportsim-image stocksim-image ledgersim-image perfsim-image pagerpanic-image intranet-image vnc-image kubeadm-image

## doctor: check the host side — Docker, compose, buildx, disk, .env passwords and exposure, the
## node images, whether the app answers. `dbcanvas-cli doctor` checks the installation itself.
doctor:
	@bash scripts/doctor.sh

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

## install-pull: a first install from pulled images — minutes instead of `make images`' hour.
install-pull: env pull-images compose

## publish-images: push this release's locally built node images to IMAGE_REGISTRY. This
## publishes, so no other target runs it.
publish-images:
	@bash scripts/registry.sh push

## install: everything a first run needs — every image a node can need (`make images`:
## the OS bases, the Intranet, VNC desktop, K3D collector and Kubeadm node, and the five
## demo applications), then DBCanvas itself.
## Safe to re-run; `make compose` alone is enough once the images exist.
##
## `make versions` is deliberately NOT part of this. It starts a container and queries
## every Percona, MariaDB, MySQL, Helm and registry endpoint for each image, which took
## people upwards of three hours — and it was here only because `make images` used to
## overwrite versions.yaml, so a first run had to re-probe what it had just deleted. The
## two files are now separate (images.yaml = what was built, versions.yaml = what is
## installable), so the catalog committed to this repo survives `make images` and the
## pickers are populated the moment DBCanvas comes up. Run `make versions` when you want
## it refreshed — newly released minors, or an OS image that was not in the matrix before.
##
## A failed image does not stop it: `make images` reports what failed, Validate says it
## again at the node that needs it, and it can be retried from there or its own target.
install: images compose

## adopt-images: give the images this host already built the current release's tag, instead
## of rebuilding them. Every image is tagged with its release (dbcanvas-systemd:…-amd64-v0.0.14)
## so installations sharing a Docker daemon cannot overwrite each other's; images built before
## that have no release in their tag. Run once after upgrading when they were built from this
## checkout. Nothing is rebuilt or removed.
adopt-images:
	@bash images/adopt.sh

## compose: create .env if needed, then build and start the stack
compose: env
	APP_VERSION=$(VERSION) docker compose up --build -d
	@echo ""
	@echo "  dbcanvas is up → http://localhost:$(APP_PORT)"
	@echo "  View logs:    make logs"
	@echo "  Stop:         make down"

## env: materialize .env from .env.example (only if missing), and make sure the two
## generated catalogs exist as files. Docker bind-mounts them into the container, and a
## bind mount whose source is missing is created by the daemon as a *directory* — after
## which every picker is empty until someone notices. An empty stand-in is recoverable;
## `make images` / `make versions` overwrite it.
##
## It also names the installation (DBCANVAS_INSTANCE, see docker-compose.yml): a new .env
## gets dbcanvas-<login>, so a second person on the same Docker host gets their own image,
## volumes and stack containers. A .env from before the setting existed gets "dbcanvas" —
## the name that installation already has — so its database is not swapped for an empty one.
env:
	@if [ ! -f .env ]; then \
	  cp .env.example .env; \
	  inst="dbcanvas-$$(printf '%s' "$${USER:-$$(id -un)}" | tr 'A-Z' 'a-z' | tr -c 'a-z0-9\n' '-' | sed 's/^-*//;s/-*$$//')"; \
	  sed -i.bak "s/^DBCANVAS_INSTANCE=.*/DBCANVAS_INSTANCE=$$inst/" .env && rm -f .env.bak; \
	  echo "Created .env from .env.example (instance: $$inst)"; \
	  bash scripts/gen-secrets.sh .env; \
	elif ! grep -qE '^DBCANVAS_INSTANCE=.+' .env; then \
	  sed -i.bak '/^DBCANVAS_INSTANCE=/d' .env && rm -f .env.bak; \
	  printf '\n# Added by make env: the name this installation already had (see .env.example).\nDBCANVAS_INSTANCE=dbcanvas\n' >> .env; \
	  echo "Set DBCANVAS_INSTANCE=dbcanvas in .env — this installation's existing name, so its data stays where it is"; \
	fi
	@for f in images.yaml versions.yaml; do 	  test -e $$f || { echo "images: []" >$$f; 	    echo "Created empty $$f — run 'make images' (and 'make versions') to fill it"; }; 	done

## build: build the image only
build: env
	APP_VERSION=$(VERSION) docker compose build

## up: start containers (no rebuild)
up: env
	APP_VERSION=$(VERSION) docker compose up -d

## down: stop and remove containers
down:
	docker compose down

## restart: recreate the stack
restart: down compose

## rotate-key: re-encrypt the database's passwords and secrets under a new key. Stops
## the app, re-seals everything in one transaction, keeps the old key as .old on the
## key volume, and starts the app again.
rotate-key:
	docker compose stop app
	docker compose run --rm --no-deps app -rotate-encryption-key
	docker compose start app

## logs: follow application logs
logs:
	docker compose logs -f

## clean: stop stack and remove the built image
clean:
	docker compose down --rmi local --remove-orphans
	rm -rf dist

## smoke: render the React components off-browser and fail on any render error,
## and check the canvas can actually reach every link target the backend accepts.
## Then mount every heavy page in headless Chrome, which is the half the off-browser
## pass cannot see: effects do not run under SSR, and an effect that throws blanks
## the page. Skipped with a notice where there is no Chrome — the off-browser pass
## is the hard gate.
smoke:
	cd app/web && npm run smoke && npm run smoke:browser

## cli: cross-compile dbcanvas-cli into dist/ for every platform the app image
## ships, with a SHA256SUMS beside them, and install this machine's build as
## $(CLI_INSTALL_DIR)/dbcanvas-cli — the command every example uses. A single
## static binary per platform, no dependencies; then run `dbcanvas-cli login`.
##
## CLI_INSTALL_DIR defaults to ~/.local/bin, which needs no sudo; set it to put the
## binary elsewhere (`sudo make cli CLI_INSTALL_DIR=/usr/local/bin`), or to an empty
## value to only build.
##
## The app image builds the same matrix (see app/Dockerfile) so a running
## installation can hand the binary to somebody with no checkout; this target is
## for building it here.
cli:
	@mkdir -p dist
	@for p in $(CLI_PLATFORMS); do \
	  os=$${p%/*}; arch=$${p#*/}; out=dist/dbcanvas-cli_$${os}_$${arch}; \
	  if [ "$$os" = "windows" ]; then out=$$out.exe; fi; \
	  echo "  $$os/$$arch → $$out"; \
	  ( cd cli && GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -buildvcs=false \
	      -ldflags="-s -w -X main.version=$(VERSION)" -o ../$$out . ) || exit 1; \
	done
	@cd dist && (command -v sha256sum >/dev/null && sha256sum dbcanvas-cli_* > SHA256SUMS \
	             || shasum -a 256 dbcanvas-cli_* > SHA256SUMS)
	@echo ""
	@echo "  dbcanvas-cli $(VERSION) → dist/"
	@if [ -n "$(CLI_INSTALL_DIR)" ]; then \
	  host=dist/dbcanvas-cli_$$(go env GOHOSTOS)_$$(go env GOHOSTARCH); \
	  if [ ! -f "$$host" ]; then \
	    echo "  No build for this machine ($$(go env GOHOSTOS)/$$(go env GOHOSTARCH)) in CLI_PLATFORMS; nothing installed."; \
	  else \
	    mkdir -p "$(CLI_INSTALL_DIR)" && install -m 0755 "$$host" "$(CLI_INSTALL_DIR)/dbcanvas-cli" && \
	    echo "  Installed $$host → $(CLI_INSTALL_DIR)/dbcanvas-cli"; \
	    case ":$$PATH:" in *":$(CLI_INSTALL_DIR):"*) ;; \
	      *) echo "  $(CLI_INSTALL_DIR) is not on your PATH — add it, or run $(CLI_INSTALL_DIR)/dbcanvas-cli";; esac; \
	    if [ -e "$(CLI_INSTALL_DIR)/dbcanvas" ]; then \
	      echo "  An older copy named dbcanvas is still in $(CLI_INSTALL_DIR); the command is dbcanvas-cli now, so it can go."; \
	    fi; \
	  fi; \
	fi

## cli-test: build, vet and test the CLI module
cli-test:
	cd cli && go build ./... && go vet ./... && go test ./...

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
## (dbcanvas-k8scollector:debian-12-amd64) — Debian 12 plus percona-toolkit, which
## carries pt-k8s-debug-collector along with pt-mysql-summary and
## pt-mongodb-summary. A K3D node's Diagnostics capture runs one throwaway
## container from this image against the cluster's kubeconfig. amd64 only:
## Percona's apt repo has no arm64 percona-toolkit. `make images` builds this too.
k8scollector-image:
	bash images/service.sh k8scollector

## kubeadm-image: rebuild only the Kubeadm Cluster node image
## (dbcanvas-kubeadm:ubuntu-24.04-<arch>) — Ubuntu 24.04 with systemd, containerd and the
## node tools, built for K8S_PLATFORM rather than DOCKER_PLATFORM: a Kubernetes node has to
## run on the host's native architecture. Kubernetes itself is installed at deploy, at the
## version the frame asks for. `make images` builds this too.
kubeadm-image:
	bash images/service.sh kubeadm

## versions: probe the images in images.yaml for the versions installable on each,
## plus the PMM, operator, Helm chart and k3s catalogs → versions.yaml. SLOW (a
## container and a pile of repository queries per image — hours, not minutes), which
## is why `make install` does not run it: the repo ships a versions.yaml, and
## `make images` no longer overwrites it. Run this when the pickers should offer
## versions released since the last probe, or after building a new OS image.
## ONLY=percona / ONLY=upstream re-probes one group and keeps the other as recorded.
## Restart DBCanvas afterwards (make restart): the file is replaced, not rewritten, and a
## running app's bind mount cannot see the new one — every version picker is empty until
## the container is recreated. `make compose` alone does not recreate it.
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
