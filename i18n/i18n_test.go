package i18n_test

import (
	"bytes"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"go.yaml.in/yaml/v3"

	"github.com/grbit/meme-search-telegram-bot/access"
	"github.com/grbit/meme-search-telegram-bot/i18n"
	"github.com/grbit/meme-search-telegram-bot/saver"
	"github.com/grbit/meme-search-telegram-bot/search"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"en.yaml": {Data: []byte(`
t.hello: "Hello, {{.Name}}"
t.only_en: "English only"
t.items:
  one: "{{.Count}} item in {{.Where}}"
  other: "{{.Count}} items in {{.Where}}"
`)},
		"ru.yaml": {Data: []byte(`
t.hello: "Privet, {{.Name}}"
t.items:
  one: "{{.Count}} shtuka v {{.Where}}"
  few: "{{.Count}} shtuki v {{.Where}}"
  many: "{{.Count}} shtuk v {{.Where}}"
  other: "{{.Count}} shtuki v {{.Where}}"
`)},
	}
}

func newBundle(t *testing.T, log *slog.Logger, locales ...fs.FS) *i18n.Bundle {
	t.Helper()
	b, err := i18n.NewBundle("en", log, locales...)
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}
	return b
}

func TestLocalizerLanguageSelection(t *testing.T) {
	b := newBundle(t, discard(), testFS())
	tests := []struct {
		code string
		want string
	}{
		{"en", "Hello, Ann"},
		{"ru", "Privet, Ann"},
		{"RU", "Privet, Ann"},
		{"ru-RU", "Privet, Ann"},
		{"en-us", "Hello, Ann"},
		{"pt-br", "Hello, Ann"},
		{"de", "Hello, Ann"},
		{"", "Hello, Ann"},
		{"not a tag!", "Hello, Ann"},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			got := b.Localizer(tt.code).T("t.hello", map[string]any{"Name": "Ann"})
			if got != tt.want {
				t.Errorf("T = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMissingTranslationFallsBackToDefault(t *testing.T) {
	b := newBundle(t, discard(), testFS())
	if got := b.Localizer("ru").T("t.only_en", nil); got != "English only" {
		t.Errorf("T = %q, want default language text", got)
	}
}

func TestMissingIDRendersIDAndLogsDebug(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	b := newBundle(t, log, testFS())
	if got := b.Localizer("ru").T("t.nope", nil); got != "t.nope" {
		t.Errorf("T = %q, want the id", got)
	}
	if got := b.Localizer("en").N("t.nope_n", 3, nil); got != "t.nope_n" {
		t.Errorf("N = %q, want the id", got)
	}
	out := buf.String()
	if !strings.Contains(out, "level=DEBUG") || !strings.Contains(out, "t.nope") || !strings.Contains(out, "t.nope_n") {
		t.Errorf("expected debug log naming the ids, got %q", out)
	}
}

func TestPlural(t *testing.T) {
	b := newBundle(t, discard(), testFS())
	tests := []struct {
		lang  string
		count int
		want  string
	}{
		{"en", 1, "1 item in box"},
		{"en", 0, "0 items in box"},
		{"en", 5, "5 items in box"},
		{"ru", 1, "1 shtuka v box"},
		{"ru", 21, "21 shtuka v box"},
		{"ru", 2, "2 shtuki v box"},
		{"ru", 24, "24 shtuki v box"},
		{"ru", 5, "5 shtuk v box"},
		{"ru", 11, "11 shtuk v box"},
	}
	for _, tt := range tests {
		got := b.Localizer(tt.lang).N("t.items", tt.count, map[string]any{"Where": "box"})
		if got != tt.want {
			t.Errorf("%s N(%d) = %q, want %q", tt.lang, tt.count, got, tt.want)
		}
	}
}

func TestNDoesNotMutateData(t *testing.T) {
	b := newBundle(t, discard(), testFS())
	data := map[string]any{"Where": "box"}
	b.Localizer("en").N("t.items", 2, data)
	if _, ok := data["Count"]; ok {
		t.Error("N added Count to the caller's map")
	}
	if got := b.Localizer("en").N("t.items", 2, nil); got != "2 items in <no value>" {
		t.Errorf("N with nil data = %q", got)
	}
}

func TestNewBundleErrors(t *testing.T) {
	if _, err := i18n.NewBundle("", discard(), testFS()); err == nil {
		t.Error("empty default language: want error")
	}
	if _, err := i18n.NewBundle("not a tag!", discard(), testFS()); err == nil {
		t.Error("invalid default language: want error")
	}
	bad := fstest.MapFS{"en.yaml": {Data: []byte("a: [unclosed")}}
	if _, err := i18n.NewBundle("en", discard(), bad); err == nil {
		t.Error("malformed yaml: want error")
	}
	dup := fstest.MapFS{"en.yaml": {Data: []byte(`t.hello: "again"`)}}
	if _, err := i18n.NewBundle("en", discard(), testFS(), dup); err == nil || !strings.Contains(err.Error(), "t.hello") {
		t.Errorf("duplicate id across modules: want error naming it, got %v", err)
	}
}

func TestSeveralLocaleFSesMerge(t *testing.T) {
	other := fstest.MapFS{"en.yaml": {Data: []byte(`o.bye: "Bye"`)}, "README.md": {Data: []byte("not a locale")}}
	b := newBundle(t, discard(), testFS(), other)
	l := b.Localizer("en")
	if got := l.T("o.bye", nil); got != "Bye" {
		t.Errorf("o.bye = %q", got)
	}
	if got := l.T("t.only_en", nil); got != "English only" {
		t.Errorf("t.only_en = %q", got)
	}
}

// TestModuleCatalogParity keeps en and ru catalogs in sync for every shipped
// module, so a new message cannot silently fall back to English for ru users.
func TestModuleCatalogParity(t *testing.T) {
	modules := map[string]fs.FS{
		"search": search.LocaleFS(),
		"saver":  saver.LocaleFS(),
		"access": access.LocaleFS(),
	}
	for name, fsys := range modules {
		t.Run(name, func(t *testing.T) {
			en := catalogIDs(t, fsys, "en.yaml")
			ru := catalogIDs(t, fsys, "ru.yaml")
			if len(en) == 0 {
				t.Fatal("en.yaml has no messages")
			}
			for _, id := range en {
				if !slices.Contains(ru, id) {
					t.Errorf("%s missing in ru.yaml", id)
				}
				if !strings.HasPrefix(id, name+".") {
					t.Errorf("%s lacks the %q module prefix", id, name+".")
				}
			}
			for _, id := range ru {
				if !slices.Contains(en, id) {
					t.Errorf("%s missing in en.yaml", id)
				}
			}
		})
	}
	// The real catalogs must also load together without id collisions or parse errors.
	newBundle(t, discard(), search.LocaleFS(), saver.LocaleFS(), access.LocaleFS())
}

func catalogIDs(t *testing.T, fsys fs.FS, name string) []string {
	t.Helper()
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func TestLanguages(t *testing.T) {
	b, err := i18n.NewBundle("en", nil, fstest.MapFS{
		"en.yaml": {Data: []byte("a: \"A\"\n")},
		"ru.yaml": {Data: []byte("a: \"А\"\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Languages(); !slices.Equal(got, []string{"en", "ru"}) {
		t.Fatalf("languages = %v", got)
	}
}
