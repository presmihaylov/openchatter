SHELL := /bin/bash
export GOBIN := $(CURDIR)/bin

.PHONY: build run test e2e lint db-up db-down db-reset migrate-new tidy

build:
	go build -o bin/openchatterd ./cmd/openchatterd
	go build -o bin/openchatter ./cmd/openchatter

run: build db-up
	set -a && source .env && set +a && ./bin/openchatterd

test:
	go test ./...

e2e: build db-up
	./scripts/e2e.sh

lint:
	go vet ./...
	@command -v staticcheck >/dev/null && staticcheck ./... || echo "staticcheck not installed; skipping"

db-up:
	docker compose up -d --wait db

db-down:
	docker compose down

db-reset:
	docker compose down -v && docker compose up -d --wait db

# usage: make migrate-new name=add_foo
migrate-new:
	migrate create -ext sql -dir migrations -seq $(name)

tidy:
	go mod tidy

## ui-smoke: headless-browser smoke test of the web UI (needs Chrome + `npm i puppeteer-core` next to scripts/ui-smoke.js)
ui-smoke:
	node scripts/ui-smoke.js
