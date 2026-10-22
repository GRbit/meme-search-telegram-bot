package search

import (
	"context"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

// tags returns the server's tag list, or ok false when the server has none
// to give (stock server, or the listing failed).
func (m *Module) tags(ctx context.Context) ([]memesearch.Tag, bool) {
	if m.srv == nil || !m.caps().TagList {
		return nil, false
	}
	list, err := m.tagList.get(ctx, m.now(), m.srv.Tags)
	if err != nil {
		m.log.WarnContext(ctx, "search: tag list failed", "err", err)
		return nil, false
	}
	m.log.DebugContext(ctx, "search: tag list", "count", len(list))
	return list, true
}

// tagMatchOffered reports whether settings offer "any"/"all": only with two
// or more tags (one tag matches the same either way) on a server that applies it.
func (m *Module) tagMatchOffered(p prefs) bool {
	return len(p.Tags) > 1 && m.caps().TagMatch
}
