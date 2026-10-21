# Task: search modes and filters in the private chat

Status: planned. Part 1 works with the current API v1 and can start now; part 2 needs upstream task 5
(`feat/api-v1-search-filters` in `../meme-search-upstream-task.md`) deployed on the server.

## Why

The bot runs every private chat search as one vector search over the whole library (`SEARCH_MODE`, default
`vector`). That is right for inline mode: fuzzy, fast, one query per keystroke. In the private chat users also
need:

- strict search: memes whose description contains the exact phrase;
- word search: any of the words (API `keyword` mode, available today);
- search inside chosen folders (e.g. only `tg-grbit`, 1 000 memes instead of 18 000) and by tags.

Filtering happens on the server, before ranking (see upstream task 5): the bot never filters the top 20 itself,
which would return near-empty pages for narrow scopes.

## What the API offers

| Feature | API today | After upstream task 5 |
|---|---|---|
| `mode=vector`, `mode=keyword` (any word) | yes | yes |
| `mode=phrase` (exact phrase, case-insensitive) | no | yes |
| `tag` filter (repeatable, at most 10, OR) | yes, applied in SQL | plus `tag_match=all` |
| `folder` filter (repeatable, at most 10) | no | yes, applied in SQL |
| `folder` field on memes | no | yes |
| `GET /api/v1/folders`, `GET /api/v1/tags` | no | yes, with meme counts |
| applied filters echoed in `meta` | `tags` only | `folders`, `tags`, `tag_match` |

## Part 1 (now): modes and tags

1. **Search settings per user**, stored in SQLite (new migration, table e.g. `search_prefs`: user id, mode, tags,
   tag match, folders, updated at). Defaults come from config, so a user who never opens settings behaves as
   today.
2. **`/settings` command** (localized en/ru, listed in `/help`) with an inline keyboard:
   - mode: "Smart" (vector), "Any word" (keyword), "Exact phrase" (phrase, shown only when the server supports
     it);
   - tags: free text input until part 2 brings the tag list; show the chosen ones, a button to clear;
   - "Reset" back to defaults.
   Changing settings is free (no limit charge).
3. **Per-query overrides** in the message text, for one search only, overriding the saved value of that dimension:
   - `"exact phrase"` in straight or `«»` quotes -> phrase mode (part 2; until then keyword mode with those words
     and a short note that exact search needs a newer server);
   - `#tag` (repeatable) -> tag filter;
   - `dir:name` (repeatable, part 2) -> folder filter; `dir:*` ignores the saved folder scope.
   The rest of the text is the query. Parsing is a pure function with table tests (quotes, Cyrillic, `#` inside
   words, empty query after removing filters -> help message).
4. **Show the scope.** When a search is not the default, the first album caption (or a line before it) says what
   was applied, e.g. "Exact phrase, folders: tg-grbit, tags: cat", plus a "Search everywhere" button that reruns
   the query without saved filters. An empty result under filters says so ("nothing in tg-grbit") instead of the
   generic "nothing found".
5. **Paging and "similar".** The page session stores the filters, so "More" keeps them. Phase 4 "similar"
   applies the user's saved folder and tag scope.
6. **Inline mode stays vector.** Open question for the owner: should inline also apply the saved folder and tag
   scope? Recommendation: yes for folders and tags (same API call, no extra cost), never the mode.
7. **Limits:** a filtered search counts as one search, like today.

## Part 2 (after upstream task 5): folders and exact phrase

1. **Capability check** at startup and every `WARMUP_INTERVAL`: `GET /api/v1/folders` -> 404 means an older
   server; folders, the folder list and phrase mode stay hidden, and the log says so at info. Additionally, after
   every filtered search compare `meta` with what was sent: if the server dropped a filter (an older server
   ignores unknown parameters and returns unfiltered results), log a warning, tell the user the filter is not
   supported and mark the capability off.
2. **Folder and tag pickers** in `/settings`: multi-select inline keyboards from `GET /api/v1/folders` and
   `GET /api/v1/tags` (names with meme counts, paged, cached for a few minutes), "all" and "only mine" shortcuts
   (`only mine` = the user's save folder when they are a saver).
3. **Privacy:** folder names like `tg-alice` reveal who saves memes. New setting `PUBLIC_FOLDERS` (comma list,
   default empty = none): public users see and may filter only those folders; trusted users and savers see all.
   A `dir:` override naming a hidden folder is treated as unknown.
4. **Folder in results:** with `folder` in the meme object, the library match by file name can use folder plus
   file name and skip the SHA-256 download on name collisions (mediacache `Prepare` and `Resolve`).
5. Config: `SEARCH_MODE` stays the default mode for private chats; document every new setting in `.env.example`,
   `config.example.yml` and the README settings table; README gets a "Search modes and filters" section.

## Tests and process

Same rules as the rest of the project: test-first per item, `go vet`, `go test -race ./...`, golangci-lint, slog
debug lines for every user action (settings changes, applied filters, capability changes), no tokens in logs,
`docs/plan.md` implementation notes for deviations. The fake API in tests must reject or echo filters like the
real one, including an "old server" mode that ignores unknown parameters.
