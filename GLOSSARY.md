# Glossary

- **meme-search**: the upstream self-hosted app (Rails + captioning service) that stores, describes and searches memes.
- **Search API v1**: meme-search's read-only HTTP API (`/api/v1/search`, `/api/v1/memes/:id`, `/content`), bearer token with scopes `search:read`, `media:read`.
- **Meme**: one image known to meme-search, identified by a sparse integer **meme id**; carries a base **file name** (no directory) and a description.
- **Library**: the folders meme-search indexes. **Library dir** (`LIBRARY_DIR`) is their common root as the bot sees it.
- **Image path**: a library folder registered in meme-search Settings -> Image paths, by bare folder name; only flat folders are served.
- **Save folder**: the per-sender folder `<prefix><sender>` the saver writes into, inside the library dir.
- **Cache channel**: a private Telegram channel (`CACHE_CHAT_ID`) the bot uploads memes to so it gets a **file_id** for each.
- **file_id**: Telegram's handle for an uploaded file; lets the bot send it again without re-uploading. Inline results accept only file_ids (or public URLs).
- **file_unique_id**: Telegram's stable id of the same file across chats and bots; used to recognise a meme the bot itself sent.
- **Library index**: the table of library files with base name, SHA-256 and (once uploaded) file_id.
- **Warmup**: background upload of library files without a file_id to the cache channel.
- **Real-time upload**: an upload a user is waiting for (album, inline miss); always ahead of warmup in the upload queue.
- **Tier**: a user's access level: public (daily limits), trusted (unlimited search), saver (may save, implies trusted).
- **Charge**: counting one search against the public daily limits.
- **Host**: a program that owns the Telegram bot and wires modules (this repo's `cmd/bot`, or a future combined bot).
- **Module**: a package that attaches narrowly matched handlers to a host's bot (`search`, `saver`); `access` and `mediacache` are services modules use.
