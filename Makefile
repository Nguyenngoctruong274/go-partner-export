.DEFAULT_GOAL := help
BIN := bin/exportd

# Local defaults match docker-compose.yml. Credentials here are for local use only.
export PX_DATABASE_URL ?= postgres://partner:partner@localhost:5432/partner?sslmode=disable
export PX_BLOB_DRIVER ?= fs
export PX_BLOB_FS_ROOT ?= ./data/blob
export PX_SFTP_HOST ?= localhost
export PX_SFTP_PORT ?= 2222
export PX_SFTP_USER ?= partner
export PX_SFTP_AUTH ?= password
export PX_SFTP_PASSWORD ?= partner
export PX_SFTP_INSECURE_SKIP_HOST_KEY ?= true

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the exportd binary
	go build -trimpath -o $(BIN) ./cmd/exportd

.PHONY: run
run: ## Run exportd locally (needs `make up`)
	go run ./cmd/exportd

.PHONY: test
test: ## Run unit tests with the race detector
	go test -race -count=1 ./...

.PHONY: test-db
test-db: ## Run all tests, including PostgreSQL ones (needs `make up`)
	PX_TEST_DATABASE_URL=postgres://partner:partner@localhost:5432/partner_test?sslmode=disable go test -race -count=1 ./...

.PHONY: cover
cover: ## Run tests and print total coverage
	go test -race -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

.PHONY: fmt
fmt: ## Format code
	gofmt -s -w .

.PHONY: up
up: ## Start local dependencies (PostgreSQL, SFTP, fake GCS)
	docker compose up -d

.PHONY: stack
stack: ## Run exportd and its dependencies in Docker
	docker compose --profile stack up --build

.PHONY: down
down: ## Stop everything
	docker compose --profile stack down
