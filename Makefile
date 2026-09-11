SHELL := /bin/bash

# Load APP_PORT for echoing the URL (falls back to 8080).
APP_PORT ?= $(shell test -f .env && grep -E '^APP_PORT=' .env | cut -d= -f2 || echo 8080)

# The release this tree is. Stamped into both binaries so the app can say what it is
# and the What's New dialog knows whether an account has seen these notes.
VERSION ?= $(shell cat VERSION 2>/dev/null || echo dev)

# The platforms `make cli` cross-compiles for. Keep in sync with cliPlatforms in
# app/clidownload.go, which is what the API page offers for download.
CLI_PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: install install-extras compose env build up down logs restart clean images extra-images versions smoke cli cli-test trafficsim-image hotelsim-image airlinesim-image carsim-image marketchaos-image stocksim-image intranet-image vnc-image

## install: everything a first run needs — every image DBCanvas can build (the OS
## bases and the Intranet, then the optional ones on top: the VNC desktop, the K3D
## collector, the Big Hole FTDC viewer and the six demo apps), the version
## catalog those bases yield, then DBCanvas itself. Safe to re-run; `make compose`
## alone is enough once the images exist.
##
## The optional half runs through install-extras rather than extra-images so that a
## failure in somebody else's npm registry, GitHub or Percona repo cannot stop a
## first run from ending with DBCanvas up: what failed is reported here, said again
## at Validate by the node that needs it, and can be retried from the web interface
## or the per-image target. `make extra-images` on its own still fails loudly.
install: images versions install-extras compose

## install-extras: the optional images, built the way `make install` wants them —
## every one of them, and a failure reported rather than fatal. Not the target to
## use by hand; that is `make extra-images`.
install-extras:
	@bash images/service.sh all || { \
	  echo ""; \
	  echo "  Some images above did not build — DBCanvas will still start, and only the"; \
	  echo "  node types that need them are affected."; \
	  echo "  Retry with 'make extra-images', or from the Build button DBCanvas shows at Validate."; \
	}
	@bash images/apps.sh || { \
	  echo ""; \
	  echo "  Some demo application images did not build — the rest of DBCanvas is unaffected."; \
	  echo "  Retry with 'make extra-images' or the per-app target (e.g. 'make trafficsim-image')."; \
	}

## compose: create .env if needed, then build and start the stack
compose: env
	APP_VERSION=$(VERSION) docker compose up --build -d
	@echo ""
	@echo "  dbcanvas is up → http://localhost:$(APP_PORT)"
	@echo "  View logs:    make logs"
	@echo "  Stop:         make down"

## env: materialize .env from .env.example (only if missing)
env:
	@test -f .env || { cp .env.example .env && echo "Created .env from .env.example"; }

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
## ships, with a SHA256SUMS beside them. A single static binary per platform, no
## dependencies — put one on your PATH and run `dbcanvas login`.
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
	  ( cd cli && GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath \
	      -ldflags="-s -w -X main.version=$(VERSION)" -o ../$$out . ) || exit 1; \
	done
	@cd dist && (command -v sha256sum >/dev/null && sha256sum dbcanvas-cli_* > SHA256SUMS \
	             || shasum -a 256 dbcanvas-cli_* > SHA256SUMS)
	@echo ""
	@echo "  dbcanvas-cli $(VERSION) → dist/"
	@echo "  Install: install dist/dbcanvas-cli_$$(go env GOOS)_$$(go env GOARCH) /usr/local/bin/dbcanvas"

## cli-test: build, vet and test the CLI module
cli-test:
	cd cli && go build ./... && go vet ./... && go test ./...

## images: what every stack needs — the operating-system bases (systemd images for
## each OS × the one platform this installation targets, recorded in versions.yaml)
## and the pre-baked Intranet, baked onto the Oracle Linux 9 base.
##
## The Intranet is here rather than in extra-images because it is not an extra: it is
## the DNS and the CA the rest of a stack is built against, so a canvas with no
## Intranet node is a canvas where almost nothing validates. Everything genuinely
## optional stays in extra-images.
##
## Its build is the one part of this that reaches Percona's repositories, so a
## failure there is reported and tolerated rather than taking the OS bases and the
## rest of `make install` down with it — DBCanvas still starts, Validate says which
## image is missing, and an admin can build it from the Build button under that error
## or come back here for `make intranet-image`.
images:
	bash images/build.sh
	@bash images/service.sh intranet || { \
	  echo ""; \
	  echo "  The Intranet image did not build — the OS bases above are fine and DBCanvas will start."; \
	  echo "  Retry with 'make intranet-image', or from the Build button DBCanvas shows at Validate."; \
	}

