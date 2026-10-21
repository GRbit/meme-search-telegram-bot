package search

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
	"github.com/grbit/meme-search-telegram-bot/store"
)

// migrations are append-only: never edit a released step.
var migrations = []string{
	`CREATE TABLE search_prefs (
		user_id INTEGER PRIMARY KEY,
		mode TEXT NOT NULL DEFAULT '',
		tags TEXT NOT NULL DEFAULT '[]',
		folders TEXT NOT NULL DEFAULT '[]',
		updated_at INTEGER NOT NULL
	)`,
}

// prefs are one user's saved search settings. Zero values mean the host's
// defaults, so a user who never opened /settings searches as before.
type prefs struct {
	Mode memesearch.Mode // "" means Config.Mode
	Tags []string
	// Folders are stored for the upstream folder filter; the API cannot
	// filter by folder yet, so they are never sent.
	Folders []string
}

func (p prefs) isZero() bool {
	return p.Mode == "" && len(p.Tags) == 0 && len(p.Folders) == 0
}

// loadPrefs returns the user's settings. A read error means defaults: a
// broken settings table must not take search down.
func (m *Module) loadPrefs(ctx context.Context, user int64) prefs {
	var p prefs
	var mode, tags, folders string
	err := m.deps.DB.QueryRowContext(ctx,
		`SELECT mode, tags, folders FROM search_prefs WHERE user_id = ?`, user).Scan(&mode, &tags, &folders)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return p
	case err != nil:
		m.log.WarnContext(ctx, "search: read settings failed, using defaults", "user_id", user, "err", err)
		return p
	}
	p.Mode = memesearch.Mode(mode)
	if err := json.Unmarshal([]byte(tags), &p.Tags); err != nil {
		m.log.WarnContext(ctx, "search: stored tags unreadable, ignoring", "user_id", user, "err", err)
	}
	if err := json.Unmarshal([]byte(folders), &p.Folders); err != nil {
		m.log.WarnContext(ctx, "search: stored folders unreadable, ignoring", "user_id", user, "err", err)
	}
	return p
}

// savePrefs stores p; settings equal to the defaults delete the row.
func (m *Module) savePrefs(ctx context.Context, user int64, p prefs) error {
	if p.isZero() {
		return store.WriteTx(ctx, m.deps.DB, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM search_prefs WHERE user_id = ?`, user)
			return err
		})
	}
	tags, err := json.Marshal(nonNil(p.Tags))
	if err != nil {
		return err
	}
	folders, err := json.Marshal(nonNil(p.Folders))
	if err != nil {
		return err
	}
	return store.WriteTx(ctx, m.deps.DB, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO search_prefs (user_id, mode, tags, folders, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (user_id) DO UPDATE SET mode = excluded.mode, tags = excluded.tags,
				folders = excluded.folders, updated_at = excluded.updated_at`,
			user, string(p.Mode), string(tags), string(folders), m.now().Unix())
		if err != nil {
			return fmt.Errorf("search: save settings: %w", err)
		}
		return nil
	})
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// pendingInputs remembers users who pressed "Set tags": their next private
// text is the tag list, not a search. Memory only: after a restart the text
// is a search again, which is harmless.
type pendingInputs struct {
	mu    sync.Mutex
	until map[int64]time.Time
}

const pendingInputTTL = 10 * time.Minute

func (p *pendingInputs) set(user int64, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.until == nil {
		p.until = map[int64]time.Time{}
	}
	for u, t := range p.until {
		if !now.Before(t) {
			delete(p.until, u)
		}
	}
	p.until[user] = now.Add(pendingInputTTL)
}

func (p *pendingInputs) active(user int64, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	t, ok := p.until[user]
	return ok && now.Before(t)
}

func (p *pendingInputs) clear(user int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.until, user)
}
