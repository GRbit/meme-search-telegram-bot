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
	// tagMatch all is sent only to a Server; tagMatchApplied says the last
	// search confirmed it, otherwise memes with any of the tags were searched.
	tagMatch        memesearch.TagMatch
	tagMatchApplied bool
}

// wantsAllTags reports whether "all of these tags" makes a difference.
func (sc scope) wantsAllTags() bool { return sc.tagMatch == memesearch.TagMatchAll && len(sc.tags) > 1 }

// privateScope applies a query's overrides on top of the user's settings,
// per dimension: #tags replace saved tags, dir: replaces saved folders.
func (m *Module) privateScope(p prefs, o overrides) scope {
	// The user's way of combining tags also applies to tags typed in the query.
	sc := scope{mode: m.effectiveMode(p), tags: p.Tags, folders: p.Folders, tagMatch: p.TagMatch}
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
	switch {
	case sc.wantsAllTags() && sc.tagMatchApplied:
		parts = append(parts, loc.T("search.scope_tags_all", map[string]any{"Tags": strings.Join(sc.tags, ", ")}))
	case len(sc.tags) > 0:
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
// first: the query's own filters, the user's settings, then the group default
// for queries typed in groups. The mode stays Config.Mode: inline answers are a
// quick grid, and the settings mode is for the private chat.
//
// Telegram never says which group a query comes from, so there is no
// per-group scope; INLINE_GROUP_SCOPE is one default for all groups.
func (m *Module) inlineScope(p prefs, o overrides, chatType string) scope {
	const userLayer = 1
	layers := []overrides{o, {tags: p.Tags, folders: p.Folders}}
	if isGroupChatType(chatType) {
		layers = append(layers, m.groupDefault)
	}
	sc := scope{mode: m.cfg.Mode}
	for i, l := range layers {
		if len(l.tags) > 0 {
			sc.tags = l.tags
			// The host's group tags were chosen to be matched any-of; the user's
			// own setting applies to their own tags only.
			if i == 0 || i == userLayer {
				sc.tagMatch = p.TagMatch
			}
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
