package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
	"github.com/grbit/meme-search-telegram-bot/saver"
)

// required is the minimal environ that passes validation.
func required() []string {
	return []string{
		"TELEGRAM_BOT_TOKEN=123456:secretbottoken",
		"MEME_SEARCH_URL=https://memes.example.com",
		"MEME_SEARCH_API_TOKEN=apitokenvalue",
	}
}

// env puts extra first so it overrides required(): the first occurrence of a name wins.
func env(extra ...string) []string { return append(append([]string{}, extra...), required()...) }

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustLoad(t *testing.T, environ []string, exeDir string) Config {
	t.Helper()
	c, err := Load(environ, exeDir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

func TestDefaults(t *testing.T) {
	exe := t.TempDir()
	c := mustLoad(t, required(), exe)

	want := Config{
		TelegramToken: "123456:secretbottoken",
		LogLevel:      slog.LevelInfo,
		DBPath:        filepath.Join(exe, "data", "bot.db"),
		DefaultLang:   "en",
		Location:      time.UTC,
		File:          "",
	}
	want.MemeSearch.URL = "https://memes.example.com"
	want.MemeSearch.Token = "apitokenvalue"
	want.MemeSearch.Timeout = memesearch.DefaultTimeout
	want.Search.ResultsPerPage = 5
	want.Search.Mode = memesearch.ModeVector
	want.Search.InlineResults = 3
	want.Search.InlineDebounce = time.Second
	want.Search.ResultsMemoryLimit = 20000
	want.Media.CacheChatID = 0
	want.Media.LibraryDir = filepath.Join(exe, "memes")
	want.Media.WarmupInterval = 6 * time.Hour
	want.Media.UploadsPerMin = 20
	want.Media.WarmupUploadsPerMin = 10
	want.Media.FeedInterval = time.Minute
	want.Access.PublicDailyPerUser = 5
	want.Access.PublicDailyTotal = 120
	want.Access.InlineChargeWindow = 10 * time.Second
	want.Access.Location = time.UTC
	want.Saver.Dir = filepath.Join(exe, "memes")
	want.Saver.DirPrefix = "tg-"
	want.Saver.Mode = saver.ModePerUser
	want.Saver.Backend = saver.BackendFolder
	want.Saver.Folder = "telegram"
	want.Saver.Location = time.UTC

	if !reflect.DeepEqual(c, want) {
		t.Errorf("defaults:\n got %+v\nwant %+v", c, want)
	}
}

func TestEveryEnvVarParsed(t *testing.T) {
	exe := t.TempDir()
	c := mustLoad(t, []string{
		"TELEGRAM_BOT_TOKEN=tg",
		"MEME_SEARCH_URL=http://meme_search:3000",
		"MEME_SEARCH_API_TOKEN=api",
		"MEME_SEARCH_TIMEOUT=30s",
		"CACHE_CHAT_ID=-1001234567890",
		"LIBRARY_DIR=/lib",
		"WARMUP_INTERVAL=1h30m",
		"FEED_INTERVAL=30s",
		"CACHE_UPLOADS_PER_MIN=30",
		"WARMUP_UPLOADS_PER_MIN=30",
		"RESULTS_PER_PAGE=10",
		"SEARCH_MODE=keyword",
		"INLINE_RESULTS=7",
		"INLINE_DEBOUNCE=250ms",
		"INLINE_GROUP_SCOPE=#reaction dir:x",
		"SAVE_DIR=/save",
		"SAVE_DIR_PREFIX=bot-",
		"SAVE_MODE=single",
		"SAVE_FOLDER=all_memes",
		"SAVE_NOTE=see you",
		"SAVE_BACKEND=api",
		"SAVE_USER_IDS=1, 2,3",
		"TRUSTED_USER_IDS= 4 ,5 ",
		"PUBLIC_DAILY_PER_USER=0",
		"PUBLIC_DAILY_TOTAL=7",
		"INLINE_CHARGE_WINDOW=500ms",
		"ADMIN_CONTACT=@admin",
		"DEFAULT_LANG=ru",
		"TZ=Europe/Amsterdam",
		"DB_PATH=/db/x.db",
		"LOG_LEVEL=DEBUG",
	}, exe)

	ams, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name      string
		got, want any
	}{
		{"TelegramToken", c.TelegramToken, "tg"},
		{"MemeSearch.URL", c.MemeSearch.URL, "http://meme_search:3000"},
		{"MemeSearch.Token", c.MemeSearch.Token, "api"},
		{"MemeSearch.Timeout", c.MemeSearch.Timeout, 30 * time.Second},
		{"Media.CacheChatID", c.Media.CacheChatID, int64(-1001234567890)},
		{"Media.LibraryDir", c.Media.LibraryDir, "/lib"},
		{"Media.WarmupInterval", c.Media.WarmupInterval, 90 * time.Minute},
		{"Media.FeedInterval", c.Media.FeedInterval, 30 * time.Second},
		{"Media.UploadsPerMin", c.Media.UploadsPerMin, 30},
		{"Media.WarmupUploadsPerMin", c.Media.WarmupUploadsPerMin, 30},
		{"Search.ResultsPerPage", c.Search.ResultsPerPage, 10},
		{"Search.Mode", c.Search.Mode, memesearch.ModeKeyword},
		{"Search.InlineResults", c.Search.InlineResults, 7},
		{"Search.InlineDebounce", c.Search.InlineDebounce, 250 * time.Millisecond},
		{"Search.InlineGroupScope", c.Search.InlineGroupScope, "#reaction dir:x"},
		{"Saver.Dir", c.Saver.Dir, "/save"},
		{"Saver.DirPrefix", c.Saver.DirPrefix, "bot-"},
		{"Saver.Mode", c.Saver.Mode, saver.ModeSingle},
		{"Saver.Folder", c.Saver.Folder, "all_memes"},
		{"Saver.Note", c.Saver.Note, "see you"},
		{"Saver.Backend", c.Saver.Backend, saver.BackendAPI},
		{"Access.SaveUserIDs", c.Access.SaveUserIDs, []int64{1, 2, 3}},
		{"Access.TrustedUserIDs", c.Access.TrustedUserIDs, []int64{4, 5}},
		{"Access.PublicDailyPerUser", c.Access.PublicDailyPerUser, 0},
		{"Access.PublicDailyTotal", c.Access.PublicDailyTotal, 7},
		{"Access.InlineChargeWindow", c.Access.InlineChargeWindow, 500 * time.Millisecond},
		{"Access.AdminContact", c.Access.AdminContact, "@admin"},
		{"DefaultLang", c.DefaultLang, "ru"},
		{"Location", c.Location.String(), ams.String()},
		{"Access.Location", c.Access.Location.String(), ams.String()},
		{"Saver.Location", c.Saver.Location.String(), ams.String()},
		{"DBPath", c.DBPath, "/db/x.db"},
		{"LogLevel", c.LogLevel, slog.LevelDebug},
		{"SaverEnabled", c.SaverEnabled, true},
	}
	for _, ch := range checks {
		if !reflect.DeepEqual(ch.got, ch.want) {
			t.Errorf("%s = %#v, want %#v", ch.name, ch.got, ch.want)
		}
	}
}

func TestPrecedence(t *testing.T) {
	exe := t.TempDir()
	file := writeFile(t, exe, "config.yml", `
search:
  results_per_page: 7
  mode: keyword
access:
  admin_contact: "@file"
  save_user_ids: [10, 20]
`)
	tests := []struct {
		name     string
		environ  []string
		wantRPP  int
		wantMode memesearch.Mode
		wantAdm  string
		wantSave []int64
	}{
		{"file over default", env(), 7, memesearch.ModeKeyword, "@file", []int64{10, 20}},
		{"env over file", env("RESULTS_PER_PAGE=3", "ADMIN_CONTACT=@env", "SAVE_USER_IDS=30"), 3, memesearch.ModeKeyword, "@env", []int64{30}},
		// Compose passes unset variables through as empty strings; they must not wipe file values.
		{"empty env ignored", env("RESULTS_PER_PAGE=", "ADMIN_CONTACT="), 7, memesearch.ModeKeyword, "@file", []int64{10, 20}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := mustLoad(t, tt.environ, exe)
			if c.File != file {
				t.Errorf("File = %q, want %q", c.File, file)
			}
			if c.Search.ResultsPerPage != tt.wantRPP || c.Search.Mode != tt.wantMode ||
				c.Access.AdminContact != tt.wantAdm || !reflect.DeepEqual(c.Access.SaveUserIDs, tt.wantSave) {
				t.Errorf("got rpp=%d mode=%s admin=%q save=%v", c.Search.ResultsPerPage, c.Search.Mode,
					c.Access.AdminContact, c.Access.SaveUserIDs)
			}
		})
	}
}

