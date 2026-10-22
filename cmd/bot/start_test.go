package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-telegram/bot"
)

// TestRunStartsAndStops wires every module against a fake Bot API, waits for
// the first getUpdates (the bot is serving) and stops it: a wiring or
// migration error that unit tests miss fails here instead of at deploy.
func TestRunStartsAndStops(t *testing.T) {
	var polled, commands atomic.Bool
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		var result any = true
		switch method {
		case "getMe":
			result = map[string]any{"id": 999, "is_bot": true, "first_name": "Memes", "username": "meme_test_bot"}
		case "setMyCommands":
			commands.Store(true)
		case "getUpdates":
			polled.Store(true)
			// Long polling: hold the request like Telegram does, until the bot gives up.
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
			result = []any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	defer tg.Close()

	dir := t.TempDir()
	environ := []string{
		"TELEGRAM_BOT_TOKEN=123:fake",
		"MEME_SEARCH_URL=http://127.0.0.1:1",
		"MEME_SEARCH_API_TOKEN=fake",
		"CACHE_CHAT_ID=-100123",
		"SAVE_USER_IDS=1",
		"SAVE_DIR=" + filepath.Join(dir, "memes"),
		"DB_PATH=" + filepath.Join(dir, "bot.db"),
		"WARMUP_INTERVAL=1h",
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- run(ctx, environ, dir, bot.WithServerURL(tg.URL)) }()

	deadline := time.Now().Add(10 * time.Second)
	for !polled.Load() {
		select {
		case err := <-done:
			t.Fatalf("run returned before serving: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("bot never polled for updates")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !commands.Load() {
		t.Error("the command menu was not set before serving")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after cancel")
	}
}
