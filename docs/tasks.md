# Bot tasks

Two lists: what works with the current meme-search API v1, and what waits for upstream changes. Upstream
branch ids (D1, D2, E, ...) refer to `../meme-search-upstream-task.md`. Every task follows the project rules:
test-first, `go vet`, `go test -race ./...`, golangci-lint, slog debug lines for every user action, no tokens in
logs, README/.env.example/config.example.yml for new settings, `docs/plan.md` implementation notes for
deviations.

## Part 1: possible now

### 1.1 Search settings in the private chat

Today every private chat search is one vector search over the whole library (`SEARCH_MODE`).

- Per-user settings in SQLite (new migration, e.g. `search_prefs`: user id, mode, tags, folders, updated at;
  folders stay empty until 2.1). Defaults come from config, so a user who never opens settings behaves as
  today.
- `/settings` (en/ru, listed in `/help`) with an inline keyboard: mode "Smart" (vector) or "Words" (keyword:
  any word of the query, ranked so memes matching more words come first); tags as free text until 2.2 brings
  the tag list; "Reset". Changing settings is free.
- Tags use the existing v1 `tag` filter (at most 10, a meme matches any of them, applied by the server before
  ranking).
- Exact phrase search is not planned: keyword ranking is good enough.

### 1.2 Per-query overrides

In the message text, for one search only, overriding the saved value of that dimension: `#tag` (repeatable);
`dir:name` (repeatable, works after 2.1; until then a short note that the server cannot filter by folder);
`dir:*` ignores the saved folder scope. The rest is the query. A pure parsing function with table tests
(Cyrillic, `#` inside words, empty query after removing filters -> help message).

### 1.3 Show the scope

When a search is not the default, the first caption says what was applied ("Words, tags: cat") with a "Search
everywhere" button that reruns without saved filters. An empty filtered result says so instead of the generic
"nothing found". The page session stores the filters, so "More" keeps them.

### 1.4 Inline scope

Inline stays vector. Precedence, most specific first: per-query `#tag`/`dir:` -> group code (1.5) -> the user's
saved tags and folders -> optional `INLINE_GROUP_SCOPE` default for `chat_type` group/supergroup. Telegram never
tells a bot which chat an inline query comes from (only `chat_type`), so a group scope can only travel in the
query text (1.5).

### 1.5 Group scope (after 1.4; owner to confirm the design)

- A group admin runs `/memescope@bot` in the group (privacy mode needs no change for `/cmd@bot`; the caller is
  checked with `getChatAdministrators`; anonymous admins send as the group itself and count as admins).
- The bot stores the scope under a random short code and posts a message with a
  `switch_inline_query_current_chat` button that prefills `@bot g:<code> `; admins pin it.
- Inline queries carrying `g:<code>` use the group scope; the code is stripped before searching (the 200-char
  API limit applies after that). Unknown codes are ignored (debug log). Typing `@bot` by hand uses the personal
  scope. Spoofing a code is harmless: memes are not private.
- Table e.g. `group_scopes(chat_id PK, code UNIQUE, tags, folders, set_by, updated_at)`; `my_chat_member`
  left/kicked deletes the row. Commands: show, set, reset, repost button, off.
- Limits are unchanged: the user typing the query pays, one charge per `INLINE_CHARGE_WINDOW`.

### 1.6 "Similar" buttons (phase 4 of `docs/plan.md`)

Vector search with the meme's description as the query (truncated to 200 chars), honouring the user's scope;
re-enables charging `srch:s:`. Switches to `GET /api/v1/memes/:id/similar` when H1 lands (2.4).

## Part 2: waits for upstream

### 2.1 Folder filter (needs D1 + D2)

Done: capabilities, `/settings` picker, `dir:`, inline and group scope, and mediacache matching by folder plus
file name, and "Only mine" (cmd/bot passes the saver's `FolderFor`). Not done yet: the picker for `/memescope`
(admins type `dir:` names, checked against the list).

- Capability check at startup and every `WARMUP_INTERVAL`: `GET /api/v1/folders` 404 -> older server, folder
  options stay hidden (info log). After every filtered search compare `meta.folders` with what was sent: an
  older server ignores unknown parameters and returns unfiltered results; then warn, tell the user, mark the
  capability off.
- `/settings` folder picker from `GET /api/v1/folders` (names with counts, paged, cached a few minutes), with
  "all" and "only mine" (the user's save folder) shortcuts; group scope (1.5) gets the same picker.
- With the `folder` field on memes, mediacache matches library files by folder plus file name and skips the
  SHA-256 download on name collisions.

### 2.2 Tag picker (needs E)

Done for `/settings`: "Pick tags" from the list, "Any tag"/"All tags". `/memescope` gets its picker with folders
(step 6 of the part 2 order); until then admins type `#tags`, matched any-of.

`/settings` and `/memescope` pick tags from `GET /api/v1/tags`; optional "all of these tags" via `tag_match=all`.

### 2.3 Listing and change feed (needs G) - done

Done; see `docs/plan.md` implementation notes. Deviation: the notice matches saved files by base name (they end
in a content hash) instead of storing meme ids, which the folder writer never learns. `SAVE_NOTIFY=false` turns
the notice off. Not done: the occasional full relisting the API docs suggest against changes missed during a
poll; a new meme changes again when its description arrives, so a miss heals itself.

- Warmup without filesystem access: enumerate memes via the cursor listing instead of scanning `LIBRARY_DIR`
  (the library lives on another server today). Keep the folder scan for setups that have it.
- Poll the feed: queue new memes for warmup right away instead of every `WARMUP_INTERVAL`.
- With `description_status`: tell a user who saved a meme when it became searchable (optional, needs storing
  the saved meme ids per user).

### 2.4 Similar by id (needs H1)

Done, with one deviation: a session with tags or folders keeps the description query, because the endpoint has
no filters and filtering 20 neighbours locally could leave nothing.

Replace 1.6's description query with `GET /api/v1/memes/:id/similar` (exact, no re-embedding).

### 2.5 More than 20 results (needs H2)

Done. Fetches stay free, as paging always was: they continue a charged search.

"More" fetches the next page with `offset` when the 20 cached ids run out.

### 2.6 Save through the API (needs F2, maintainer decision pending) - done

Done as `SAVE_BACKEND=api` (default `folder`). Upload goes to the save folder of `SAVE_MODE` with
`generate_description=true`. Deviation: the API only fills registered folders, so `invalid_folder` (and a server
without the API or token) falls back to the folder writer; transient upload errors do not (the user is told to
retry), since a file written without a shared volume would never be found. One request per album item.

New saver backend: `POST /api/v2/memes` with a `media:write` token into a chosen folder, so saving works without
a shared volume and the meme is searchable without waiting for a rescan. Keep the folder writer for setups
without the write API.

### 2.7 Nested save folders (needs L) - done

Done as specified. The user folder is the bare sender name (`telegram/alice/`), without `SAVE_DIR_PREFIX`,
matching L's `telegram/<user>/`. Nothing to detect: the bot only writes files, and on a server without L the
owner simply does not choose this mode.

`SAVE_MODE=nested`: every saver gets `SAVE_DIR/<SAVE_FOLDER>/<user>/` (one level, like L supports). With the parent
registered once in meme-search with "include subfolders", a user added to `SAVE_USER_IDS` is searchable after
the next scan of the parent, with no server change. Existing `tg-*` folders stay as they are (moving them loses
meme-search descriptions until upstream K exists).
