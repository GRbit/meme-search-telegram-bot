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
   An answer with any miss carries a localized "memes are loading" results button (start parameter `loading`,
   which `/start` treats like no payload) and `cache_time` 1.
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
  Order: newest first by the first valid `YYYY-MM-DD` in the base name, ties by `rel_path`, undated files last.
- Upload limiter: one queue for all cache channel uploads, `CACHE_UPLOADS_PER_MIN` total (default 20). Priority
  `realtime` (inline miss) always goes before `background` (warmup); background is additionally
  capped at `WARMUP_UPLOADS_PER_MIN` (default 10). A Telegram 429 pauses the queue for `retry_after`.
  When a user's new inline query queues misses, that user's still-queued uploads from earlier queries move to
  `background` unless another user's current query (or a `Resolve` caller) still wants them.
- `file_unique_id` and SHA-256 lookups let the saver detect memes already in the collection.

Saving (module `saver`):

5. **Photo or image document -> file** under `SAVE_DIR/<prefix><sender>/`, `<sender>` = Telegram username of the
   person talking to the bot (fallback `id<user id>`), `prefix` from `SAVE_DIR_PREFIX` (default `tg-`). Flat folders
   only; README says each folder is registered in meme-search Settings -> Image paths by its bare name. With
   `SAVE_MODE=single` every sender shares `SAVE_DIR/<SAVE_FOLDER>/` instead, with `SAVE_MODE=nested` each sender
   gets `SAVE_DIR/<SAVE_FOLDER>/<sender>/` (see Implementation notes). File name
   `YYYY-MM-DD_<source>_<message id>_<sha256 first 20 hex>.<ext>`; `<source>` from `forward_origin` (user username
   or display name, channel username or title), non-forwarded -> sender's username. Sanitising: keep `\p{L}\p{N}`, everything else
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
(required, scopes `search:read,media:read`), `CACHE_CHAT_ID` (empty = inline off, no warmup), `LIBRARY_DIR` (=
`SAVE_DIR`), `WARMUP_INTERVAL` (6h), `CACHE_UPLOADS_PER_MIN` (20), `WARMUP_UPLOADS_PER_MIN` (10), `RESULTS_PER_PAGE`
(5), `SEARCH_MODE` (vector), `INLINE_RESULTS` (3), `INLINE_DEBOUNCE` (1s), `SAVE_DIR` (`memes/` next to binary),
`SAVE_MODE` (per_user), `SAVE_FOLDER` (telegram), `SAVE_DIR_PREFIX` (tg-), `SAVE_NOTE`, `SAVE_USER_IDS`,
`TRUSTED_USER_IDS`, `PUBLIC_DAILY_PER_USER` (5), `PUBLIC_DAILY_TOTAL` (120), `INLINE_CHARGE_WINDOW` (10s),
`ADMIN_CONTACT`, `DEFAULT_LANG` (en), `TZ` (UTC), `DB_PATH` (`data/bot.db` next to binary), `CONFIG_FILE`,
`LOG_LEVEL` (info).

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
- Inline answers with cache misses also use `cache_time` 1 and show a "memes are loading" button, so an empty or
  partial grid explains itself and is not replayed after the uploads finish; all-hit answers keep 30s.
- Extra setting `INLINE_RESULTS` (default 3, was a fixed 20): each inline query asks for that many memes and
  queues only their misses, so typing does not flood the upload queue. Valid 1..20, the API's `limit` cap.
- Extra setting `INLINE_DEBOUNCE` (default 1s, 0 off, max 5s because Telegram expires inline queries after
  about 10s and the search and answer need the rest): an inline query waits that long and is dropped unanswered
  if the same user sent a newer one. The access middleware charges at arrival, before the wait, so dropped
  queries cost what they did before (free inside `INLINE_CHARGE_WINDOW`). `search.Config.InlineDebounce` zero
  means off, like `InlineChargeWindow`; the 1s default lives in the config table.
