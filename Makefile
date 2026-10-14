.DEFAULT_GOAL := help

DOCKER        ?= docker
GOLANGCI_LINT ?= golangci-lint
REDOCLY       ?= npx --yes @redocly/cli@2.61.0
COMPOSE       := $(DOCKER) compose -f docker-compose.dev.yml
BIN           := bin/backend

.PHONY: help run build test test-integration lint fmt vuln migrate openapi openapi-lint \
        db-up db-down stack-up stack-down stack-logs media inbox env

help: ## Show this help
	@grep -hE '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-18s %s\n", $$1, $$2}'

env: ## Create .env from .env.example if missing
	@test -f .env || { cp .env.example .env && echo "created .env from .env.example"; }

run: env ## Run the backend on the host against the dev stack
	go run ./cmd/backend serve

build: ## Build the binary into bin/
	CGO_ENABLED=0 go build -trimpath -o $(BIN) ./cmd/backend

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

stack-down: ## Stop the dev stack (data volume is kept)
	$(COMPOSE) --profile app down

stack-logs: ## Follow the dev stack logs
	$(COMPOSE) --profile app logs -f

media: ## Generate test MP3 files into .dev/media
	dev/scripts/gen-media.sh ./.dev/media

inbox: ## Generate inbox files with known quality issues into .dev/media/incoming
	dev/scripts/gen-inbox.sh ./.dev/media
