# --- STAGE 1: build ---
# Runs on the builder's native platform and cross-compiles: the sqlite driver is pure Go,
# so the compile needs no QEMU emulation. The runtime stage below still does for a foreign platform.
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" -o /out/bot ./cmd/bot

# --- STAGE 2: runtime ---
FROM alpine:3.21

# ca-certificates for HTTPS to Telegram, tzdata so TZ (day boundary of the daily limits) works.
RUN apk add --no-cache ca-certificates tzdata

# The uid is pinned (not auto-allocated): files the bot saves into the shared memes folder must be
# owned by the same uid as meme-search's upload folder (it uses 1000), and an existing /data volume
# must stay writable across rebuilds.
ARG APP_UID=1000
RUN adduser -D -H -u ${APP_UID} bot \
    && mkdir -p /data /memes /config \
    && chown bot /data /memes /config

COPY --from=builder /out/bot /usr/local/bin/bot

ENV DB_PATH=/data/bot.db \
    SAVE_DIR=/memes \
    LIBRARY_DIR=/memes \
    CONFIG_FILE=/config/config.yml

VOLUME ["/data"]

USER bot
WORKDIR /data

ENTRYPOINT ["bot"]
