package module_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/go-telegram/bot"

	"github.com/grbit/meme-search-telegram-bot/i18n"
	"github.com/grbit/meme-search-telegram-bot/module"
)

type cmdModule struct{ cmds []module.Command }

func (cmdModule) Register(*bot.Bot)            {}
func (c cmdModule) Commands() []module.Command { return c.cmds }

type plainModule struct{}

func (plainModule) Register(*bot.Bot) {}

// setMyCommandsCall is one recorded setMyCommands request.
type setMyCommandsCall struct {
	scope    string // scope type
	lang     string
	commands map[string]string // name -> description
	order    []string
}

func fakeCommandsAPI(t *testing.T, fail bool) (*bot.Bot, func() []setMyCommandsCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []setMyCommandsCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/setMyCommands") {
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("form: %v", err)
		}
		var c setMyCommandsCall
		var scope struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(r.FormValue("scope")), &scope)
		c.scope, c.lang = scope.Type, r.FormValue("language_code")
		var cmds []struct{ Command, Description string }
		_ = json.Unmarshal([]byte(r.FormValue("commands")), &cmds)
		c.commands = map[string]string{}
		for _, x := range cmds {
			c.commands[x.Command] = x.Description
			c.order = append(c.order, x.Command)
		}
		mu.Lock()
		calls = append(calls, c)
		mu.Unlock()
		if fail {
			_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: nope"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
	}))
	t.Cleanup(srv.Close)
	b, err := bot.New("1:x", bot.WithServerURL(srv.URL), bot.WithSkipGetMe())
	if err != nil {
		t.Fatal(err)
	}
	return b, func() []setMyCommandsCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]setMyCommandsCall(nil), calls...)
	}
}

func testBundle(t *testing.T) *i18n.Bundle {
	t.Helper()
	b, err := i18n.NewBundle("en", nil, fstest.MapFS{
		"en.yaml": {Data: []byte("t.start: \"Start\"\nt.help: \"Help\"\nt.scope: \"Group scope\"\n")},
		"ru.yaml": {Data: []byte("t.start: \"Старт\"\nt.help: \"Помощь\"\nt.scope: \"Область группы\"\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSetCommands(t *testing.T) {
	b, calls := fakeCommandsAPI(t, false)
	mods := []module.Module{
		plainModule{},
		cmdModule{[]module.Command{
			{Name: "start", DescriptionID: "t.start", Scope: module.ScopePrivate},
			{Name: "help", DescriptionID: "t.help", Scope: module.ScopePrivate},
			{Name: "memescope", DescriptionID: "t.scope", Scope: module.ScopeGroups},
		}},
	}
	module.SetCommands(t.Context(), b, testBundle(t), nil, mods...)
	got := calls()
	// Default list (no language_code) plus one per shipped language, for each scope.
	if len(got) != 6 {
		t.Fatalf("calls = %+v", got)
	}
	for _, c := range got {
		switch c.scope {
		case "all_private_chats":
			if strings.Join(c.order, ",") != "start,help" {
				t.Errorf("private commands = %v", c.order)
			}
			want := map[string]string{"": "Start", "en": "Start", "ru": "Старт"}[c.lang]
			if c.commands["start"] != want {
				t.Errorf("lang %q start = %q, want %q", c.lang, c.commands["start"], want)
			}
		case "all_group_chats":
			if strings.Join(c.order, ",") != "memescope" {
				t.Errorf("group commands = %v", c.order)
			}
		default:
			t.Errorf("unexpected scope %q", c.scope)
		}
	}
}

// Nothing to show means no request: an empty list would also wipe commands
// another host set for that scope.
func TestSetCommandsSkipsEmptyScopes(t *testing.T) {
	b, calls := fakeCommandsAPI(t, false)
	module.SetCommands(t.Context(), b, testBundle(t), nil, plainModule{},
		cmdModule{[]module.Command{{Name: "start", DescriptionID: "t.start", Scope: module.ScopePrivate}}})
	for _, c := range calls() {
		if c.scope != "all_private_chats" {
			t.Fatalf("call for scope %q", c.scope)
		}
	}
}

type warnCounter struct {
	mu sync.Mutex
	n  int
}

func (h *warnCounter) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelWarn }
func (h *warnCounter) Handle(context.Context, slog.Record) error {
	h.mu.Lock()
	h.n++
	h.mu.Unlock()
	return nil
}
func (h *warnCounter) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *warnCounter) WithGroup(string) slog.Handler      { return h }

// A failed setMyCommands only costs the menu: it is logged and startup goes on.
func TestSetCommandsFailureIsAWarning(t *testing.T) {
	b, calls := fakeCommandsAPI(t, true)
	h := &warnCounter{}
	module.SetCommands(t.Context(), b, testBundle(t), slog.New(h),
		cmdModule{[]module.Command{{Name: "start", DescriptionID: "t.start", Scope: module.ScopePrivate}}})
	if len(calls()) != 3 || h.n != 3 {
		t.Fatalf("calls = %d warnings = %d", len(calls()), h.n)
	}
}
