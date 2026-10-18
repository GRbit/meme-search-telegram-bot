package saver

import (
	"encoding/hex"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/go-telegram/bot/models"
)

const unknownName = "unknown"

// sanitize keeps letters and digits and turns everything else into "_", so the
// result is safe as a path element and stable across filesystems.
func sanitize(s string) string {
	var sb strings.Builder
	lastUnderscore := true // also trims leading separators
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			sb.WriteRune(r)
			lastUnderscore = false
		} else if !lastUnderscore {
			sb.WriteByte('_')
			lastUnderscore = true
		}
	}
	return strings.TrimRight(sb.String(), "_")
}

func orUnknown(s string) string {
	if s = sanitize(s); s != "" {
		return s
	}
	return unknownName
}

// senderName names the folder owner; the id fallback covers users without a username.
func senderName(u *models.User) string {
	if s := sanitize(u.Username); s != "" {
		return s
	}
	return "id" + strconv.FormatInt(u.ID, 10)
}

func chatName(c models.Chat) string {
	if c.Username != "" {
		return orUnknown(c.Username)
	}
	return orUnknown(c.Title)
}

// sourceName says where a meme came from: the original author for forwards,
// the sender otherwise.
func sourceName(msg *models.Message) string {
	if fo := msg.ForwardOrigin; fo != nil {
		switch {
		case fo.MessageOriginUser != nil:
			u := fo.MessageOriginUser.SenderUser
			if u.Username != "" {
				return orUnknown(u.Username)
			}
			return orUnknown(u.FirstName + " " + u.LastName)
		case fo.MessageOriginHiddenUser != nil:
			return orUnknown(fo.MessageOriginHiddenUser.SenderUserName)
		case fo.MessageOriginChat != nil:
			return chatName(fo.MessageOriginChat.SenderChat)
		case fo.MessageOriginChannel != nil:
			return chatName(fo.MessageOriginChannel.Chat)
		}
	}
	return senderName(msg.From)
}

// hashPrefixLen is in hex chars: 80 bits keep names unique per folder in
// practice while staying short enough to read.
const hashPrefixLen = 20

func fileBase(date time.Time, source string, msgID int, sum [32]byte) string {
	return fmt.Sprintf("%s_%s_%d_%s", date.Format("2006-01-02"), source, msgID, hex.EncodeToString(sum[:])[:hashPrefixLen])
}

var mimeExt = map[string]string{
	"image/jpeg": "jpg",
	"image/jpg":  "jpg",
	"image/png":  "png",
	"image/webp": "webp",
}

// pickExt prefers Telegram's own file path (it reflects the real format), then
// the declared MIME type, then jpg because photos are always re-encoded as JPEG.
func pickExt(filePath, mimeType string) string {
	if e := sanitize(strings.TrimPrefix(path.Ext(filePath), ".")); e != "" {
		return strings.ToLower(e)
	}
	mt, _, _ := strings.Cut(strings.ToLower(mimeType), ";")
	if e, ok := mimeExt[strings.TrimSpace(mt)]; ok {
		return e
	}
	return "jpg"
}

// isSavableMIME accepts the still-image formats meme-search's own uploader
// takes (jpg, png, webp); anything else would sit in the library unsearchable.
// GIF is left out on purpose: animations are not memes for this collection.
func isSavableMIME(mimeType string) bool {
	mt, _, _ := strings.Cut(strings.ToLower(mimeType), ";")
	_, ok := mimeExt[strings.TrimSpace(mt)]
	return ok
}

// fileRef is the Telegram file chosen from a message.
type fileRef struct {
	FileID       string
	FileUniqueID string
	Size         int64
	MIME         string
}

// pickFile returns the file to save, or false when the message has none.
func pickFile(msg *models.Message) (fileRef, bool) {
	if len(msg.Photo) > 0 {
		best := msg.Photo[0]
		for _, p := range msg.Photo[1:] {
			a, b := p.Width*p.Height, best.Width*best.Height
			if a > b || (a == b && p.FileSize > best.FileSize) {
				best = p
			}
		}
		return fileRef{FileID: best.FileID, FileUniqueID: best.FileUniqueID, Size: int64(best.FileSize)}, true
	}
	if d := msg.Document; d != nil && isSavableMIME(d.MimeType) {
		return fileRef{FileID: d.FileID, FileUniqueID: d.FileUniqueID, Size: d.FileSize, MIME: d.MimeType}, true
	}
	return fileRef{}, false
}
