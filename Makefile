.PHONY: build test lint fmt bot

build:
	go build -o bin/exitcheck ./cmd/exitcheck

test:
	go test -race ./...

lint:
	go vet ./...
	test -z "$$(gofmt -l .)"

fmt:
	gofmt -w .

# Runs the Telegram bot with .env loaded and Redis from compose.yaml.
bot: build
	docker compose up -d --wait redis
	set -a; [ -f .env ] && . ./.env; set +a; REDIS_URL=$${REDIS_URL:-redis://localhost:6379/0} ./bin/exitcheck bot
