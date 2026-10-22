package saver

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
	"github.com/grbit/meme-search-telegram-bot/store"
)

// noticeTTL drops saves the feed never reported: a folder that is not
// registered in meme-search would otherwise keep its rows forever.
const noticeTTL = 7 * 24 * time.Hour

// migrations are append-only: never edit a released step.
var migrations = []string{
	// One row per saved file waiting for its description. A batch (one
	// message, or one album) is (chat_id, reply_to) and gets one notice.
	`CREATE TABLE saver_pending (
		id INTEGER PRIMARY KEY,
		chat_id INTEGER NOT NULL,
		reply_to INTEGER NOT NULL,
		lang TEXT NOT NULL,
		base_name TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'pending',
		saved_at INTEGER NOT NULL
	);
	CREATE INDEX saver_pending_base_name ON saver_pending (base_name);
	CREATE INDEX saver_pending_batch ON saver_pending (chat_id, reply_to);`,
}

const (
	rowPending = "pending"
	rowReady   = "ready"
	rowFailed  = "failed"
)

// noticesOn reports whether a save can be followed by a "searchable now"
// notice: it needs the database and a server whose change feed exists, as
// the feed is the only place description_status arrives.
func (m *Module) noticesOn() bool {
	return m.deps.DB != nil && m.deps.Caps != nil && m.deps.Caps.Capabilities().Listing
}

// trackSaved remembers a written file until the feed says it is described.
func (m *Module) trackSaved(ctx context.Context, chatID int64, replyTo int, lang, baseName string) {
	if !m.noticesOn() {
		return
	}
	err := store.WriteTx(ctx, m.deps.DB, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO saver_pending (chat_id, reply_to, lang, base_name, saved_at)
			VALUES (?, ?, ?, ?, ?)`, chatID, replyTo, lang, baseName, m.deps.Now().Unix())
		return err
	})
	if err != nil {
		m.log.Warn("saver: track saved file", "err", err)
		return
	}
	m.log.Debug("saver: waiting for description", "chat_id", chatID, "reply_to", replyTo, "file", baseName)
}

type batch struct {
	chatID  int64
	replyTo int
}

// MemesChanged takes pages of meme-search's change feed (see
// mediacache.Cache.OnFeed) and tells users when the memes they saved became
// searchable, or could not be described. An album gets one notice once all
// its memes are settled.
//
// Files are matched by base name: saved names end in a content hash, so they
// do not collide in practice.
func (m *Module) MemesChanged(ctx context.Context, memes []memesearch.Meme) {
	if m.deps.DB == nil {
		return
	}
	if err := m.expireNotices(ctx); err != nil {
		m.log.Warn("saver: expire pending notices", "err", err)
	}
	var touched []batch
	err := store.WriteTx(ctx, m.deps.DB, func(tx *sql.Tx) error {
		touched = touched[:0]
		for _, mm := range memes {
			status := rowReady
			switch mm.DescriptionStatus {
			case memesearch.StatusReady:
			case memesearch.StatusFailed:
				status = rowFailed
			default:
				continue
			}
			rows, err := tx.QueryContext(ctx, `UPDATE saver_pending SET status = ?
				WHERE base_name = ? AND status = ? RETURNING chat_id, reply_to`, status, mm.Filename, rowPending)
			if err != nil {
				return err
			}
			for rows.Next() {
				var b batch
				if err := rows.Scan(&b.chatID, &b.replyTo); err != nil {
					_ = rows.Close()
					return err
				}
				touched = append(touched, b)
			}
			if err := rows.Close(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		m.log.Warn("saver: record descriptions", "err", err)
		return
	}
	done := map[batch]bool{}
	for _, b := range touched {
		if !done[b] {
			done[b] = true
			m.notifyIfSettled(ctx, b)
		}
	}
}

func (m *Module) expireNotices(ctx context.Context) error {
	return store.WriteTx(ctx, m.deps.DB, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM saver_pending WHERE saved_at < ?`,
			m.deps.Now().Add(-noticeTTL).Unix())
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			m.log.Debug("saver: pending notices expired", "count", n)
		}
		return nil
	})
}

// notifyIfSettled sends the batch's notice once none of it is pending, and
// forgets the batch. The rows go first: a lost notice is better than a repeated one.
func (m *Module) notifyIfSettled(ctx context.Context, b batch) {
	var ready, failed, pending int
	var lang string
	err := store.WriteTx(ctx, m.deps.DB, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT
				COALESCE(SUM(status = 'ready'), 0), COALESCE(SUM(status = 'failed'), 0),
				COALESCE(SUM(status = 'pending'), 0), COALESCE(MAX(lang), '')
			FROM saver_pending WHERE chat_id = ? AND reply_to = ?`, b.chatID, b.replyTo).
			Scan(&ready, &failed, &pending, &lang)
		if err != nil || pending > 0 {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM saver_pending WHERE chat_id = ? AND reply_to = ?`, b.chatID, b.replyTo)
		return err
	})
	if err != nil {
		m.log.Warn("saver: settle notice", "chat_id", b.chatID, "err", err)
		return
	}
	if pending == 0 && ready+failed == 0 {
		return // expired meanwhile
	}
	if pending > 0 {
		m.log.Debug("saver: batch still waiting for descriptions", "chat_id", b.chatID, "reply_to", b.replyTo,
			"pending", pending)
		return
	}
	text := m.noticeText(lang, ready, failed)
	m.log.Debug("saver: searchable notice", "chat_id", b.chatID, "reply_to", b.replyTo, "ready", ready, "failed", failed)
	b2 := m.bot.Load()
	if b2 == nil {
		m.log.Warn("saver: notice not sent, module not registered")
		return
	}
	m.send(ctx, b2, b.chatID, b.replyTo, text)
}

func (m *Module) noticeText(lang string, ready, failed int) string {
	t := m.deps.I18n.Localizer(lang).T
	if ready+failed == 1 {
		if ready == 1 {
			return t("saver.searchable", nil)
		}
		return t("saver.describe_failed", nil)
	}
	var lines []string
	if ready > 0 {
		lines = append(lines, t("saver.searchable_album", map[string]any{"Count": ready}))
	}
	if failed > 0 {
		lines = append(lines, t("saver.describe_failed_album", map[string]any{"Count": failed}))
	}
	return strings.Join(lines, "\n")
}

// migrate prepares the notice table when the host passed a database.
func migrate(db *sql.DB) error {
	// New takes no context (frozen signature); migrations are quick and local.
	ctx := context.Background()
	if err := store.Check(ctx, db); err != nil {
		return fmt.Errorf("saver: %w", err)
	}
	if err := store.Migrate(ctx, db, "saver", migrations); err != nil {
		return fmt.Errorf("saver: %w", err)
	}
	return nil
}
