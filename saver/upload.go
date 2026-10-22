package saver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-telegram/bot/models"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

// uploadAPI saves through meme-search's upload API when the API backend is
// on and the server takes uploads. done is false when the folder writer must
// save instead.
func (m *Module) uploadAPI(ctx context.Context, from *models.User, name string, data []byte, log *slog.Logger) (out outcome, done bool) {
	if m.cfg.Backend != BackendAPI {
		return 0, false
	}
	if !m.deps.Caps.Capabilities().Upload {
		log.Debug("saver: server takes no uploads, writing the file")
		return 0, false
	}
	folder := m.folderName(from)
	res, err := m.deps.Uploader.Upload(ctx, []memesearch.UploadFile{{Name: name, Data: data}},
		memesearch.UploadOptions{Folder: folder, GenerateDescription: true})
	var apiErr *memesearch.APIError
	switch {
	case errors.Is(err, memesearch.ErrUnsupported):
		log.Debug("saver: upload API unsupported, writing the file", "err", err)
		return 0, false
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusUnprocessableEntity && apiErr.Code == "invalid_folder":
		// The API only fills registered folders; writing creates the folder, which
		// the owner (or meme-search's "include subfolders") registers once.
		if _, seen := m.unregistered.LoadOrStore(folder, true); seen {
			log.Debug("saver: save folder not registered in meme-search, writing the file", "folder", folder)
		} else {
			log.Info("saver: save folder not registered in meme-search, writing the file", "folder", folder)
		}
		return 0, false
	case err != nil:
		// Not the folder writer: without a shared volume the file would land
		// where meme-search never looks, after telling the user "Saved".
		m.log.Warn("saver: upload failed", "user_id", from.ID, "folder", folder, "err", err)
		return outFailed, true
	}
	switch {
	case len(res.Created) == 1:
		c := res.Created[0]
		log.Debug("saver: uploaded", "meme_id", c.Meme.ID, "file", c.Meme.Filename, "folder", res.Folder,
			"description_queued", c.DescriptionQueued)
		return outSaved, true
	case len(res.Duplicates) == 1:
		log.Debug("saver: duplicate in save folder (server)", "folder", res.Folder)
		return outDuplicate, true
	case len(res.Errors) == 1:
		e := res.Errors[0]
		m.log.Warn("saver: server refused the file", "user_id", from.ID, "code", e.Code, "message", e.Message)
		return outFailed, true
	}
	m.log.Warn("saver: upload answer without an outcome", "user_id", from.ID)
	return outFailed, true
}
