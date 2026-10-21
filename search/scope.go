package search

import "github.com/grbit/meme-search-telegram-bot/memesearch"

// scope is what one search is limited to besides its query.
type scope struct {
	mode memesearch.Mode
	tags []string
	// folders were asked for but are not applied: the API has no folder
	// filter yet, so the search runs everywhere and the user is told.
	folders []string
}

// privateScope applies a query's overrides on top of the user's settings,
// per dimension: #tags replace saved tags, dir: replaces saved folders.
func (m *Module) privateScope(p prefs, o overrides) scope {
	sc := scope{mode: m.effectiveMode(p), tags: p.Tags, folders: p.Folders}
	if len(o.tags) > 0 {
		sc.tags = o.tags
	}
	switch {
	case len(o.folders) > 0:
		sc.folders = o.folders
	case o.allFolders:
		sc.folders = nil
	}
	return sc
}

func (sc scope) options(limit int) memesearch.SearchOptions {
	return memesearch.SearchOptions{Mode: sc.mode, Limit: limit, Tags: sc.tags}
}
