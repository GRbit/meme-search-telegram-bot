package search

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/mediacache"
	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

// prepParallel bounds concurrent Prepare calls (file reads or API downloads)
// for one page, so one search cannot fire a page worth of downloads at once.
const prepParallel = 4

// item is one meme ready to send: by FileID, or as Data to upload.
type item struct {
	meme  memesearch.Meme
	photo mediacache.Photo
}

// deliver sends ms to chat and returns how many were sent. Memes that cannot
// be prepared or that Telegram rejects are skipped and logged.
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
	p, err := m.deps.Media.Prepare(ctx, meme)
	switch {
	case errors.Is(err, mediacache.ErrNotPhoto):
		// GIFs and videos in the collection are expected; they just do not fit a photo album.
		m.log.DebugContext(ctx, "search: not a photo, skipping meme", "meme_id", meme.ID, "err", err)
		return nil
	case err != nil:
		m.log.WarnContext(ctx, "search: prepare failed, skipping meme", "meme_id", meme.ID, "err", err)
		return nil
	}
	m.log.DebugContext(ctx, "search: meme prepared", "meme_id", meme.ID, "by_file_id", p.Data == nil, "bytes", len(p.Data))
	return &item{meme: meme, photo: p}
}

// albumMedia builds the media group. Uploads get fresh readers on every call
// because a failed request has already drained the previous ones.
func albumMedia(items []item) []models.InputMedia {
	media := make([]models.InputMedia, len(items))
	for i, it := range items {
		if it.photo.Data == nil {
			media[i] = &models.InputMediaPhoto{Media: it.photo.FileID}
			continue
		}
		// The part name only has to be unique within the request; Photo.Name may repeat or hold odd characters.
		name := fmt.Sprintf("meme%d", i)
		media[i] = &models.InputMediaPhoto{Media: "attach://" + name, MediaAttachment: bytes.NewReader(it.photo.Data)}
	}
	return media
}

func (m *Module) sendSingles(ctx context.Context, b *bot.Bot, chat int64, items []item) int {
	sent := 0
	for _, it := range items {
		var photo models.InputFile = &models.InputFileString{Data: it.photo.FileID}
		if it.photo.Data != nil {
			photo = &models.InputFileUpload{Filename: uploadName(it), Data: bytes.NewReader(it.photo.Data)}
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
	if it.photo.Data == nil || msg == nil {
		return
	}
	p, ok := largestPhoto(msg.Photo)
	if !ok {
		m.log.WarnContext(ctx, "search: sent message has no photo, not remembering", "meme_id", it.meme.ID)
		return
	}
	if err := m.deps.Media.RememberPhoto(ctx, it.meme.ID, it.photo, p.FileID, p.FileUniqueID); err != nil {
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

func uploadName(it item) string {
	if it.photo.Name != "" {
		return it.photo.Name
	}
	return fmt.Sprintf("meme%d.jpg", it.meme.ID)
}
