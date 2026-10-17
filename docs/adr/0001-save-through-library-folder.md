# 1. Save memes by writing into the library folder

Search API v1 is permanently read-only (upstream `docs/search-api.md`), so the saver writes files into a folder
meme-search indexes; meme-search picks them up on rescan and describes them on its own schedule. This needs the bot
on the same host (shared volume, matching uid). A write API is proposed upstream (separate task); until it exists
this ADR stands.
