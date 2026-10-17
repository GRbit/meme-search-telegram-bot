package search

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

// prepParallel bounds concurrent Resolve/Content calls for one page; the
// upload limiter and the API rate limit are the real throttles.
const prepParallel = 4

// item is one meme ready to send: by file_id, or as bytes to upload.
type item struct {
	meme   memesearch.Meme
	fileID string
	data   []byte
}

// deliver sends ms to chat and returns how many were sent. Memes that cannot
// be fetched or that Telegram rejects are skipped and logged.
func (m *Module) deliver(ctx context.Context, b *bot.Bot, chat int64, ms []memesearch.Meme) int {
	items := m.prepare(ctx, ms)
	if len(items) == 0 {
		return 0
	}
	if len(items) == 1 {
		return m.sendSingles(ctx, b, chat, items)
	}
	msgs, err := b.SendMediaGroup(ctx, &bot.SendMediaGroupParams{ChatID: chat, Media: albumMedia(items)})
	if err != nil {
		m.log.WarnContext(ctx, "search: album rejected, sending one by one", "chat_id", chat, "items", len(items), "err", err)
		return m.sendSingles(ctx, b, chat, items)
	}
	m.log.DebugContext(ctx, "search: album sent", "chat_id", chat, "items", len(items))
	if len(msgs) != len(items) {
		m.log.WarnContext(ctx, "search: album answer size mismatch, not remembering file_ids",
			"sent", len(items), "got", len(msgs))
		return len(items)
	}
	for i, it := range items {
		m.remember(ctx, it, msgs[i])
	}
	return len(items)
}

// prepare turns memes into items, keeping their order and dropping failures.
func (m *Module) prepare(ctx context.Context, ms []memesearch.Meme) []item {
	out := make([]*item, len(ms))
	sem := make(chan struct{}, prepParallel)
	var wg sync.WaitGroup
	for i, meme := range ms {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = m.prepareOne(ctx, meme)
		})
	}
	wg.Wait()
	items := make([]item, 0, len(ms))
	for _, it := range out {
		if it != nil {
			items = append(items, *it)
		}
	}
	return items
}

func (m *Module) prepareOne(ctx context.Context, meme memesearch.Meme) *item {
	if m.deps.Media.UploadsEnabled() {
		fid, err := m.deps.Media.Resolve(ctx, meme)
		if err != nil {
			m.log.WarnContext(ctx, "search: resolve failed, skipping meme", "meme_id", meme.ID, "err", err)
			return nil
		}
		return &item{meme: meme, fileID: fid}
	}
	if fid, ok := m.deps.Media.Lookup(ctx, meme); ok {
		m.log.DebugContext(ctx, "search: media cache hit", "meme_id", meme.ID)
		return &item{meme: meme, fileID: fid}
	}
	m.log.DebugContext(ctx, "search: media cache miss, fetching content", "meme_id", meme.ID)
	c, err := m.deps.API.Content(ctx, meme.ID)
	if err != nil {
		m.log.WarnContext(ctx, "search: content fetch failed, skipping meme", "meme_id", meme.ID, "err", err)
		return nil
	}
	return &item{meme: meme, data: c.Data}
}

// albumMedia builds the media group. Uploads get fresh readers on every call
// because a failed request has already drained the previous ones.
func albumMedia(items []item) []models.InputMedia {
	media := make([]models.InputMedia, len(items))
	for i, it := range items {
		if it.data == nil {
			media[i] = &models.InputMediaPhoto{Media: it.fileID}
			continue
		}
		name := fmt.Sprintf("meme%d", i)
		media[i] = &models.InputMediaPhoto{Media: "attach://" + name, MediaAttachment: bytes.NewReader(it.data)}
	}
	return media
}

func (m *Module) sendSingles(ctx context.Context, b *bot.Bot, chat int64, items []item) int {
	sent := 0
	for _, it := range items {
		var photo models.InputFile = &models.InputFileString{Data: it.fileID}
		if it.data != nil {
			photo = &models.InputFileUpload{Filename: uploadName(it.meme), Data: bytes.NewReader(it.data)}
		}
		msg, err := b.SendPhoto(ctx, &bot.SendPhotoParams{ChatID: chat, Photo: photo})
		if err != nil {
			m.log.WarnContext(ctx, "search: photo rejected, skipping meme", "chat_id", chat, "meme_id", it.meme.ID, "err", err)
			continue
		}
		sent++
		m.remember(ctx, it, msg)
	}
	m.log.DebugContext(ctx, "search: photos sent one by one", "chat_id", chat, "sent", sent, "items", len(items))
	return sent
}

// remember stores the file_id of an uploaded item; items sent by file_id are already known.
func (m *Module) remember(ctx context.Context, it item, msg *models.Message) {
	if it.data == nil || msg == nil {
		return
	}
	p, ok := largestPhoto(msg.Photo)
	if !ok {
		m.log.WarnContext(ctx, "search: sent message has no photo, not remembering", "meme_id", it.meme.ID)
		return
	}
	if err := m.deps.Media.Remember(ctx, it.meme.ID, p.FileID, p.FileUniqueID); err != nil {
		m.log.WarnContext(ctx, "search: remember file_id failed", "meme_id", it.meme.ID, "err", err)
		return
	}
	m.log.DebugContext(ctx, "search: file_id remembered", "meme_id", it.meme.ID)
}

func largestPhoto(ps []models.PhotoSize) (models.PhotoSize, bool) {
	if len(ps) == 0 {
		return models.PhotoSize{}, false
	}
	best := ps[0]
	for _, p := range ps[1:] {
		if p.Width*p.Height > best.Width*best.Height {
			best = p
		}
	}
	return best, true
}

func uploadName(meme memesearch.Meme) string {
	if meme.Filename != "" {
		return meme.Filename
	}
	return fmt.Sprintf("meme%d.jpg", meme.ID)
}
