.PHONY: test lint vet run build

test:
	go test -race ./...

vet:
	go vet ./...

lint: vet
	golangci-lint run ./...

build:
	CGO_ENABLED=0 go build -o bin/meme-search-telegram-bot ./cmd/bot

# Reads secrets from .env.local (gitignored).
run:
	set -a; . ./.env.local; set +a; go run ./cmd/bot
