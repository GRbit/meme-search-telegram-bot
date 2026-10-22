package saver

import (
	"testing"

	"github.com/go-telegram/bot/models"
)

func TestFolderFor(t *testing.T) {
	alice := &models.User{ID: 1, Username: "alice"}
	noName := &models.User{ID: 1}
	stranger := &models.User{ID: 2, Username: "bob"}
	for _, tc := range []struct {
		name string
		mut  func(*Config)
		user *models.User
		want string
	}{
		{"per_user", nil, alice, "tg-alice"},
		{"per_user without username", nil, noName, "tg-id1"},
		{"single", func(c *Config) { c.Mode = ModeSingle; c.Folder = "shared" }, alice, "shared"},
		{"nested uses a slash like meme-search", func(c *Config) { c.Mode = ModeNested }, alice, "telegram/alice"},
		{"cannot save", nil, stranger, ""},
		{"nil user", nil, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, tc.mut)
			if got := e.mod.FolderFor(tc.user); got != tc.want {
				t.Fatalf("FolderFor = %q, want %q", got, tc.want)
			}
		})
	}
}

// FolderFor must name the folder process actually writes to.
func TestFolderForMatchesTheWrittenFolder(t *testing.T) {
	for _, mode := range []Mode{ModePerUser, ModeSingle, ModeNested} {
		e := newEnv(t, func(c *Config) { c.Mode = mode })
		e.api.files["p"] = []byte("x")
		e.send(photoMsg(1, "p"))
		e.api.reply(t)
		if got := e.ls(t, e.mod.FolderFor(&models.User{ID: 1, Username: "alice"})); len(got) != 1 {
			t.Errorf("%s: files in FolderFor's folder = %v", mode, got)
		}
	}
}