func TestFileProvidesEverything(t *testing.T) {
	exe := t.TempDir()
	writeFile(t, exe, "config.yml", `
telegram:
  bot_token: filetoken
meme_search:
  url: https://f.example
  api_token: fileapi
  timeout: 3s
cache:
  chat_id: -100
  library_dir: /flib
  warmup_interval: 2h
  uploads_per_min: 5
  warmup_uploads_per_min: 4
saver:
  dir: /fsave
  dir_prefix: x-
  note: hi
access:
  trusted_user_ids: "7,8"
  public_daily_per_user: 1
  public_daily_total: 2
  inline_charge_window: 1s
default_lang: ru
tz: Asia/Tokyo
db_path: /fdb
log_level: warn
`)
	c := mustLoad(t, nil, exe)
	if c.TelegramToken != "filetoken" || c.MemeSearch.Token != "fileapi" || c.MemeSearch.Timeout != 3*time.Second ||
		c.Media.CacheChatID != -100 || c.Media.LibraryDir != "/flib" || c.Media.WarmupUploadsPerMin != 4 ||
		c.Saver.Note != "hi" || !reflect.DeepEqual(c.Access.TrustedUserIDs, []int64{7, 8}) ||
		c.Location.String() != "Asia/Tokyo" || c.LogLevel != slog.LevelWarn || c.DBPath != "/fdb" {
		t.Errorf("file values not applied: %+v", c)
	}
}

