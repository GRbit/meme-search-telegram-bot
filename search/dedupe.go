package search

import (
	"path"
	"strings"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

// minHashLen is the shortest hex run taken as a content hash: the current
// saver writes the first 20 hex characters of the SHA-256, older ones 32.
const minHashLen = 20

// contentHash returns the content hash in a file name, lowercased, or "".
//
// The hash is a run of 20 or more hex characters in the base name (without
// extension), bounded by the start, the end or a non-hex character. That finds
// the saver's names (2026-10-18_alice_123_<hash>.jpg), meme-search's clash
// renames (<...>_<hash>_1700000000.jpg) and other tools' hash names in any
// case. A name with two different such runs has no hash: picking one could
// hide a different meme.
func contentHash(filename string) string {
	stem := filename[:len(filename)-len(path.Ext(filename))]
	found := ""
	start := -1
	for i := 0; i <= len(stem); i++ {
		if i < len(stem) && isHex(stem[i]) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 && i-start >= minHashLen {
			run := strings.ToLower(stem[start:i])
			if found != "" && found != run {
				return ""
			}
			found = run
		}
		start = -1
	}
	return found
}

// sameContent reports whether two hashes name the same content: savers cut
// the same SHA-256 to different lengths, so one must be a prefix of the other.
func sameContent(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return strings.HasPrefix(b, a)
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// dupKeys remembers the memes already shown in one search, so the same
// picture stored in several folders appears once.
//
// File names alone do not count: generic names (image.png) repeat across
// folders for different pictures.
type dupKeys struct {
	ids map[int64]bool
	// hashes groups full hashes by their first minHashLen characters, which
	// any two hashes of the same content share.
	hashes map[string][]string
}

// newDupKeys starts with shown: memes already on screen, or the source of "Similar".
func newDupKeys(shown ...memesearch.Meme) *dupKeys {
	k := &dupKeys{ids: map[int64]bool{}, hashes: map[string][]string{}}
	for _, m := range shown {
		k.add(m)
	}
	return k
}

func (k *dupKeys) add(m memesearch.Meme) {
	k.ids[m.ID] = true
	if h := contentHash(m.Filename); h != "" {
		k.hashes[h[:minHashLen]] = append(k.hashes[h[:minHashLen]], h)
	}
}

// seen reports whether m is a meme already shown or has the content of one.
func (k *dupKeys) seen(m memesearch.Meme) bool {
	if k.ids[m.ID] {
		return true
	}
	h := contentHash(m.Filename)
	if h == "" {
		return false
	}
	for _, other := range k.hashes[h[:minHashLen]] {
		if sameContent(h, other) {
			return true
		}
	}
	return false
}

// filter keeps the first of every group of duplicates (results come best
// ranked first) and records what it keeps.
func (k *dupKeys) filter(ms []memesearch.Meme) (kept []memesearch.Meme, dropped int) {
	kept = make([]memesearch.Meme, 0, len(ms))
	for _, m := range ms {
		if k.seen(m) {
			dropped++
			continue
		}
		k.add(m)
		kept = append(kept, m)
	}
	return kept, dropped
}
