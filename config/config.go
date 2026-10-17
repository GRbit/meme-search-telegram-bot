// Package config loads the host configuration: defaults, then an optional YAML
// file, then environment variables, each layer overriding the previous one.
//
// Environment names map to YAML keys through an explicit table rather than by
// splitting on "_", because keys themselves contain underscores
// (MEME_SEARCH_API_TOKEN -> meme_search.api_token).
//
// Relative default paths (memes/, data/bot.db, config.yml) resolve against the
// executable's directory. Relative paths a user sets in the file or the
// environment are kept as given, so they resolve against the working directory.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/grbit/meme-search-telegram-bot/access"
	"github.com/grbit/meme-search-telegram-bot/configerr"
	"github.com/grbit/meme-search-telegram-bot/mediacache"
	"github.com/grbit/meme-search-telegram-bot/memesearch"
	"github.com/grbit/meme-search-telegram-bot/saver"
	"github.com/grbit/meme-search-telegram-bot/search"
)

// Config is the union of the host settings and every module's Config.
type Config struct {
	TelegramToken string
	LogLevel      slog.Level
	DBPath        string
	DefaultLang   string
	Location      *time.Location

	MemeSearch memesearch.Config
	Search     search.Config
	Media      mediacache.Config
	Access     access.Config
	Saver      saver.Config
	// SaverEnabled is true when at least one user may save.
	SaverEnabled bool

	// File is the config file that was read; empty when none existed.
	File string
}

const (
	envConfigFile     = "CONFIG_FILE"
	defaultConfigFile = "config.yml"
)

// setting is one row of the env <-> YAML table, the single source of truth
// for names, defaults, parsing and the summary.
type setting struct {
	env    string
	key    string // dotted YAML path
	def    string
	defRel bool // def is a path relative to the executable's directory
	secret bool
	// field is "<Section>.<GoField>" when a module's Config.Validate owns the
	// value rules, so its *configerr.Field errors can be reported by env name.
	field string
	set   func(c *Config, v string) error
	get   func(c *Config) string
}

