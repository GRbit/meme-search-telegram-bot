package search

import (
	"strings"

	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/i18n"
	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

// scope is what one search is limited to besides its query.
type scope struct {
	mode memesearch.Mode
	tags []string
	// folders are sent only to a Server; foldersApplied is set by the last
	// search with them. When false the search ran everywhere and the user is told.
	folders        []string
	foldersApplied bool
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
// Folders count only when the server applied them.
func (sc scope) filtered() bool { return len(sc.tags) > 0 || sc.foldersApplied }

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

func isGroupChatType(t string) bool {
	return t == string(models.ChatTypeGroup) || t == string(models.ChatTypeSupergroup)
}

// inlineScope resolves an inline query's scope per dimension, most specific
// first: the query's own filters, the group code's scope, the user's
// settings, then the group default for queries typed in groups. The mode stays Config.Mode: inline
// answers are a quick grid, and the settings mode is for the private chat.
func (m *Module) inlineScope(p prefs, o overrides, g *groupScope, chatType string) scope {
	layers := []overrides{o}
	if g != nil {
		layers = append(layers, overrides{tags: g.tags, folders: g.folders})
	}
	layers = append(layers, overrides{tags: p.Tags, folders: p.Folders})
	if isGroupChatType(chatType) {
		layers = append(layers, m.groupDefault)
	}
	sc := scope{mode: m.cfg.Mode}
	for _, l := range layers {
		if len(l.tags) > 0 {
			sc.tags = l.tags
			break
		}
	}
	for _, l := range layers {
		if len(l.folders) > 0 {
			sc.folders = l.folders
			break
		}
		if l.allFolders {
			break
		}
	}
	return sc
}
