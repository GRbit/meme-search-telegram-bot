package search

import (
	"context"
	"sync"
	"time"
)

// latestQueries remembers each user's newest inline query, so a debounced
// query can tell whether the user kept typing while it waited.
type latestQueries struct {
	mu   sync.Mutex
	seq  uint64
	last map[int64]uint64
}

func (l *latestQueries) begin(user int64) uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = map[int64]uint64{}
	}
	l.seq++
	l.last[user] = l.seq
	return l.seq
}

func (l *latestQueries) isLatest(user int64, seq uint64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last[user] == seq
}

// finish forgets the user once their latest query is done, so the map holds
// only users with a query in flight.
func (l *latestQueries) finish(user int64, seq uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last[user] == seq {
		delete(l.last, user)
	}
}

func (l *latestQueries) len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.last)
}

func realAfter(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// waitLatest sleeps the debounce and reports whether the query is still the
// user's latest; the caller must finish(user, seq) when ok.
func (m *Module) waitLatest(ctx context.Context, user int64, queryID string) (uint64, bool) {
	seq := m.inline.begin(user)
	tick, stop := m.after(m.cfg.InlineDebounce)
	select {
	case <-tick:
	case <-ctx.Done():
		stop()
		m.inline.finish(user, seq)
		return 0, false
	}
	if !m.inline.isLatest(user, seq) {
		m.log.DebugContext(ctx, "search: inline superseded", "user_id", user, "inline_query_id", queryID)
		return 0, false
	}
	return seq, true
}