var settings = []setting{
	{env: "TELEGRAM_BOT_TOKEN", key: "telegram.bot_token", secret: true,
		set: func(c *Config, v string) error { c.TelegramToken = v; return nil },
		get: func(c *Config) string { return c.TelegramToken }},
	{env: "MEME_SEARCH_URL", key: "meme_search.url", field: "MemeSearch.URL",
		set: func(c *Config, v string) error { c.MemeSearch.URL = v; return nil },
		get: func(c *Config) string { return c.MemeSearch.URL }},
	{env: "MEME_SEARCH_API_TOKEN", key: "meme_search.api_token", secret: true, field: "MemeSearch.Token",
		set: func(c *Config, v string) error { c.MemeSearch.Token = v; return nil },
		get: func(c *Config) string { return c.MemeSearch.Token }},
	{env: "MEME_SEARCH_TIMEOUT", key: "meme_search.timeout", def: memesearch.DefaultTimeout.String(), field: "MemeSearch.Timeout",
		set: func(c *Config, v string) error { return parseDuration(v, &c.MemeSearch.Timeout) },
		get: func(c *Config) string { return c.MemeSearch.Timeout.String() }},
	{env: "RESULTS_PER_PAGE", key: "search.results_per_page", field: "Search.ResultsPerPage", def: "5",
		set: func(c *Config, v string) error { return parseInt(v, &c.Search.ResultsPerPage) },
		get: func(c *Config) string { return strconv.Itoa(c.Search.ResultsPerPage) }},
	{env: "SEARCH_MODE", key: "search.mode", field: "Search.Mode", def: string(memesearch.ModeVector),
		set: func(c *Config, v string) error { c.Search.Mode = memesearch.Mode(v); return nil },
		get: func(c *Config) string { return string(c.Search.Mode) }},
	{env: "CACHE_CHAT_ID", key: "cache.chat_id", def: "0",
		set: func(c *Config, v string) error { return parseInt64(v, &c.Media.CacheChatID) },
		get: func(c *Config) string { return strconv.FormatInt(c.Media.CacheChatID, 10) }},
	// Empty means "same as SAVE_DIR", resolved after all layers.
	{env: "LIBRARY_DIR", key: "cache.library_dir",
		set: func(c *Config, v string) error { c.Media.LibraryDir = v; return nil },
		get: func(c *Config) string { return c.Media.LibraryDir }},
	{env: "WARMUP_INTERVAL", key: "cache.warmup_interval", def: "6h",
		set: func(c *Config, v string) error { return parseDuration(v, &c.Media.WarmupInterval) },
		get: func(c *Config) string { return c.Media.WarmupInterval.String() }},
	{env: "CACHE_UPLOADS_PER_MIN", key: "cache.uploads_per_min", def: "20",
		set: func(c *Config, v string) error { return parseInt(v, &c.Media.UploadsPerMin) },
		get: func(c *Config) string { return strconv.Itoa(c.Media.UploadsPerMin) }},
	{env: "WARMUP_UPLOADS_PER_MIN", key: "cache.warmup_uploads_per_min", def: "10",
		set: func(c *Config, v string) error { return parseInt(v, &c.Media.WarmupUploadsPerMin) },
		get: func(c *Config) string { return strconv.Itoa(c.Media.WarmupUploadsPerMin) }},
	{env: "SAVE_DIR", key: "saver.dir", field: "Saver.Dir", def: "memes", defRel: true,
		set: func(c *Config, v string) error { c.Saver.Dir = v; return nil },
		get: func(c *Config) string { return c.Saver.Dir }},
	{env: "SAVE_DIR_PREFIX", key: "saver.dir_prefix", def: "tg-",
		set: func(c *Config, v string) error { c.Saver.DirPrefix = v; return nil },
		get: func(c *Config) string { return c.Saver.DirPrefix }},
	{env: "SAVE_NOTE", key: "saver.note",
		set: func(c *Config, v string) error { c.Saver.Note = v; return nil },
		get: func(c *Config) string { return c.Saver.Note }},
	{env: "SAVE_USER_IDS", key: "access.save_user_ids", field: "Access.SaveUserIDs",
		set: func(c *Config, v string) error { return parseIDs(v, &c.Access.SaveUserIDs) },
		get: func(c *Config) string { return formatIDs(c.Access.SaveUserIDs) }},
	{env: "TRUSTED_USER_IDS", key: "access.trusted_user_ids", field: "Access.TrustedUserIDs",
		set: func(c *Config, v string) error { return parseIDs(v, &c.Access.TrustedUserIDs) },
		get: func(c *Config) string { return formatIDs(c.Access.TrustedUserIDs) }},
	{env: "PUBLIC_DAILY_PER_USER", key: "access.public_daily_per_user", field: "Access.PublicDailyPerUser", def: "5",
		set: func(c *Config, v string) error { return parseInt(v, &c.Access.PublicDailyPerUser) },
		get: func(c *Config) string { return strconv.Itoa(c.Access.PublicDailyPerUser) }},
	{env: "PUBLIC_DAILY_TOTAL", key: "access.public_daily_total", field: "Access.PublicDailyTotal", def: "120",
		set: func(c *Config, v string) error { return parseInt(v, &c.Access.PublicDailyTotal) },
		get: func(c *Config) string { return strconv.Itoa(c.Access.PublicDailyTotal) }},
	{env: "INLINE_CHARGE_WINDOW", key: "access.inline_charge_window", field: "Access.InlineChargeWindow", def: "10s",
		set: func(c *Config, v string) error { return parseDuration(v, &c.Access.InlineChargeWindow) },
		get: func(c *Config) string { return c.Access.InlineChargeWindow.String() }},
	{env: "ADMIN_CONTACT", key: "access.admin_contact",
		set: func(c *Config, v string) error { c.Access.AdminContact = v; return nil },
		get: func(c *Config) string { return c.Access.AdminContact }},
	{env: "DEFAULT_LANG", key: "default_lang", def: "en",
		set: func(c *Config, v string) error { c.DefaultLang = v; return nil },
		get: func(c *Config) string { return c.DefaultLang }},
	{env: "TZ", key: "tz", def: "UTC",
		set: func(c *Config, v string) error {
			loc, err := time.LoadLocation(v)
			if err != nil {
				return fmt.Errorf("unknown time zone %q", v)
			}
			c.Location = loc
			return nil
		},
		get: func(c *Config) string { return c.Location.String() }},
	{env: "DB_PATH", key: "db_path", def: filepath.Join("data", "bot.db"), defRel: true,
		set: func(c *Config, v string) error { c.DBPath = v; return nil },
		get: func(c *Config) string { return c.DBPath }},
	{env: "LOG_LEVEL", key: "log_level", def: "info",
		set: func(c *Config, v string) error {
			if err := c.LogLevel.UnmarshalText([]byte(v)); err != nil {
				return fmt.Errorf("invalid log level %q, want debug, info, warn or error", v)
			}
			return nil
		},
		get: func(c *Config) string { return c.LogLevel.String() }},
}

