package saver

import (
	"path"

	"github.com/go-telegram/bot/models"
)

// FolderFor returns the library folder u's saves go to, relative to the memes
// root with forward slashes, as meme-search reports a meme's folder ("tg-alice",
// "telegram", "telegram/alice"); "" when u may not save.
//
// Search uses it for an "only mine" filter, so it must stay the folder
// process writes to (a test checks both agree).
func (m *Module) FolderFor(u *models.User) string {
	if u == nil || !m.deps.Permit.CanSave(u.ID) {
		return ""
	}
	switch m.cfg.Mode {
	case ModeSingle:
		return m.cfg.Folder
	case ModeNested:
		return path.Join(m.cfg.Folder, senderName(u))
	}
	return m.cfg.DirPrefix + senderName(u)
}
