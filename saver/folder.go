package saver

import (
	"path"

	"github.com/go-telegram/bot/models"
)

// FolderFor returns the library folder u's saves go to, relative to the memes
// root with forward slashes, as meme-search reports a meme's folder ("tg-alice",
// "telegram", "telegram/alice"); "" when u may not save.
//
// Search uses it for an "only mine" filter.
func (m *Module) FolderFor(u *models.User) string {
	if u == nil || !m.deps.Permit.CanSave(u.ID) {
		return ""
	}
	return m.folderName(u)
}

// folderName is FolderFor without the permission check; process uses it too,
// so search and the writer cannot disagree on the layout.
func (m *Module) folderName(u *models.User) string {
	switch m.cfg.Mode {
	case ModeSingle:
		return m.cfg.Folder
	case ModeNested:
		// No DirPrefix: the parent already marks these as bot folders.
		return path.Join(m.cfg.Folder, senderName(u))
	}
	return m.cfg.DirPrefix + senderName(u)
}
