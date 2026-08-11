SHELL := /bin/bash

# Load APP_PORT for echoing the URL (falls back to 8080).
APP_PORT ?= $(shell test -f .env && grep -E '^APP_PORT=' .env | cut -d= -f2 || echo 8080)

.PHONY: compose env build up down logs restart clean images versions smoke trafficsim-image hotelsim-image airlinesim-image carsim-image marketchaos-image stocksim-image

## compose: create .env if needed, then build and start the stack
compose: env
	docker compose up --build -d
	@echo ""
	@echo "  dbcanvas is up → http://localhost:$(APP_PORT)"
	@echo "  View logs:    make logs"
	@echo "  Stop:         make down"

## env: materialize .env from .env.example (only if missing)
env:
	@test -f .env || { cp .env.example .env && echo "Created .env from .env.example"; }

## build: build the image only
build: env
	docker compose build

## up: start containers (no rebuild)
up: env
	docker compose up -d

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

## smoke: render the React components off-browser and fail on any render error,
## and check the canvas can actually reach every link target the backend accepts
smoke:
	cd app/web && npm run smoke

## images: build systemd base images (OS × platform matrix) → versions.yaml
images:
	bash images/build.sh

## versions: probe built images for installable Percona Server versions → versions.yaml
versions:
	bash images/versions.sh

## trafficsim-image: build the Valkey Traffic Lab demo app image (first-party Go
## binary + embedded static frontend, no systemd) — a Traffic Sim node needs this.
trafficsim-image:
	docker build -t dbcanvas-trafficsim:latest trafficsim/

## hotelsim-image: build the MongoDB Hotel Reservation Lab demo app image
## (first-party Go binary + embedded static frontend, no systemd) — a Hotel Sim
## node needs this.
hotelsim-image:
	docker build -t dbcanvas-hotelsim:latest hotelsim/

## airlinesim-image: build the MySQL Airline Reservation Lab demo app image
## (first-party Go binary + embedded static frontend, no systemd) — an Airline Sim
## node needs this.
airlinesim-image:
	docker build -t dbcanvas-airlinesim:latest airlinesim/

## carsim-image: build the PostgreSQL Car Rental Lab demo app image (first-party
## Go binary + embedded static frontend, no systemd) — a Car Rental Sim node
## needs this.
carsim-image:
	docker build -t dbcanvas-carsim:latest carsim/

## marketchaos-image: build the "Unoptimized MySQL Challenge" (MarketChaos)
## stock-exchange performance-troubleshooting demo app image (first-party Go
## binary + embedded static frontend, no systemd) — an Unoptimized MySQL
## Challenge node needs this.
marketchaos-image:
	docker build -t dbcanvas-marketchaos:latest marketchaos/

## stocksim-image: build the Stock Market Sim demo app image (first-party Go
## binary + embedded static frontend, no systemd) — a Stock Market Sim node
## needs this. Unlike its sibling sims it speaks several database engines, and
## can also be pointed at a database outside the stack entirely.
stocksim-image:
	docker build -t dbcanvas-stocksim:latest stocksim/