- The saver accepts jpg, png and webp only (the formats meme-search's uploader takes); a second "already saved"
  reply covers duplicates inside the sender's own folder.
- The Dockerfile declares only `VOLUME /data`: declaring `/memes` would silently create an anonymous volume when a
  user forgets the bind mount, and saves would vanish with the container.
- Extra setting `MEME_SEARCH_TIMEOUT` (default 30s).
- Saved file names end in the first 20 hex chars of the SHA-256 (the hash dedup already computes), so the same
  message id with other bytes no longer gets a `_2` suffix. Files saved before this keep their old names.
- Demoted inline uploads (older queries of the same user) go to the head of `background`, ahead of warmup,
  and give back their in-flight slot (64): otherwise stale jobs would fill the bound and new misses would be
  dropped. A current query wanting a demoted meme moves it back to `realtime`. Singleflight per meme and the
  `WARMUP_UPLOADS_PER_MIN` cap (which demoted jobs obey) are unchanged. A query with no misses demotes nothing.
- Warmup order by file name date is parsed in Go, not stored in a column: the upload queue itself must be kept
  sorted (otherwise files found by a later scan or saved meanwhile land behind the older backlog), so every
  queued job needs the date in memory anyway, and a column would need a backfill pass for unchanged rows that a
  scan never rewrites. Parsing and sorting 50k pending names takes about 50ms per scan. A full background queue
  evicts its oldest warmup job for a newer file (the evicted one stays pending and is queued again later); jobs
  someone waits for (realtime, demoted inline) rank before all warmup and are never evicted.
- Extra settings `SAVE_MODE` (`per_user` default, or `single`) and `SAVE_FOLDER` (default `telegram`): single mode
  writes every allowed user's memes into `SAVE_DIR/<SAVE_FOLDER>`, so meme-search needs one registered folder and
  the folder duplicate check covers all users; `SAVE_DIR_PREFIX` is ignored there. `SAVE_FOLDER` is validated
  (letters, digits, `-`, `_`), not sanitized: the owner registers exactly that name in meme-search.
- `SAVE_MODE=nested` (tasks 2.7) writes `SAVE_DIR/<SAVE_FOLDER>/<sender>/` without `SAVE_DIR_PREFIX`: the parent
  already marks the folders as the bot's, and upstream L names the child path `<parent>/<child>`. The folder
  duplicate check stays per sender, like per_user. No capability check: the mode only changes where files are
  written, and it is useful only with a server that registers subfolders, which the owner picks knowingly.
- A library row without its own `file_id` inherits `file_id` and `file_unique_id` from any row with the same
  SHA-256 (indexed since the first migration), on scan and on `AddLibraryFile`: Telegram's file_id follows content,
  so a renamed or moved folder costs a rehash, not days of re-uploads. It works within one scan because old rows
  are deleted after the walk. Failures are not inherited (one more attempt is cheap). Not covered: old rows
  deleted by an earlier scan (e.g. files moved out and back later): `mediacache_memes` stores no hash, so its
  file_ids cannot be matched to content safely; those files are uploaded again.
- Search settings (docs/tasks.md 1.1) live in the search module's own table `search_prefs`; `search.Deps.DB` is
  therefore required now. A settings row equal to the defaults is deleted, so "Reset" leaves no trace. The tag
  list a user types after "Set tags" is caught by a handler registered before text search and without the access
  middleware, so it is neither a search nor charged; the waiting state is in memory (10 min) and a restart just
  turns that text into a search.
- Per-query overrides (tasks 1.2) are parsed by word: `#x` is a tag, `dir:x` a folder (prefix case-insensitive),
  `dir:*` drops saved folders; explicit `dir:x` wins over `dir:*`. Trailing `,` or `;` is cut from tags. Folders
  are parsed and kept in the session but never sent (no API support); the first control message says all folders
  were searched. `Chargeable` parses the same way, so filters-only or over-limit tag messages are not charged.
- Scope display (tasks 1.3): the scope line sits in the control message of page 1 (albums cannot carry a caption
  plus buttons reliably). "Search everywhere" (`srch:e:<token>`) keeps the mode and drops tags and folders; it is
  offered only with tags, since folders are not applied yet and mode is not a filter. It is charged like a search.
  An empty tagged result keeps a session only for that button.
- Inline scope (tasks 1.4) is resolved per dimension (tags, folders) from layers: query filters, the user's saved
  settings, then `INLINE_GROUP_SCOPE` when `chat_type` is group or supergroup. Inline keeps `SEARCH_MODE`, ignoring
  the user's saved mode. `INLINE_GROUP_SCOPE` uses the query filter syntax and is validated to hold filters only.
- "Similar" (tasks 1.6, phase 4): buttons "Similar N" sit in the control message of each page, one per meme that
  has a description (N is its position in the album); "More" now removes only itself so older pages keep them.
  A press searches by the description (first 200 characters) in vector mode within the session's tags and drops
  the source meme from the results. `srch:s:` and `srch:e:` are charged by `Chargeable`, which cannot see sessions:
  a press on an expired session is charged although nothing is searched (rare, 1h TTL).
- Group scope (tasks 1.5): table `search_group_scopes` (chat id, 6-char code from an alphabet without look-alikes,
  tags, folders, set_by, updated_at). `set` and `reset` keep the code so a pinned button never breaks; `off`
  deletes the row and old buttons fall back to personal settings. Admin check: `sender_chat` equal to the group
  (anonymous admins) or a `getChatAdministrators` match; the anonymous-admin placeholder user alone is not
  trusted. `/memescope@otherbot` is ignored. `g:<code>` is parsed anywhere in the query (first one wins) and
  ignored in private searches. `my_chat_member` left/kicked deletes the row; a group upgraded to a supergroup
  keeps working through its code although its chat id changed (the stale row is dropped only by a later "off").
- Server capabilities (part 2): `memesearch.HTTPClient` probes the optional endpoints at startup and every
  `WARMUP_INTERVAL` (`Capabilities`, read without blocking through `CapabilitySource`). A 404 from a new route means
  an older server; for `/memes/:id/similar` only a 404 without the API's `not_found` envelope does, since the route
  also 404s for an unknown meme. 5xx, 429, 401 and network errors keep the previous value. Search parameters are
  confirmed from `meta` on every search answer (`Page.Ignored`), so a downgrade is seen at once; the plain `Search`
  turns an ignored parameter into `ErrUnsupported` rather than return unfiltered memes. The upload probe posts an
  empty form with the one API token (422 = usable, 403 = token lacks `media:write`, logged at Info); nothing is stored. The
  `Client` interface is unchanged so existing fakes stay valid; new endpoints are `HTTPClient` methods.
- Folder filter (tasks 2.1): `search.Deps.Server` (defaults to `Deps.API` when it implements it) gives the newer
  endpoints; with it folders are always sent and `Page.Ignored` decides whether they applied, so an old server
  costs nothing and the user gets the old "all folders were searched" note. Typed `dir:` names are matched to the
  server's list (exact, else a unique case-insensitive match); unknown names get a reply instead of a search. A
  search-owned middleware ahead of the host's refuses them uncharged when a fresh (under 5 minutes) folder list is
  cached; it never fetches, so the first `dir:` search after the cache expires is still charged. Inline passes unknown names on (the server's 422 is an empty
  grid). The folder list is cached 5 minutes and a failed refresh keeps the stale list. Picker buttons carry the
  list index plus an FNV hash of the name: names can be 300 characters, callback data 64 bytes, and a changed list
  must not select the wrong folder. "Only mine" needs `Deps.OwnFolder`, which the host does not wire yet.
- Tag picker (tasks 2.2): the folder picker became a generic `picker` (folders, tags) over a 5-minute list cache.
  "All tags" is stored as `search_prefs.tag_match` (migration step 3) and forgotten when the tags are cleared. It
  applies to the user's own tags, saved or typed as `#tag`, and is offered only with two or more tags on a server
  with `tag_match`. A group's tags and `INLINE_GROUP_SCOPE` stay any-of. An ignored `tag_match` (old server)
  gives a Warn log and a note under the results, like folders.
- Similar by id (tasks 2.4): "Similar N" calls `GET /api/v1/memes/:id/similar` (limit 20) when the server has it
  and the session has no tags or folders. It falls back to the description query on an older server, when the
  meme is gone (404) or has no embedding yet (empty answer), and always for a filtered session. A rate limit is
  reported as before, not retried as a second request.
- Offset paging (tasks 2.5): a session keeps the server's `meta.next_offset`; "More" past the fetched memes asks
  for the next 20 with that offset, appends them (dropping ids already shown or the "Similar" source: the library
  can shift between requests), and "Page n of m+" says more exist. Only `next_offset` enables it, so an older
  server pages 20 as before; an ignored offset (downgrade mid-session) ends paging with "No more results" rather
  than repeating page 1. Fetches are free like paging and serialized per session. Similar by id never pages.
- Change feed (tasks 2.3): `mediacache` polls `GET /api/v1/memes` every `FEED_INTERVAL` (1m) while the probe
  reports the listing, at most 20 pages of 100 per poll (the listing has its own 60/min budget, so a first full
  listing of a big library spreads over several polls). The cursor lives in `mediacache_feed` and is stored after
  every page; `422 invalid_cursor` resets it. No poll at startup: the probe has not answered yet. With a library
  dir, a feed meme whose file name is not indexed triggers an early rescan (the scan reads files, no API download).
  Without one, photo memes go to `mediacache_api_memes` and are uploaded from the API at background priority
  (queue key `api:<id>`, so a realtime job for the same meme is never merged into a job nobody waits for); every
  `WARMUP_INTERVAL` the still-pending ones are queued again, a 404 or non-photo content drops the row. Deleted memes
  are not in the feed; the periodic full relisting the API docs suggest is skipped, as a missed change is followed
  by the description change anyway.
- "Searchable now" notice: the saver records each written file in `saver_pending` (only while the server has the
  listing) keyed by the message it replied to, and `MemesChanged` (fed by `mediacache.Cache.OnFeed`) marks rows
  ready or failed by base name. One reply per message or album once nothing in it is pending; rows older than 7
  days are dropped silently (e.g. a folder not registered in meme-search).
- Folder matching (tasks 2.1, mediacache side): a meme with `folder` is looked up at `<folder>/<filename>` in the
  library index first, since `LIBRARY_DIR` is meme-search's memes root; a hit settles name collisions without
  downloading the content to compare hashes. No row there (an older server without the field, or `LIBRARY_DIR`
  mounted at another level) falls back to the base-name lookup. For the feed's rescan trigger a name found in
  another folder counts as indexed only when the index has no file in the meme's folder at all (layouts differ);
  otherwise every poll in such a setup would rescan.
- Save through the API (tasks 2.6): `SAVE_BACKEND=api` uploads each saved file (API token with `media:write`) into
  `FolderFor`'s folder, `generate_description=true`, while the probe reports the upload capability. Created ->
  "Saved" (and the searchable notice tracks the stored name), duplicate in folder -> "Already saved", per-file
  error or a failed request -> "Could not save". `ErrUnsupported` and `422 invalid_folder` fall back to the folder
  writer, which creates the folder for the owner (or upstream L) to register. No local file means no
  `AddLibraryFile`; the feed warms the meme up.
