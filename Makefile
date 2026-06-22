SHELL := /bin/bash

# Load APP_PORT for echoing the URL (falls back to 8080).
APP_PORT ?= $(shell test -f .env && grep -E '^APP_PORT=' .env | cut -d= -f2 || echo 8080)

.PHONY: compose env build up down logs restart clean images

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

## images: build systemd base images (OS × platform matrix) → versions.yaml
images:
	bash images/build.sh
