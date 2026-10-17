package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"
)

// Without a host default handler go-telegram/bot prints every unmatched update
// with log.Printf("%+v"), message text and user names included.
func TestUnhandledLogsOnlyKind(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	unhandled(log)(t.Context(), nil, &models.Update{ID: 5, Message: &models.Message{
		Text: "private words", From: &models.User{ID: 9, Username: "secretname"},
		Sticker: &models.Sticker{FileID: "s"},
	}})
	out := buf.String()
	if !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "kind=message") || !strings.Contains(out, "user_id=9") {
		t.Fatalf("log = %q, want a debug line with kind and user id", out)
	}
	if strings.Contains(out, "private words") || strings.Contains(out, "secretname") {
		t.Fatalf("log leaks message content: %q", out)
	}
}
