# Embedding the modules in another bot

Every feature of this bot is a public package that attaches to a `*bot.Bot` you own
([go-telegram/bot](https://github.com/go-telegram/bot)). Your host keeps its own handlers; the modules match only
their own updates: `search` takes every private non-command text message, `/start`, `/help`, `srch:` callbacks
and (with a cache channel) inline queries; `saver` takes photos and jpg/png/webp documents from allowed users.
go-telegram/bot runs the first handler that matches, in registration order, so register your own handlers for
private text (or anything else that overlaps) before the modules. Never use `bot.WithDefaultHandler` for
something the modules should see.

```go
db, _ := store.Open(ctx, "data/bot.db", log)
bundle, _ := i18n.NewBundle("en", log, search.LocaleFS(), saver.LocaleFS(), access.LocaleFS())
api, _ := memesearch.New(memesearch.Config{URL: url, Token: token}, nil, log)

b, _ := bot.New(botToken)
b.RegisterHandlerMatchFunc(isVoice, transcribe) // your own handlers stay as they are

acc, _ := access.New(ctx, access.Config{TrustedUserIDs: []int64{me}, PublicDailyPerUser: 5, PublicDailyTotal: 100},
	access.Deps{DB: db, I18n: bundle, Log: log})
media, _ := mediacache.New(ctx, mediacache.Config{CacheChatID: cacheChat, LibraryDir: "/memes"},
	mediacache.Deps{DB: db, API: api, Sender: b, Lister: api, Caps: api, Log: log}) // Lister, Caps: optional feed
sv, _ := saver.New(saver.Config{Dir: "/memes"}, saver.Deps{Permit: acc, Known: media, I18n: bundle,
	DB: db, Caps: api, Uploader: api, Log: log}) // all three optional; Uploader for Backend: saver.BackendAPI
media.OnFeed(sv.MemesChanged)
srch, _ := search.New(search.Config{}, search.Deps{API: api, Media: media, I18n: bundle, DB: db, Log: log,
	Middlewares: []bot.Middleware{acc.Middleware(search.Chargeable)},
	OwnFolder:   sv.FolderFor}) // optional: the "Only mine" folder shortcut

for _, m := range []module.Module{srch, sv} {
	m.Register(b)
}
go api.WatchCapabilities(ctx, 6*time.Hour)
go media.Run(ctx)
b.Start(ctx)
```

Rules the modules follow, so a host can rely on them:

- **Explicit dependencies.** No package-level state, no environment reads, no global logger. Each module has a
  `Config` (zero values mean documented defaults; `Config.Validate` returns `*configerr.Field` errors naming the
  Go field, so a host can map them to its own setting names) and a `Deps` struct.
- **Pick what you need.** A host that only wants saving builds `saver` with any `saver.Permit` and no media
  cache; a host that only wants search can skip `saver`.
- **One SQLite file.** Pass the same `*sql.DB` to every module. Use `store.Open`, or your own handle as long as
  `store.Check` accepts it (busy timeout and foreign keys on). Tables are prefixed with the module name, migrations
  are tracked per module in `store_migrations`.
- **One locale bundle.** Pass every module's `LocaleFS()` to `i18n.NewBundle`. Message ids are prefixed with the
  module name; add a language by adding `<lang>.yaml` next to each module's `en.yaml`.
- **Run the cache.** `mediacache.Run` serves the upload queue and warmup; without it, `Resolve` waits.

`cmd/bot/main.go` is the complete version of this example, and `module/embedding_test.go` builds a bot from the
modules plus a foreign handler without `cmd/bot`.
