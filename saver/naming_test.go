package saver

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
)

func TestSanitize(t *testing.T) {
	for in, want := range map[string]string{
		"alice":         "alice",
		"  Ann-Marie  ": "Ann_Marie",
		"__a__b__":      "a_b",
		"\u0418\u0432\u0430\u043d \u041f\u0435\u0442\u0440\u043e\u0432": "\u0418\u0432\u0430\u043d_\u041f\u0435\u0442\u0440\u043e\u0432",
		"!!!":          "",
		"a/../b":       "a_b",
		"":             "",
		"x\U0001f600y": "x_y",
	} {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSourceName(t *testing.T) {
	from := &models.User{ID: 42, Username: "sender"}
	noName := &models.User{ID: 43}
	origin := func(o models.MessageOrigin) *models.MessageOrigin { return &o }
	for name, tc := range map[string]struct {
		msg  *models.Message
		want string
	}{
		"not forwarded":             {&models.Message{From: from}, "sender"},
		"not forwarded no username": {&models.Message{From: noName}, "id43"},
		"user username": {&models.Message{From: from, ForwardOrigin: origin(models.MessageOrigin{
			MessageOriginUser: &models.MessageOriginUser{SenderUser: models.User{Username: "bob", FirstName: "Bob"}}})}, "bob"},
		"user names": {&models.Message{From: from, ForwardOrigin: origin(models.MessageOrigin{
			MessageOriginUser: &models.MessageOriginUser{SenderUser: models.User{FirstName: "Bob", LastName: "Ray"}}})}, "Bob_Ray"},
		"user nothing": {&models.Message{From: from, ForwardOrigin: origin(models.MessageOrigin{
			MessageOriginUser: &models.MessageOriginUser{}})}, "unknown"},
		"hidden": {&models.Message{From: from, ForwardOrigin: origin(models.MessageOrigin{
			MessageOriginHiddenUser: &models.MessageOriginHiddenUser{SenderUserName: "Secret Guy"}})}, "Secret_Guy"},
		"chat username": {&models.Message{From: from, ForwardOrigin: origin(models.MessageOrigin{
			MessageOriginChat: &models.MessageOriginChat{SenderChat: models.Chat{Username: "grp", Title: "Group"}}})}, "grp"},
		"chat title": {&models.Message{From: from, ForwardOrigin: origin(models.MessageOrigin{
			MessageOriginChat: &models.MessageOriginChat{SenderChat: models.Chat{Title: "My Group"}}})}, "My_Group"},
		"channel username": {&models.Message{From: from, ForwardOrigin: origin(models.MessageOrigin{
			MessageOriginChannel: &models.MessageOriginChannel{Chat: models.Chat{Username: "memes_ch", Title: "Memes"}}})}, "memes_ch"},
		"channel title": {&models.Message{From: from, ForwardOrigin: origin(models.MessageOrigin{
			MessageOriginChannel: &models.MessageOriginChannel{Chat: models.Chat{Title: "Funny Memes!"}}})}, "Funny_Memes"},
	} {
		if got := sourceName(tc.msg); got != tc.want {
			t.Errorf("%s: sourceName = %q, want %q", name, got, tc.want)
		}
	}
}

func TestFileBase(t *testing.T) {
	d := time.Date(2026, 10, 18, 23, 30, 0, 0, time.UTC)
	sum := sha256.Sum256([]byte("meme"))
	want := "2026-10-18_bob_77_" + hex.EncodeToString(sum[:])[:20]
	if got := fileBase(d, "bob", 77, sum); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestPickExt(t *testing.T) {
	for _, tc := range []struct{ path, mime, want string }{
		{"photos/file_1.JPG", "", "jpg"},
		{"documents/file_2.png", "image/jpeg", "png"},
		{"documents/file_3", "image/webp", "webp"},
		{"", "image/PNG; charset=x", "png"},
		{"", "image/unknown", "jpg"},
		{"", "", "jpg"},
	} {
		if got := pickExt(tc.path, tc.mime); got != tc.want {
			t.Errorf("pickExt(%q,%q) = %q, want %q", tc.path, tc.mime, got, tc.want)
		}
	}
}

func TestPickFile(t *testing.T) {
	photo := &models.Message{Photo: []models.PhotoSize{
		{FileID: "s", Width: 90, Height: 90, FileSize: 100},
		{FileID: "l", Width: 800, Height: 600, FileSize: 5000},
		{FileID: "l2", Width: 800, Height: 600, FileSize: 6000},
		{FileID: "m", Width: 320, Height: 240, FileSize: 900},
	}}
	if ref, ok := pickFile(photo); !ok || ref.FileID != "l2" {
		t.Fatalf("photo: %+v %v", ref, ok)
	}
	for mime, want := range map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": false, "video/mp4": false, "": false} {
		_, ok := pickFile(&models.Message{Document: &models.Document{FileID: "d", MimeType: mime}})
		if ok != want {
			t.Errorf("document %q: ok = %v, want %v", mime, ok, want)
		}
	}
	if _, ok := pickFile(&models.Message{Sticker: &models.Sticker{FileID: "st"}}); ok {
		t.Error("sticker must not match")
	}
}

// Only formats meme-search's own uploader accepts (and the media cache indexes)
// may be saved; a HEIC file in the library would never become searchable.
func TestIsSavableMIME(t *testing.T) {
	for mt, want := range map[string]bool{
		"image/jpeg": true, "image/png": true, "image/webp": true, "IMAGE/JPEG; q=1": true,
		"image/gif": false, "image/heic": false, "image/bmp": false, "image/tiff": false,
		"image/avif": false, "application/pdf": false, "": false,
	} {
		if got := isSavableMIME(mt); got != want {
			t.Errorf("isSavableMIME(%q) = %v, want %v", mt, got, want)
		}
	}
}