// Load builds the config. environ is os.Environ() in production; exeDir is the
// directory of the executable, the base for relative default paths.
//
// All parse and validation problems are returned together, each naming its
// environment variable, so one restart fixes a broken config.
func Load(environ []string, exeDir string) (Config, error) {
	envs := parseEnviron(environ)

	raw := make(map[string]string, len(settings))
	for _, s := range settings {
		if s.defRel {
			raw[s.key] = filepath.Join(exeDir, s.def)
		} else {
			raw[s.key] = s.def
		}
	}

	var c Config
	path, ok := envs[envConfigFile]
	if !ok {
		path = filepath.Join(exeDir, defaultConfigFile)
	}
	file, found, err := readFile(path)
	if err != nil {
		return Config{}, err
	}
	if found {
		c.File = path
		for k, v := range file {
			raw[k] = v
		}
	}

	for _, s := range settings {
		if v, ok := envs[s.env]; ok {
			raw[s.key] = v
		}
	}

	var errs []error
	for _, s := range settings {
		if err := s.set(&c, strings.TrimSpace(raw[s.key])); err != nil {
			errs = append(errs, fieldErr(s.env, "%v", err))
		}
	}
	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}

	if c.Media.LibraryDir == "" {
		c.Media.LibraryDir = c.Saver.Dir
	}
	c.Access.Location = c.Location
	c.Saver.Location = c.Location
	c.SaverEnabled = len(c.Access.SaveUserIDs) > 0

	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// parseEnviron keeps the first occurrence of each name, as os.Getenv does.
// Empty values count as unset: compose files pass `VAR=${VAR}` through as
// empty strings, which must not wipe a value from the config file.
func parseEnviron(environ []string) map[string]string {
	m := make(map[string]string, len(environ))
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || v == "" {
			continue
		}
		if _, dup := m[k]; !dup {
			m[k] = v
		}
	}
	return m
}

// readFile flattens the YAML file into dotted keys. Unknown keys are errors:
// a silently ignored typo would leave a default the user meant to change.
func readFile(path string) (map[string]string, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("config: read %s: %w", path, err)
	}
	// yaml.Node keeps scalars as written, so ids and durations reach the
	// parsers unchanged instead of round-tripping through int or float.
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, false, fmt.Errorf("config: parse %s: %w", path, err)
	}
	out := map[string]string{}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return out, true, nil // empty or comment-only file
	}
	known := make(map[string]bool, len(settings))
	for _, s := range settings {
		known[s.key] = true
	}
	if err := flatten(doc.Content[0], "", known, out); err != nil {
		return nil, false, fmt.Errorf("config: %s: %w", path, err)
	}
	return out, true, nil
}

func flatten(n *yaml.Node, prefix string, known map[string]bool, out map[string]string) error {
	if n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n.Kind != yaml.MappingNode {
		if prefix == "" {
			return errors.New("top level must be a mapping")
		}
		return fmt.Errorf("%s: expected a section of keys", prefix)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i].Value
		if prefix != "" {
			key = prefix + "." + key
		}
		v := n.Content[i+1]
		if v.Kind == yaml.AliasNode {
			v = v.Alias
		}
		if !known[key] {
			if v.Kind == yaml.MappingNode && isSection(key, known) {
				if err := flatten(v, key, known, out); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("unknown key %q", key)
		}
		s, err := scalar(v)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		if s != nil {
			out[key] = *s
		}
	}
	return nil
}

func isSection(key string, known map[string]bool) bool {
	for k := range known {
		if strings.HasPrefix(k, key+".") {
			return true
		}
	}
	return false
}

// scalar returns nil for an explicit null so `key:` with no value keeps the default.
// A sequence is joined with commas, the same form the environment uses for id lists.
func scalar(n *yaml.Node) (*string, error) {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Tag == "!!null" {
			return nil, nil
		}
		return &n.Value, nil
	case yaml.SequenceNode:
		parts := make([]string, 0, len(n.Content))
		for _, item := range n.Content {
			if item.Kind != yaml.ScalarNode {
				return nil, errors.New("list items must be plain values")
			}
			parts = append(parts, item.Value)
		}
		s := strings.Join(parts, ",")
		return &s, nil
	default:
		return nil, errors.New("expected a value, got a section")
	}
}