func TestConfigFileLocation(t *testing.T) {
	exe := t.TempDir()
	other := t.TempDir()
	alt := writeFile(t, other, "alt.yml", "admin_contact_typo: 1\n")
	if _, err := Load(env("CONFIG_FILE="+alt), exe); err == nil || !strings.Contains(err.Error(), "admin_contact_typo") {
		t.Errorf("CONFIG_FILE not honoured or unknown key accepted: %v", err)
	}
	// A missing explicit file is as fine as a missing default one.
	c := mustLoad(t, env("CONFIG_FILE="+filepath.Join(other, "none.yml")), exe)
	if c.File != "" {
		t.Errorf("File = %q for a missing file", c.File)
	}
}

func TestFileErrors(t *testing.T) {
	tests := []struct {
		name, body, want string
	}{
		{"malformed", "search: [unclosed", "config.yml"},
		{"unknown key", "search:\n  bogus: 1\n", "search.bogus"},
		{"section as value", "search: 5\n", "search"},
		{"value as section", "db_path:\n  x: 1\n", "db_path"},
		{"bad value names env var", "search:\n  results_per_page: lots\n", "RESULTS_PER_PAGE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exe := t.TempDir()
			writeFile(t, exe, "config.yml", tt.body)
			_, err := Load(env(), exe)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want mention of %q", err, tt.want)
			}
		})
	}
}

