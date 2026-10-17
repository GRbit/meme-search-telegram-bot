.PHONY: test lint vet run build

test:
	go test -race ./...

vet:
	go vet ./...

lint: vet
	golangci-lint run ./...

build:
	CGO_ENABLED=0 go build -o bin/meme-search-telegram-bot ./cmd/bot

# Reads secrets from .env.local (gitignored). Runs the built binary rather than
# go run: default paths (data/, memes/) are relative to the executable, and go
# run's executable lives in a temporary build directory.
run: build
	set -a; . ./.env.local; set +a; ./bin/meme-search-telegram-bot