func (c *Config) validate() error {
	var errs []error
	if c.TelegramToken == "" {
		errs = append(errs, fieldErr("TELEGRAM_BOT_TOKEN", "required"))
	}
	if c.Media.WarmupInterval <= 0 {
		errs = append(errs, fieldErr("WARMUP_INTERVAL", "must be positive, got %s", c.Media.WarmupInterval))
	}
	if c.Media.UploadsPerMin <= 0 {
		errs = append(errs, fieldErr("CACHE_UPLOADS_PER_MIN", "must be positive, got %d", c.Media.UploadsPerMin))
	}
	if c.Media.WarmupUploadsPerMin <= 0 {
		errs = append(errs, fieldErr("WARMUP_UPLOADS_PER_MIN", "must be positive, got %d", c.Media.WarmupUploadsPerMin))
	} else if c.Media.WarmupUploadsPerMin > c.Media.UploadsPerMin && c.Media.UploadsPerMin > 0 {
		errs = append(errs, fieldErr("WARMUP_UPLOADS_PER_MIN", "must not exceed CACHE_UPLOADS_PER_MIN (%d), got %d",
			c.Media.UploadsPerMin, c.Media.WarmupUploadsPerMin))
	}
	if c.DefaultLang == "" {
		errs = append(errs, fieldErr("DEFAULT_LANG", "must not be empty"))
	}
	errs = append(errs, moduleErrs("MemeSearch", c.MemeSearch.Validate())...)
	errs = append(errs, moduleErrs("Search", c.Search.Validate())...)
	errs = append(errs, moduleErrs("Access", c.Access.Validate())...)
	errs = append(errs, moduleErrs("Saver", c.Saver.Validate())...)
	return errors.Join(errs...)
}

// moduleErrs maps a module's Config.Validate result to env-named errors.
func moduleErrs(section string, err error) []error {
	fields, rest := configerr.Fields(err)
	out := make([]error, 0, len(fields)+len(rest))
	for _, f := range fields {
		env := section + "." + f.Name
		for _, s := range settings {
			if s.field == env {
				env = s.env
				break
			}
		}
		out = append(out, fieldErr(env, "%s", f.Reason))
	}
	for _, e := range rest {
		out = append(out, fmt.Errorf("config: %s: %w", section, e))
	}
	return out
}

// fieldErr names both the env var and the YAML key: the user may have set
// either one.
func fieldErr(env, format string, args ...any) error {
	key := ""
	for _, s := range settings {
		if s.env == env {
			key = s.key
		}
	}
	return fmt.Errorf("config: %s (%s): %s", env, key, fmt.Sprintf(format, args...))
}

// Summary renders the effective config for the startup log with secrets masked.
//
// One KEY=value line per setting, in table order, named by env var because
// that is what self-hosters set.
func (c Config) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s=%s\n", envConfigFile, c.File)
	for _, s := range settings {
		v := s.get(&c)
		if s.secret {
			v = mask(v)
		}
		fmt.Fprintf(&b, "%s=%s\n", s.env, v)
	}
	fmt.Fprintf(&b, "inline_enabled=%t\nsaver_enabled=%t", c.Media.CacheChatID != 0, c.SaverEnabled)
	return b.String()
}

// mask keeps a short prefix so an operator can tell which token is loaded;
// short values are hidden entirely since a prefix would reveal most of them.
func mask(s string) string {
	switch {
	case s == "":
		return ""
	case len(s) <= 8:
		return "***"
	default:
		return s[:4] + "..."
	}
}

func parseInt(v string, dst *int) error {
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("invalid integer %q", v)
	}
	*dst = n
	return nil
}

func parseInt64(v string, dst *int64) error {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid integer %q", v)
	}
	*dst = n
	return nil
}

func parseDuration(v string, dst *time.Duration) error {
	d, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("invalid duration %q, use Go syntax such as 10s, 5m or 6h", v)
	}
	*dst = d
	return nil
}

// parseIDs tolerates spaces and empty items ("1, 2,") since lists are typed by hand.
func parseIDs(v string, dst *[]int64) error {
	var ids []int64
	for part := range strings.SplitSeq(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid user id %q in comma-separated list", part)
		}
		ids = append(ids, id)
	}
	*dst = ids
	return nil
}

func formatIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}