func TestEmptyFileIsFine(t *testing.T) {
	exe := t.TempDir()
	writeFile(t, exe, "config.yml", "# nothing yet\n")
	mustLoad(t, env(), exe)
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name    string
		environ []string
		want    string
	}{
		{"no bot token", []string{"MEME_SEARCH_URL=https://x", "MEME_SEARCH_API_TOKEN=a"}, "TELEGRAM_BOT_TOKEN"},
		{"no url", []string{"TELEGRAM_BOT_TOKEN=t", "MEME_SEARCH_API_TOKEN=a"}, "MEME_SEARCH_URL"},
		{"no api token", []string{"TELEGRAM_BOT_TOKEN=t", "MEME_SEARCH_URL=https://x"}, "MEME_SEARCH_API_TOKEN"},
		{"url scheme", env("MEME_SEARCH_URL=ftp://x"), "MEME_SEARCH_URL"},
		{"url no host", env("MEME_SEARCH_URL=https://"), "MEME_SEARCH_URL"},
		{"url garbage", env("MEME_SEARCH_URL=memes.example.com"), "MEME_SEARCH_URL"},
		{"url query", env("MEME_SEARCH_URL=https://x/?a=1"), "MEME_SEARCH_URL"},
		{"url userinfo", env("MEME_SEARCH_URL=https://u:p@x"), "MEME_SEARCH_URL"},
		{"timeout negative", env("MEME_SEARCH_TIMEOUT=-1s"), "MEME_SEARCH_TIMEOUT"},
		{"rpp eleven", env("RESULTS_PER_PAGE=11"), "RESULTS_PER_PAGE"},
		{"rpp not int", env("RESULTS_PER_PAGE=five"), "RESULTS_PER_PAGE"},
		{"inline results too many", env("INLINE_RESULTS=21"), "INLINE_RESULTS"},
		{"inline results negative", env("INLINE_RESULTS=-1"), "INLINE_RESULTS"},
		{"inline debounce negative", env("INLINE_DEBOUNCE=-1s"), "INLINE_DEBOUNCE"},
		{"inline group scope with words", env("INLINE_GROUP_SCOPE=cats #x"), "INLINE_GROUP_SCOPE"},
		{"inline debounce bad", env("INLINE_DEBOUNCE=1"), "INLINE_DEBOUNCE"},
		{"inline debounce too long", env("INLINE_DEBOUNCE=6s"), "INLINE_DEBOUNCE"},
		{"mode", env("SEARCH_MODE=hybrid"), "SEARCH_MODE"},
		{"per user negative", env("PUBLIC_DAILY_PER_USER=-1"), "PUBLIC_DAILY_PER_USER"},
		{"total negative", env("PUBLIC_DAILY_TOTAL=-1"), "PUBLIC_DAILY_TOTAL"},
		{"window negative", env("INLINE_CHARGE_WINDOW=-1s"), "INLINE_CHARGE_WINDOW"},
		{"window bad", env("INLINE_CHARGE_WINDOW=10"), "INLINE_CHARGE_WINDOW"},
		{"warmup interval negative", env("WARMUP_INTERVAL=-1s"), "WARMUP_INTERVAL"},
		{"feed interval negative", env("FEED_INTERVAL=-1s"), "FEED_INTERVAL"},
		{"save backend unknown", env("SAVE_BACKEND=s3"), "SAVE_BACKEND"},
		{"uploads negative", env("CACHE_UPLOADS_PER_MIN=-1"), "CACHE_UPLOADS_PER_MIN"},
		{"warmup uploads negative", env("WARMUP_UPLOADS_PER_MIN=-1"), "WARMUP_UPLOADS_PER_MIN"},
		{"warmup above total", env("CACHE_UPLOADS_PER_MIN=5", "WARMUP_UPLOADS_PER_MIN=6"), "WARMUP_UPLOADS_PER_MIN"},
		{"chat id bad", env("CACHE_CHAT_ID=@channel"), "CACHE_CHAT_ID"},
		{"ids bad", env("SAVE_USER_IDS=1,x"), "SAVE_USER_IDS"},
		{"save mode unknown", env("SAVE_MODE=shared"), "SAVE_MODE"},
		{"save folder nested", env("SAVE_MODE=single", "SAVE_FOLDER=a/b"), "SAVE_FOLDER"},
		{"save folder nested in nested mode", env("SAVE_MODE=nested", "SAVE_FOLDER=a/b"), "SAVE_FOLDER"},
		{"ids not positive", env("SAVE_USER_IDS=1,0"), "SAVE_USER_IDS"},
		{"trusted negative", env("TRUSTED_USER_IDS=-5"), "TRUSTED_USER_IDS"},
		{"trusted bad", env("TRUSTED_USER_IDS=1;2"), "TRUSTED_USER_IDS"},
		{"tz", env("TZ=Mars/Olympus"), "TZ"},
		{"log level", env("LOG_LEVEL=loud"), "LOG_LEVEL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(tt.environ, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want mention of %s", err, tt.want)
			}
		})
	}
}

