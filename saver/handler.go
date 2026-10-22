package saver

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// maxDownload is Telegram's own bot download limit; the cap guards against a
// misbehaving server, not against users.
const maxDownload = 20 << 20

type outcome int

const (
	outSaved outcome = iota
	outAlready
	outDuplicate
	outFailed
)

func (o outcome) String() string {
	return [...]string{"saved", "already_in_collection", "duplicate", "failed"}[o]
}

// match is narrow on purpose: other modules handle everything we skip.
func (m *Module) match(u *models.Update) bool {
	msg := u.Message
	if msg == nil || msg.From == nil || msg.Chat.Type != models.ChatTypePrivate {
		return false
	}
	if _, ok := pickFile(msg); !ok {
		return false
	}
	return m.deps.Permit.CanSave(msg.From.ID)
}

func (m *Module) handle(ctx context.Context, b *bot.Bot, u *models.Update) {
	msg := u.Message
	ref, _ := pickFile(msg)
	log := m.log.With("user_id", msg.From.ID, "message_id", msg.ID, "media_group", msg.MediaGroupID)
	log.Debug("saver: photo received", "file_size", ref.Size, "document", msg.Document != nil)

	key := albumKey{chatID: msg.Chat.ID, group: msg.MediaGroupID}
	if msg.MediaGroupID != "" {
		m.albums.begin(key, albumState{b: b, chatID: msg.Chat.ID, ctx: ctx, lang: msg.From.LanguageCode, replyTo: msg.ID})
	}
	out := m.process(ctx, b, msg, ref, log)
	log.Debug("saver: result", "outcome", out.String())
	if msg.MediaGroupID != "" {
		m.albums.finish(key, out)
		return
	}
	loc := m.deps.I18n.Localizer(msg.From.LanguageCode)
	m.send(ctx, b, msg.Chat.ID, msg.ID, m.singleText(loc.T, out))
}

type translate func(id string, data map[string]any) string

func (m *Module) singleText(t translate, out outcome) string {
	switch out {
	case outSaved:
		return m.withNote(t("saver.saved", nil))
	case outAlready:
		return t("saver.already_in_collection", nil)
	case outDuplicate:
		return t("saver.duplicate", nil)
	}
	return t("saver.error", nil)
}

func (m *Module) withNote(s string) string {
	if m.cfg.Note != "" {
		return s + "\n" + m.cfg.Note
	}
	return s
}

func (m *Module) flushAlbum(st albumState) {
	ctx, cancel := context.WithTimeout(st.ctx, 30*time.Second)
	defer cancel()
	t := m.deps.I18n.Localizer(st.lang).T
	count := func(id string, n int) string { return t(id, map[string]any{"Count": n}) }
	var lines []string
	if st.saved > 0 {
		lines = append(lines, m.withNote(t("saver.saved_album", map[string]any{"Count": st.saved, "Total": st.total()})))
	}
	if st.already > 0 {
		lines = append(lines, count("saver.album_already", st.already))
	}
	if st.duplicate > 0 {
		lines = append(lines, count("saver.album_duplicate", st.duplicate))
	}
	if st.failed > 0 {
		lines = append(lines, count("saver.album_failed", st.failed))
	}
	m.log.Debug("saver: album done", "saved", st.saved, "already", st.already, "duplicate", st.duplicate, "failed", st.failed)
	m.send(ctx, st.b, st.chatID, st.replyTo, strings.Join(lines, "\n"))
}

func (m *Module) send(ctx context.Context, b *bot.Bot, chatID int64, replyTo int, text string) {
	_, err := b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          chatID,
		Text:            text,
		ReplyParameters: &models.ReplyParameters{MessageID: replyTo, AllowSendingWithoutReply: true},
	})
	if err != nil {
		m.log.Warn("saver: send reply failed", "chat_id", chatID, "err", err)
	}
}

func (m *Module) process(ctx context.Context, b *bot.Bot, msg *models.Message, ref fileRef, log *slog.Logger) outcome {
	if m.deps.Known != nil {
		known, err := m.deps.Known.KnownUniqueID(ctx, ref.FileUniqueID)
		if err != nil {
			m.log.Warn("saver: unique id lookup failed", "err", err)
		} else if known {
			log.Debug("saver: known file_unique_id, skipping download")
			return outAlready
		}
	}

	data, filePath, err := m.download(ctx, b, ref)
	if err != nil {
		m.log.Warn("saver: download failed", "user_id", msg.From.ID, "message_id", msg.ID, "err", err)
		return outFailed
	}
	sum := sha256.Sum256(data)
	log.Debug("saver: downloaded", "bytes", len(data))

	if m.deps.Known != nil {
		known, err := m.deps.Known.KnownSHA256(ctx, sum)
		if err != nil {
			m.log.Warn("saver: sha256 lookup failed", "err", err)
		} else if known {
			log.Debug("saver: known sha256, not writing")
			return outAlready
		}
	}

	date := m.deps.Now()
	if msg.Date != 0 {
		date = time.Unix(int64(msg.Date), 0)
	}
	base := fileBase(date.In(m.cfg.Location), sourceName(msg), msg.ID, sum)
	ext := pickExt(filePath, ref.MIME)
	if out, done := m.uploadAPI(ctx, msg.From, base+"."+ext, data, log); done {
		return out
	}
	folder := m.saveFolder(msg.From)
	log.Debug("saver: save folder", "mode", string(m.cfg.Mode), "folder", folder)

	m.mu.Lock()
	var target string
	dup := m.hashes.contains(folder, sum)
	if !dup {
		target, err = writeAtomic(folder, base, ext, data)
		if err == nil {
			m.hashes.record(target, sum)
		}
	}
	m.mu.Unlock()
	switch {
	case dup:
		log.Debug("saver: duplicate in save folder", "folder", folder)
		return outDuplicate
	case err != nil:
		m.log.Warn("saver: write failed", "user_id", msg.From.ID, "folder", folder, "err", err)
		return outFailed
	}
	log.Debug("saver: written", "path", target)

	if m.deps.Known != nil {
		if err := m.deps.Known.AddLibraryFile(ctx, target); err != nil {
			m.log.Warn("saver: add library file failed", "path", target, "err", err)
		}
	}
	return outSaved
}

func (m *Module) saveFolder(from *models.User) string {
	return filepath.Join(m.cfg.Dir, filepath.FromSlash(m.folderName(from)))
}

// download returns the bytes and Telegram's file path (used for the extension).
func (m *Module) download(ctx context.Context, b *bot.Bot, ref fileRef) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, m.downloadTimeout)
	defer cancel()
	f, err := b.GetFile(ctx, &bot.GetFileParams{FileID: ref.FileID})
	if err != nil {
		return nil, "", fmt.Errorf("getFile: %w", err)
	}
	// The link embeds the bot token: it must never reach a log line or an
	// error string, so only its pieces are ever reported.
	link := b.FileDownloadLink(f)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, "", errors.New("build download request")
	}
	start := time.Now()
	resp, err := m.httpc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	m.log.Debug("saver: telegram file download", "status", resp.StatusCode, "duration", time.Since(start), "file_size", f.FileSize)
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("download: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, "", fmt.Errorf("download body: %w", err)
	}
	if len(data) > maxDownload {
		return nil, "", errors.New("download: file too large")
	}
	return data, f.FilePath, nil
}
