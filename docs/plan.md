# meme-search-telegram-bot - development plan

A Telegram bot for [meme-search](https://github.com/meme-search/meme-search): search a self-hosted collection
through Search API v1 (read-only, bearer token), share memes inline in any chat, and save memes posted to the bot
into a library folder. Written in Go, published as a ready-to-run image for self-hosters. @grbit is the product
owner; this file is the brief for an orchestrator agent that runs the work with subagents.

Repository: `github.com/GRbit/meme-search-telegram-bot`, cloned at
`~/ssd/go/src/github.com/GRbit/meme-search-telegram-bot` (branch `main`). Upstream reference clone (read-only, for
checking the API contract): `~/ssd/go/src/github.com/GRbit/meme-search`. Owner's running instance:
`https://memes.grbit.eu` (credentials in the bot repo's `.env.local`, gitignored, never committed or logged).
Once phase 0 lands, `docs/plan.md` in the repo is the source of truth; this file points there.

Related upstream request: [issue #197](https://github.com/meme-search/meme-search/issues/197) (the owner posts
there himself; do not comment on upstream issues). A write API and a meme listing endpoint for upstream are
tracked separately in `meme-search-upstream-task.md` (Task 3); the bot is built against the current read-only v1.

## Decisions log (grilling session 2026-10-17/18)

- Logging: stdlib `log/slog`. Modules take a `*slog.Logger`. Info: startup, enabled modules, warmup progress
  summaries. Debug: every external HTTP call (method, path, status, duration, never the token), every user
  interaction step (update type, user id, chosen branch, result count), cache hits/misses, limiter decisions.
- Go 1.27; module path `github.com/grbit/meme-search-telegram-bot`.
- Dependencies (complete list, nothing else without asking): `go-telegram/bot`, `nicksnyder/go-i18n/v2`,
  `golang.org/x/text`, `go.yaml.in/yaml/v3`, `ncruces/go-sqlite3` (cgo-free, smaller than modernc). No koanf:
  config is defaults -> YAML file -> env through an explicit table, hand-written. Locale files are YAML (same
  parser), no TOML dependency.
- golangci-lint v2 (local 2.15.0); `.golangci.yml` v2 format; CI pins the same version.
- Git: each subagent works in its own worktree next to the repo (`meme-search-telegram-bot-wt-<task>`, branch
  `feat/<task>`) and commits there. The orchestrator rebases onto `main` and fast-forwards (commits preserved, no
  merge commits), then removes worktree and branch. At the end only the main repo folder remains.
- Commit messages: whole message (subject + body) at most 196 characters; lowercase type prefix, short imperative
  subject, body says why. English. No `git push`, no tags until the owner tested locally and said ok.
- Agents work test-first. Phase 1c and 2b agent on Sonnet, others and all architecture reviews on Opus.
- Architecture review after each phase (see below): only `Strong` candidates, applied by the orchestrator.
  One `code-review` before handing the bot to the owner.
- Testing in real Telegram is done by the owner; the orchestrator says when ready and reads logs.
- Inline mode needs a private cache channel (`CACHE_CHAT_ID`); without it inline mode is off (logged at startup).
- Cache warmup from the library folder (`LIBRARY_DIR`), matched to API results by file name, SHA-256 for
  ambiguous names. Upload limiter with priorities: real-time uploads always go to the front of the queue.
- README must be a full manual: how the bot works (mermaid flowchart), cache channel explanation, step-by-step
  setup (BotFather, inline, channel, API token, folders), config reference.

## Approved feature set

Search (Search API v1: `GET /api/v1/search`, `GET /api/v1/memes/:id`, `GET /api/v1/memes/:id/content`;
contract in upstream `docs/search-api-openapi.yml` and `docs/search-api.md`; `q` at most 200 chars, `limit` 1-20,
60 requests/min/token; clients must ignore unknown response fields; meme ids are sparse integers; `filename` is a
base name without directory):

1. **Text -> memes.** Any non-command text message in a private chat is a search (mode `vector`, `SEARCH_MODE`
   can set `keyword`); the bot answers with an album of the top `RESULTS_PER_PAGE` (default 5) results and a
   separate control message with buttons (albums cannot carry keyboards). `/start` and `/help` explain usage.
2. **Inline mode.** `@bot query` in any chat shows a grid of cached photos (`InlineQueryResultCachedPhoto`);
   picking one sends it. Results not yet cached are skipped in that answer and queued for real-time upload, so the
   next query shows them. Inline answers never wait for uploads. Requires `CACHE_CHAT_ID`.
3. **"More" button.** The bot fetches `limit=20` once and keeps the result ids in memory (per search, TTL 1h);
   paging serves the next page from memory, no API call, no limit charge. Expired -> "search again" message.
4. **"Similar" buttons** (phase 4): vector search with that meme's description as the query (truncated to 200
   chars). Counts as a search.

Media cache (`mediacache`, shared by search and saver):

- Telegram sends a photo either as uploaded bytes or by `file_id`; every send returns `file_id` and
  `file_unique_id`. The cache stores them so a meme is uploaded at most once.
- Library index: `LIBRARY_DIR` (root of the meme-search image folders; default = `SAVE_DIR`) is scanned; each image
  file gets a row `(rel_path, base_name, sha256, size, mtime, file_id, file_unique_id)`. Rescan every
  `WARMUP_INTERVAL` (default 6h) picks up new/changed files (size+mtime), hashing only those.
- Resolving an API meme to a `file_id`: by meme id (`mediacache_memes`), else by `base_name` in the library index:
  one row -> use it; several rows -> fetch the bytes from the API, SHA-256, pick the row with that hash. A library
  row without `file_id` is uploaded to the cache channel. No library row -> fetch bytes from the API and upload.
  The result is remembered by meme id.
- Private chat albums never wait for the channel (decided after the third architecture review): a meme without
  a `file_id` is sent as bytes (library file if possible, else API) and the returned `file_id` is remembered,
  also on its library row. The channel serves only inline misses and warmup.
- Warmup: when `CACHE_CHAT_ID` is set and `LIBRARY_DIR` exists, a background worker uploads every library file that
  has no `file_id` to the cache channel. Files the saver writes are queued right away.
- Upload limiter: one queue for all cache channel uploads, `CACHE_UPLOADS_PER_MIN` total (default 20). Priority
  `realtime` (inline miss) always goes before `background` (warmup); background is additionally
  capped at `WARMUP_UPLOADS_PER_MIN` (default 10). A Telegram 429 pauses the queue for `retry_after`.
- `file_unique_id` and SHA-256 lookups let the saver detect memes already in the collection.

Saving (module `saver`):

5. **Photo or image document -> file** under `SAVE_DIR/<prefix><sender>/`, `<sender>` = Telegram username of the
   person talking to the bot (fallback `id<user id>`), `prefix` from `SAVE_DIR_PREFIX` (default `tg-`). Flat folders
   only; README says each folder is registered in meme-search Settings -> Image paths by its bare name. File name
   `YYYY-MM-DD_<source>_<message id>.<ext>`; `<source>` from `forward_origin` (user username or display name,
   channel username or title), non-forwarded -> sender's username. Sanitising: keep `\p{L}\p{N}`, everything else
   -> `_`, collapse runs, trim. `photo`: largest size; `document`: image MIME only; ignore stickers, GIFs, video.
   An album (`media_group_id`) gets one confirmation. Reply "Saved" plus optional `SAVE_NOTE`. Enabled when
   `SAVE_USER_IDS` is non-empty; `SAVE_DIR` default `memes/` next to the executable, created if missing.
6. **Already in the collection:** if the incoming photo's `file_unique_id` or the bytes' SHA-256 is known to the
   media cache (library-wide), do not save; reply "already in the collection". Without a library index, SHA-256
   dedup still covers the sender's own folder.

Access and limits:

7. **Tiers.** `SAVE_USER_IDS` may save and are implicitly trusted. `TRUSTED_USER_IDS`: unlimited search. Everyone
   else: `PUBLIC_DAILY_PER_USER` (default 5) and `PUBLIC_DAILY_TOTAL` (default 120) searches per day; `0` disables
   public search. Over the limit: localized message with `ADMIN_CONTACT`. Counters in SQLite keyed by user id, day
   boundary in `TZ`. Text searches and "similar" count; paging does not; inline queries count once per user per
   `INLINE_CHARGE_WINDOW` (default 10s), because Telegram sends a query per keystroke.
8. **Localization.** go-i18n v2, YAML message files embedded per module, language from Telegram `language_code`,
   fallback `DEFAULT_LANG` (default `en`); `en` and `ru` shipped.
9. **Configuration.** Defaults -> YAML file (`CONFIG_FILE`, default `config.yml` next to the executable,
   `/config/config.yml` in Docker; missing file is fine) -> env overrides, mapped by an explicit table.
   `.env.example` lists every variable with a comment (meaning, default, required or not); `config.example.yml`
   mirrors it.
10. **Packaging.** Multi-stage Dockerfile, non-root user with pinned uid (build arg, default 1000), volumes `/data`
    (SQLite), `/memes` (library + saves), `/config`. `docker-compose.yml` (bot alone) and
    `docker-compose.meme-search.yml` (joins upstream's network, `http://meme_search:3000`). GitHub Actions: test +
    lint on PR and push; release on tags -> `ghcr.io/grbit/meme-search-telegram-bot` (amd64 + arm64).

Variables (default): `TELEGRAM_BOT_TOKEN` (required), `MEME_SEARCH_URL` (required), `MEME_SEARCH_API_TOKEN`
(required, scopes `search:read,media:read`), `CACHE_CHAT_ID` (empty = inline off, no warmup), `LIBRARY_DIR`
(= `SAVE_DIR`), `WARMUP_INTERVAL` (6h), `CACHE_UPLOADS_PER_MIN` (20), `WARMUP_UPLOADS_PER_MIN` (10),
`RESULTS_PER_PAGE` (5), `SEARCH_MODE` (vector), `SAVE_DIR` (`memes/` next to binary), `SAVE_DIR_PREFIX` (tg-),
`SAVE_NOTE`, `SAVE_USER_IDS`, `TRUSTED_USER_IDS`, `PUBLIC_DAILY_PER_USER` (5), `PUBLIC_DAILY_TOTAL` (120),
`INLINE_CHARGE_WINDOW` (10s), `ADMIN_CONTACT`, `DEFAULT_LANG` (en), `TZ` (UTC), `DB_PATH` (`data/bot.db` next to
binary), `CONFIG_FILE`, `LOG_LEVEL` (info).

Rejected: hybrid search merge, tag filters, collection filters, "show description", GIF/video saving, sending as
document, `/meme` group command, reverse search by picture, several cache channels.

## Modularity (hard requirement)

The bot is a thin host around reusable modules. The owner plans a second host ("hub") that combines these modules
with his transcriber bot (`github.com/grbit/go-asr-telegram-transcriber`, also on `go-telegram/bot`) in one
Telegram bot, so every module must be importable from another Go module and usable without this repo's `main`:

- Module packages are public. Nothing a host needs lives in `internal/`.
- Handler modules implement `module.Module`:
  ```go
  type Module interface {
      Register(b *bot.Bot) // attaches handlers via RegisterHandlerMatchFunc; never WithDefaultHandler
  }
  ```
  with a constructor taking explicit dependencies (`New(cfg Config, deps Deps) (*Module, error)`). No package-level
  state, no `os.Getenv`, no global logger: the host owns config, bot, DB, locale bundle and passes them in.
- Each module has its own `Config`; the host config is the union. A host can instantiate only the saver.
- Match functions are narrow: saver matches photo/image-document messages from allowed users, search matches
  private text messages, `/start`, `/help`, inline queries and callback data prefixed `srch:`. `access` provides a
  `bot.Middleware` the host passes to search. Table names carry a module prefix.
- Each module embeds `locales/*.yaml` and exposes `LocaleFS() fs.FS`; message ids are prefixed with the module name.
- The host decides what is enabled; modules do not probe the environment.
- `docs/embedding.md` shows how another bot imports the modules; `cmd/bot/main.go` is that example.

## Architecture

```
cmd/bot/main.go       wiring only
module/               the Module interface
config/               union config: defaults -> YAML -> env table, validation, masked summary
i18n/                 bundle builder over module locale FS'es, Localizer per language_code
memesearch/           API v1 client (Search / Meme / Content), typed errors, conformance-fixture tests
store/                SQLite open (ncruces), per-module versioned migrations
mediacache/           file_id cache, library index + warmup, priority upload limiter, known-file lookups
access/               tiers, daily limits, middleware
search/               Module: text, /start, /help, inline, paging, similar
saver/                Module: photo/document -> file, album debounce, dedup
internal/tg           small shared Telegram helpers
GLOSSARY.md, docs/adr/, docs/embedding.md, docs/plan.md, README.md
Dockerfile, docker-compose*.yml, config.example.yml, .env.example, .github/workflows/
```

## Work breakdown

| # | Phase / task | Who | Depends on |
|---|---|---|---|
| 0 | go.mod with all deps, interfaces and Deps structs, locale stubs, Makefile, lint config, GLOSSARY, ADRs, README outline, licence, docs/plan.md | orchestrator | - |
| 1a | `config/` + `i18n/` | agent A | 0 |
| 1b | `memesearch/` + `store/` | agent B | 0 |
| 1c | Dockerfile, compose files, examples, CI | agent C (Sonnet) | 0 |
| 2a | `search/` | agent D (continues B) | 1 |
| 2g | `mediacache/` incl. warmup and limiter | agent G (new) | 1 |
| 2b | `saver/` | agent E (continues C, Sonnet) | 1 |
| 2c | `access/` | agent F (continues A) | 1 |
| 3 | wiring, embedding test, docs/embedding.md, README manual, code-review, fixes; hand to owner | orchestrator | 2 |
| 3t | owner tests; bugs fixed one at a time, failing test first | owner + orchestrator | 3 |
| 4 | "similar" buttons | agent D | 3t |
| 5 | push, tag `v0.1.0` -> image | orchestrator after owner's ok | 4 |

## Architecture review loop

After each phase an Opus subagent runs step 1 ("Explore") of `mattpocock-skills:improve-codebase-architecture`
with the `codebase-design` vocabulary, `GLOSSARY.md` and ADRs, scoped to the packages that phase created. No HTML
report: it returns only `Strong` candidates (files, problem, change). The orchestrator applies them (`refactor:`
commits) unless one contradicts this plan or an ADR. Weaker candidates go into the final report.

Rules for every agent: English; ASCII only in code and comments; no em-dash anywhere; only the listed
dependencies; tokens never in logs; tests without network; `gofmt` + `golangci-lint` clean; `go vet`; exported
identifiers get doc comments, other comments explain why, not what; slog debug logging at every interaction step.

## Owner's side (after the bot exists)

- API token: Settings -> API tokens, scopes `search:read,media:read`, name `telegram-bot`.
- Cache channel: private channel, bot as admin, its chat id -> `CACHE_CHAT_ID`.
- Stack 112: bot service from `docker-compose.meme-search.yml`, `dockerAppData/meme-search/memes` as `/memes`,
  SQLite in `dockerAppData/meme-search/telegram-bot/`.
- Docs: `40-docker.md` row for stack 112, `30-services.md` if anything scheduled changes.

## Implementation notes (deviations decided during the build)

- `RESULTS_PER_PAGE=0` and other zero module settings mean the module default, not an error: module `Config`s
  treat zero as "use the default" so hosts set only what they expose.
- An expired "More" button answers with a callback popup ("search again"), not a chat message.
- Access fails open: if the counter store errors, the search is allowed and the error logged at Error.
- Inline limit denials use `cache_time` 1, because go-telegram/bot drops 0 and Telegram would cache the denial 300s.
- The saver accepts jpg, png and webp only (the formats meme-search's uploader takes); a second "already saved"
  reply covers duplicates inside the sender's own folder.
- `srch:s:` is reserved but not charged until phase 4 implements "similar".
- The Dockerfile declares only `VOLUME /data`: declaring `/memes` would silently create an anonymous volume when a
  user forgets the bind mount, and saves would vanish with the container.
- Extra setting `MEME_SEARCH_TIMEOUT` (default 30s).