func TestSaveModeNested(t *testing.T) {
	c := mustLoad(t, env("SAVE_MODE=nested", "SAVE_FOLDER=tgmemes"), t.TempDir())
	if c.Saver.Mode != saver.ModeNested || c.Saver.Folder != "tgmemes" {
		t.Errorf("mode = %q, folder = %q", c.Saver.Mode, c.Saver.Folder)
	}
}

func TestDefaultLangEmptyInFile(t *testing.T) {
	exe := t.TempDir()
	writeFile(t, exe, "config.yml", "default_lang: \"\"\n")
	if _, err := Load(env(), exe); err == nil || !strings.Contains(err.Error(), "DEFAULT_LANG") {
		t.Errorf("err = %v, want DEFAULT_LANG", err)
	}
}

func TestValidationReportsAllErrors(t *testing.T) {
	_, err := Load([]string{"RESULTS_PER_PAGE=11"}, t.TempDir())
	if err == nil {
		t.Fatal("want error")
	}
	for _, v := range []string{"TELEGRAM_BOT_TOKEN", "MEME_SEARCH_URL", "MEME_SEARCH_API_TOKEN", "RESULTS_PER_PAGE"} {
		if !strings.Contains(err.Error(), v) {
			t.Errorf("error lacks %s: %v", v, err)
		}
	}
}

func TestPaths(t *testing.T) {
	exe := t.TempDir()
	c := mustLoad(t, env("SAVE_DIR=rel/memes", "DB_PATH=rel.db"), exe)
	// User-given relative paths stay relative to the working directory.
	if c.Saver.Dir != "rel/memes" || c.DBPath != "rel.db" {
		t.Errorf("relative user paths rewritten: save=%q db=%q", c.Saver.Dir, c.DBPath)
	}
	if c.Media.LibraryDir != "rel/memes" {
		t.Errorf("LibraryDir = %q, want SAVE_DIR", c.Media.LibraryDir)
	}
}

func TestEnvFirstOccurrenceWins(t *testing.T) {
	c := mustLoad(t, env("SAVE_NOTE=first", "SAVE_NOTE=second", "NOT_AN_ASSIGNMENT"), t.TempDir())
	if c.Saver.Note != "first" {
		t.Errorf("Note = %q, want the first occurrence like os.Getenv", c.Saver.Note)
	}
}

func TestSummaryMasksSecrets(t *testing.T) {
	c := mustLoad(t, env("SAVE_USER_IDS=1,2", "CACHE_CHAT_ID=-100"), t.TempDir())
	s := c.Summary()
	for _, secret := range []string{"secretbottoken", "apitokenvalue"} {
		if strings.Contains(s, secret) {
			t.Errorf("summary leaks %q:\n%s", secret, s)
		}
	}
	for _, want := range []string{
		"TELEGRAM_BOT_TOKEN=1234...",
		"MEME_SEARCH_API_TOKEN=apit...",
		"MEME_SEARCH_URL=https://memes.example.com",
		"SAVE_USER_IDS=1,2",
		"CACHE_CHAT_ID=-100",
		"RESULTS_PER_PAGE=5",
		"TZ=UTC",
		"LOG_LEVEL=INFO",
		"CONFIG_FILE=",
	} {
		if !strings.Contains(s, want+"\n") && !strings.HasSuffix(s, want) {
			t.Errorf("summary lacks %q:\n%s", want, s)
		}
	}
}

