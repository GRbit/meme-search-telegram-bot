# 3. A media cache module with a cache channel and library warmup

Inline results need file_ids, and upstream asks bots not to publish media URLs, so memes are uploaded to a private
cache channel once and their file_ids kept in SQLite. The API has no listing endpoint and meme ids are sparse, so
warmup reads the library folder instead and matches API results by base file name, with SHA-256 deciding between
files that share a name. All channel uploads share one limiter in which real-time uploads always go first and
warmup is capped. The cache is its own package because search (albums, inline) and saver (already in collection)
both use it.
