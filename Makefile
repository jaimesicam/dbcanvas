SHELL := /bin/bash

# Load APP_PORT for echoing the URL (falls back to 8080).
APP_PORT ?= $(shell test -f .env && grep -E '^APP_PORT=' .env | cut -d= -f2 || echo 8080)

# The release this tree is. Stamped into both binaries so the app can say what it is
# and the What's New dialog knows whether an account has seen these notes.
VERSION ?= $(shell cat VERSION 2>/dev/null || echo dev)

# The platforms `make cli` cross-compiles for. Keep in sync with cliPlatforms in
# app/clidownload.go, which is what the API page offers for download.
CLI_PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64

.PHONY: install compose env build up down logs restart clean images versions smoke cli cli-test trafficsim-image hotelsim-image airlinesim-image carsim-image marketchaos-image stocksim-image intranet-image vnc-image

## install: everything a first run needs — build the node images, discover the
## versions they can install, then build and start DBCanvas itself. Safe to re-run;
## `make compose` alone is enough once the images exist.
install: images versions compose

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
## and check the canvas can actually reach every link target the backend accepts
smoke:
	cd app/web && npm run smoke

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

## images: everything a node can need — the systemd base images (OS × the one
## platform this installation targets) → versions.yaml, then the pre-baked service
## images (Intranet, Ubuntu VNC) built from them, then the demo application images
## (Traffic/Hotel/Airline/Car Rental/MarketChaos/Stock Market Sim).
images:
	bash images/build.sh

## intranet-image: rebuild only the pre-baked Intranet image
## (dbcanvas-intranet:oraclelinux-9-<arch>) — the systemd Oracle Linux 9 base plus
## OpenLDAP, bind, Squid, postfix/dovecot and Roundcube, so deploying an Intranet
## node is configuration only. `make images` builds this too.
intranet-image:
	bash images/service.sh intranet

## vnc-image: rebuild only the pre-baked Ubuntu VNC image
## (dbcanvas-vnc:ubuntu-24.04-<arch>) — the systemd Ubuntu 24.04 base plus the XFCE
## desktop, TigerVNC/noVNC, Firefox and the Percona clients. `make images` builds
## this too.
vnc-image:
	bash images/service.sh vnc

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
