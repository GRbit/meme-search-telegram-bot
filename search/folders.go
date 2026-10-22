package search

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

// listTTL keeps pickers and dir: checks off the API's 60/min listing limit
// while a user clicks around; new folders and tags show up a few minutes late.
const listTTL = 5 * time.Minute

// listCache holds one server listing (folders or tags) for listTTL.
type listCache[T any] struct {
	mu    sync.Mutex
	items []T
	at    time.Time
	have  bool
}

// get returns the cached list, refreshing it after listTTL. A failed refresh
// keeps serving the stale list: a picker that worked a minute ago should not
// vanish over one API hiccup.
func (c *listCache[T]) get(ctx context.Context, now time.Time, fetch func(context.Context) ([]T, error)) ([]T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.have && now.Sub(c.at) < listTTL {
		return c.items, nil
	}
	items, err := fetch(ctx)
	if err != nil {
		if c.have {
			return c.items, nil
		}
		return nil, err
	}
	c.items, c.at, c.have = items, now, true
	return items, nil
}

func (m *Module) caps() memesearch.Capabilities {
	if m.srv == nil {
		return memesearch.Capabilities{}
	}
	return m.srv.Capabilities()
}

// folders returns the server's folder list, or ok false when the server has
// none to give (stock server, or the listing failed).
func (m *Module) folders(ctx context.Context) ([]memesearch.Folder, bool) {
	if m.srv == nil || !m.caps().FolderList {
		return nil, false
	}
	list, err := m.folderList.get(ctx, m.now(), m.srv.Folders)
	if err != nil {
		m.log.WarnContext(ctx, "search: folder list failed", "err", err)
		return nil, false
	}
	m.log.DebugContext(ctx, "search: folder list", "count", len(list))
	return list, true
}

// resolveFolders maps typed dir: names to the server's exact names (folder
// names are case-sensitive, people type them however). Without a list every
// name passes through and the server judges it.
func (m *Module) resolveFolders(ctx context.Context, typed []string) (exact, unknown []string) {
	if len(typed) == 0 {
		return nil, nil
	}
	list, ok := m.folders(ctx)
	if !ok {
		return typed, nil
	}
	for _, t := range typed {
		name, found := matchFolder(list, t)
		if !found {
			unknown = append(unknown, t)
			continue
		}
		exact = appendNew(exact, name)
	}
	return exact, unknown
}

// matchFolder prefers an exact match and falls back to a unique
// case-insensitive one; two folders differing only in case stay ambiguous.
func matchFolder(list []memesearch.Folder, typed string) (string, bool) {
	var fold []string
	for _, f := range list {
		if f.Name == typed {
			return f.Name, true
		}
		if strings.EqualFold(f.Name, typed) {
			fold = append(fold, f.Name)
		}
	}
	if len(fold) == 1 {
		return fold[0], true
	}
	return "", false
}

// find runs one search within sc. Folders and tag_match are sent only through
// Server, whose answer says whether they were applied; sc records that.
func (m *Module) find(ctx context.Context, user int64, q string, sc *scope, limit int) ([]memesearch.Meme, error) {
	opts := sc.options(limit)
	if m.srv == nil {
		sc.foldersApplied, sc.tagMatchApplied = false, false
		return m.deps.API.Search(ctx, q, opts)
	}
	opts.Folders = sc.folders
	if sc.wantsAllTags() {
		opts.TagMatch = memesearch.TagMatchAll
	}
	page, err := m.srv.SearchPage(ctx, q, opts)
	if err != nil {
		return nil, err
	}
	sc.tagMatchApplied = sc.wantsAllTags() && !slices.Contains(page.Ignored, memesearch.ParamTagMatch)
	if sc.wantsAllTags() && !sc.tagMatchApplied {
		m.log.WarnContext(ctx, "search: server ignored tag_match=all, results have any of the tags",
			"user_id", user, "tags", sc.tags)
	}
	sc.foldersApplied = len(sc.folders) > 0 && !slices.Contains(page.Ignored, memesearch.ParamFolder)
	if len(sc.folders) > 0 && !sc.foldersApplied {
		// The client has already turned FolderFilter off; this says which user saw unfiltered results.
		m.log.WarnContext(ctx, "search: server ignored the folder filter, results are from all folders",
			"user_id", user, "folders", sc.folders)
	}
	return page.Memes, nil
}

// invalidFolder reports the server's 422 for a folder name it does not know.
func invalidFolder(err error) bool {
	var apiErr *memesearch.APIError
	return errors.As(err, &apiErr) && apiErr.Code == "invalid_folder"
}