func TestMask(t *testing.T) {
	tests := map[string]string{
		"":                "",
		"short":           "***",
		"exactly8":        "***",
		"123456789:ABCDE": "1234...",
	}
	for in, want := range tests {
		if got := mask(in); got != want {
			t.Errorf("mask(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestExamplesCoverTable keeps the shipped examples in sync with the table:
// every setting is documented in .env.example and config.example.yml, and the
// example file loads as is, yielding the defaults.
func TestExamplesCoverTable(t *testing.T) {
	envExample, err := os.ReadFile("../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range append([]setting{{env: envConfigFile}}, settings...) {
		if !regexp.MustCompile(`(?m)^#?\s*` + s.env + `=`).Match(envExample) {
			t.Errorf(".env.example does not document %s", s.env)
		}
	}

	ymlExample, err := os.ReadFile("../config.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range settings {
		leaf := s.key[strings.LastIndex(s.key, ".")+1:]
		if !regexp.MustCompile(`(?m)^\s*#?\s*` + leaf + `:`).Match(ymlExample) {
			t.Errorf("config.example.yml does not document %s", s.key)
		}
	}

	exe := t.TempDir()
	env := []string{
		envConfigFile + "=../config.example.yml",
		"TELEGRAM_BOT_TOKEN=t", "MEME_SEARCH_URL=http://x", "MEME_SEARCH_API_TOKEN=t",
	}
	got, err := Load(env, exe)
	if err != nil {
		t.Fatalf("example file does not load: %v", err)
	}
	want, err := Load(env[1:], exe)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range settings {
		if s.secret || s.env == "MEME_SEARCH_URL" {
			continue
		}
		if g, w := s.get(&got), s.get(&want); g != w {
			t.Errorf("example file changes %s: %q, default %q", s.env, g, w)
		}
	}
}

func TestTableConsistency(t *testing.T) {
	envs, keys := map[string]bool{}, map[string]bool{}
	for _, s := range settings {
		if envs[s.env] || keys[s.key] {
			t.Errorf("duplicate env %s or key %s", s.env, s.key)
		}
		envs[s.env], keys[s.key] = true, true
		if s.set == nil || s.get == nil {
			t.Errorf("%s lacks set or get", s.env)
		}
	}
	// A key may not be both a leaf and a section, or the file layout is ambiguous.
	for k := range keys {
		for other := range keys {
			if strings.HasPrefix(other, k+".") {
				t.Errorf("key %s is a prefix of %s", k, other)
			}
		}
	}
}

// Uploads use MEME_SEARCH_API_TOKEN with media:write; there is no second token to configure.
func TestNoSeparateUploadToken(t *testing.T) {
	if s := mustLoad(t, required(), t.TempDir()).Summary(); strings.Contains(s, "UPLOAD_TOKEN") {
		t.Fatalf("summary lists an upload token:\n%s", s)
	}
}

func TestResultsMemoryLimit(t *testing.T) {
	if c := mustLoad(t, env("RESULTS_MEMORY_LIMIT=500"), t.TempDir()); c.Search.ResultsMemoryLimit != 500 {
		t.Fatalf("env: memes = %d", c.Search.ResultsMemoryLimit)
	}
	if s := mustLoad(t, required(), t.TempDir()).Summary(); strings.Contains(s, "PAGE_SESSIONS") {
		t.Fatalf("a session cap is still configurable:\n%s", s)
	}
	if _, err := Load(env("RESULTS_MEMORY_LIMIT=10"), t.TempDir()); err == nil || !strings.Contains(err.Error(), "RESULTS_MEMORY_LIMIT") {
		t.Fatalf("budget below one fetch: err = %v", err)
	}
}
