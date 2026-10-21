package search

import (
	"strings"

	"github.com/grbit/meme-search-telegram-bot/i18n"
	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

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

// filtered reports whether dropping the filters could change the result.
// Folders do not count: they are not applied yet.
func (sc scope) filtered() bool { return len(sc.tags) > 0 }

func (m *Module) isDefaultScope(sc scope) bool {
	return sc.mode == m.cfg.Mode && len(sc.tags) == 0 && len(sc.folders) == 0
}

// describeScope renders what differs from the defaults, e.g. "Words, tags: cat".
func (m *Module) describeScope(loc *i18n.Localizer, sc scope) string {
	var parts []string
	if sc.mode != m.cfg.Mode {
		parts = append(parts, modeName(loc, sc.mode))
	}
	if len(sc.tags) > 0 {
		parts = append(parts, loc.T("search.scope_tags", map[string]any{"Tags": strings.Join(sc.tags, ", ")}))
	}
	if len(sc.folders) > 0 {
		parts = append(parts, loc.T("search.scope_folders", map[string]any{"Folders": strings.Join(sc.folders, ", ")}))
	}
	return strings.Join(parts, ", ")
}