## extra-images: everything OPTIONAL built on top of the bases — the pre-baked Ubuntu
## VNC image, the Big Hole FTDC viewer, the K3D diagnostics collector, and the six
## demo applications (Traffic/Hotel/Airline/Car Rental/MarketChaos/Stock Market Sim). `make install` builds these too (through
## install-extras, which tolerates a failure); this is the target for rebuilding them
## on their own, and it fails if any of them fails. Kept separate from `make images`
## because these reach into npm, GitHub and Percona's repos, so they fail for reasons
## that have nothing to do with your machine. A node whose image is missing says so at
## Validate, and an admin can build the ones with no local build context from the web
## interface rather than coming back here.
##
## It runs service.sh over the whole set, Intranet included, so this one command still
## guarantees every image above the bases exists; a good Intranet image is already
## built and costs a cached no-op.
extra-images:
	bash images/service.sh all
	bash images/apps.sh

## intranet-image: rebuild only the pre-baked Intranet image
## (dbcanvas-intranet:oraclelinux-9-<arch>) — the systemd Oracle Linux 9 base plus
## OpenLDAP, bind, Squid, postfix/dovecot and Roundcube, so deploying an Intranet
## node is configuration only. `make images` builds this too.
intranet-image:
	bash images/service.sh intranet

## vnc-image: rebuild only the pre-baked Ubuntu VNC image
## (dbcanvas-vnc:ubuntu-24.04-<arch>) — the systemd Ubuntu 24.04 base plus the XFCE
## desktop, TigerVNC/noVNC, Firefox and the Percona clients. `make extra-images`
## builds this too.
vnc-image:
	bash images/service.sh vnc

## k8scollector-image: rebuild only the K3D diagnostics collector image
## (dbcanvas-k8scollector:debian-12-amd64) — Debian 12 plus percona-toolkit, which
## carries pt-k8s-debug-collector along with pt-mysql-summary and
## pt-mongodb-summary. A K3D node's Diagnostics capture runs one throwaway
## container from this image against the cluster's kubeconfig. amd64 only:
## Percona's apt repo has no arm64 percona-toolkit. `make extra-images` builds this too.
k8scollector-image:
	bash images/service.sh k8scollector

## bighole-image: rebuild only the Big Hole image (dbcanvas-bighole:<commit>) — a
## third-party browser-only MongoDB FTDC viewer (github.com/zelmario/Big-hole),
## built from source at a pinned commit and served by nginx. A Big Hole node needs
## it. `make extra-images` builds this too.
bighole-image:
	bash images/service.sh bighole

## versions: probe built images for installable Percona Server versions → versions.yaml
versions:
	bash images/versions.sh

## trafficsim-image: build the Valkey Traffic Lab demo app image (first-party Go
## binary + embedded static frontend, no systemd) — a Traffic Sim node needs this.
trafficsim-image:
	bash images/apps.sh trafficsim

## hotelsim-image: build the MongoDB Hotel Reservation Lab demo app image
## (first-party Go binary + embedded static frontend, no systemd) — a Hotel Sim
## node needs this.
hotelsim-image:
	bash images/apps.sh hotelsim

## airlinesim-image: build the MySQL Airline Reservation Lab demo app image
## (first-party Go binary + embedded static frontend, no systemd) — an Airline Sim
## node needs this.
airlinesim-image:
	bash images/apps.sh airlinesim

## carsim-image: build the PostgreSQL Car Rental Lab demo app image (first-party
## Go binary + embedded static frontend, no systemd) — a Car Rental Sim node
## needs this.
carsim-image:
	bash images/apps.sh carsim

## marketchaos-image: build the "Unoptimized MySQL Challenge" (MarketChaos)
## stock-exchange performance-troubleshooting demo app image (first-party Go
## binary + embedded static frontend, no systemd) — an Unoptimized MySQL
## Challenge node needs this.
marketchaos-image:
	bash images/apps.sh marketchaos

## stocksim-image: build the Stock Market Sim demo app image (first-party Go
## binary + embedded static frontend, no systemd) — a Stock Market Sim node
## needs this. Unlike its sibling sims it speaks several database engines, and
## can also be pointed at a database outside the stack entirely.
stocksim-image:
	bash images/apps.sh stocksim
