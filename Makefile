.DEFAULT_GOAL := help

DOCKER        ?= docker
GOLANGCI_LINT ?= golangci-lint
REDOCLY       ?= npx --yes @redocly/cli@2.61.0
COMPOSE       := $(DOCKER) compose -f docker-compose.dev.yml
BIN           := bin/backend
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE         ?= radio-atlantida-backend:dev

.PHONY: help run build test test-integration lint fmt vuln migrate openapi openapi-lint \
        image db-up db-down stack-up stack-down stack-logs media import-samples live-check env \
        monitoring-up monitoring-down monitoring-reset

help: ## Show this help
	@grep -hE '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

env: ## Create .env from .env.example if missing
	@test -f .env || { cp .env.example .env && echo "created .env from .env.example"; }

run: env ## Run the backend on the host against the dev stack
	go run ./cmd/backend serve

build: ## Build the binary into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags="-X main.version=$(VERSION)" -o $(BIN) ./cmd/backend

image: ## Build the runtime image locally ($(IMAGE), version from git describe)
	$(DOCKER) build -f docker/Dockerfile -t $(IMAGE) \
		--build-arg VERSION=$(VERSION) --build-arg REVISION=$$(git rev-parse HEAD) \
		--build-arg CREATED=$$(date -u +%Y-%m-%dT%H:%M:%SZ) .

test: ## Unit tests (race detector on)
	go test -race ./...

test-integration: ## Unit + integration tests (needs Docker; uses testcontainers)
	go test -race -tags integration ./...

lint: ## go vet + golangci-lint
	go vet ./...
	go vet -tags integration ./...
	$(GOLANGCI_LINT) run ./...

fmt: ## Format with gofumpt and goimports
	$(GOLANGCI_LINT) fmt ./...

vuln: ## Check dependencies for known vulnerabilities
	govulncheck ./...

migrate: env ## Apply database migrations (backend migrate up)
	go run ./cmd/backend migrate up

openapi: ## Write the OpenAPI specification to api/openapi.json
	@mkdir -p api
	go run ./cmd/backend openapi > api/openapi.json

openapi-lint: openapi ## Validate api/openapi.json (Redocly CLI through npx; needs Node.js)
	$(REDOCLY) lint --config redocly.yaml api/openapi.json

db-up: env ## Start only PostgreSQL
	$(COMPOSE) up -d --wait db

db-down: ## Stop PostgreSQL
	$(COMPOSE) stop db

stack-up: env ## Start db, icecast, liquidsoap and wait until healthy
	@mkdir -p .dev/media .dev/journal .dev/logs
	$(COMPOSE) up -d --wait db icecast liquidsoap

stack-down: ## Stop the dev stack (data volumes are kept)
	$(COMPOSE) --profile app --profile monitoring down

stack-logs: ## Follow the dev stack logs
	$(COMPOSE) --profile app --profile monitoring logs -f

monitoring-up: env ## Start Grafana (database view, log search) on 127.0.0.1:3000 with Loki and Alloy; starts db if needed
	@mkdir -p .dev/journal .dev/logs
	$(COMPOSE) --profile monitoring up -d --wait grafana-role loki alloy grafana

monitoring-down: ## Stop Grafana, Loki, and Alloy only
	$(COMPOSE) --profile monitoring stop grafana alloy loki

monitoring-reset: ## Remove Grafana's, Loki's, and Alloy's volumes (new admin password; logs reloaded from the files)
	$(COMPOSE) --profile monitoring rm -s -f grafana alloy loki
	$(DOCKER) volume rm -f radio-atlantida-dev_grafana-data radio-atlantida-dev_loki-data radio-atlantida-dev_alloy-data
	@echo "run make monitoring-up to start Grafana with the password from .env and reload the log files"

media: ## Generate test MP3 files into .dev/media
	dev/scripts/gen-media.sh ./.dev/media

import-samples: ## Generate files with known quality issues into .dev/media/import-samples
	dev/scripts/gen-import-samples.sh ./.dev/media

live-check: ## Go live through a running backend with a test tone (WebRTC, no browser)
	dev/scripts/gen-live.sh ./.dev/live-check.ogg
	go run ./dev/livecheck -file ./.dev/live-check.ogg
