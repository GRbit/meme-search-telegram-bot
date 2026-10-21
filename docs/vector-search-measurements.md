# Vector search measurements (run on the meme-search server)

Goal: decide whether meme-search needs vector search work (upstream task J) with real numbers. Each meme has
about `words/2 + 1` embeddings (4-word chunks with step 2 plus the full text), so 18 000 memes are roughly 540k
vectors and 1-1.6 GB of heap. The current query cannot use a vector index as written (the KNN is wrapped in a
GROUP BY without an inner LIMIT), so today every vector search is an exact scan.

Open psql in the compose stack:

```sh
docker compose exec meme_search_db psql -U postgres -d meme_search
```

```sql
SELECT count(*) FROM image_embeddings;
SELECT avg(c), max(c) FROM (SELECT count(*) c FROM image_embeddings GROUP BY image_core_id) s;
SELECT pg_size_pretty(pg_relation_size('image_embeddings')), pg_size_pretty(pg_total_relation_size('image_embeddings'));
SELECT p.id, p.name, count(*) FROM image_embeddings e JOIN image_cores c ON c.id = e.image_core_id
  JOIN image_paths p ON p.id = c.image_path_id GROUP BY 1, 2 ORDER BY 3 DESC;
SHOW shared_buffers;
SHOW max_parallel_workers_per_gather;

SELECT embedding AS q FROM image_embeddings ORDER BY random() LIMIT 1 \gset

-- the global query, same shape Rails emits
EXPLAIN (ANALYZE, BUFFERS) SELECT image_core_id FROM (
  SELECT image_core_id, embedding <=> :'q' AS d FROM image_embeddings WHERE embedding IS NOT NULL) r
GROUP BY image_core_id ORDER BY min(d), image_core_id LIMIT 20;

-- folder-filtered variant: put a real image_paths.id from the per-folder query above
EXPLAIN (ANALYZE, BUFFERS) SELECT image_core_id FROM (
  SELECT image_core_id, embedding <=> :'q' AS d FROM image_embeddings WHERE embedding IS NOT NULL
    AND image_core_id IN (SELECT id FROM image_cores WHERE image_path_id = 1)) r
GROUP BY image_core_id ORDER BY min(d), image_core_id LIMIT 20;
```

Run each EXPLAIN three times (cold, then warm).

Reading the results:

- **Folder query under ~50 ms and global under ~300 ms warm:** nothing to do.
- **Folder fast, global over 0.5-1 s:** raise `shared_buffers` first (compose only); then compare fewer vectors per
  meme against today on about 20 real queries; an HNSW index plus a query rewrite plus `hnsw.iterative_scan` only
  if both are not enough.
- **Folder query slow:** check `BUFFERS` for scattered pages; a one-off
  `CLUSTER image_embeddings USING index_image_embeddings_on_image_core_id` shows whether locality is the problem.
