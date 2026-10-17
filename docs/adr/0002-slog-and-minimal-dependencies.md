# 2. slog and a minimal dependency set

Logging uses stdlib `log/slog`, not zerolog: bot throughput makes zerolog's speed irrelevant and a host can adapt
any logger to slog. Config is hand-written (defaults -> YAML -> env table) instead of koanf, which alone pulled in
nine modules. SQLite is `ncruces/go-sqlite3` (no cgo, half the binary size of modernc). Allowed dependencies:
go-telegram/bot, go-i18n, x/text, yaml v3, ncruces/go-sqlite3.
