package search

import (
	"path"
	"strings"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

// hashLen is the hex length of the content hash the saver puts into names
// (the first 20 hex characters of the SHA-256).
const hashLen = 20

// contentHash returns the content hash in a file name, lowercased, or "".
//
// The hash is a run of exactly 20 hex characters in the base name (without
// extension), bounded by the start, the end or a non-hex character. That finds
// the saver's names (2026-10-18_alice_123_<hash>.jpg), meme-search's clash
// renames (<...>_<hash>_1700000000.jpg) and other tools' hash names in any case.
// A longer hex run is not a hash, and a name with two different 20-hex runs
// has no hash: picking one could hide a different meme.
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
		if start >= 0 && i-start == hashLen {
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

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// dupKeys remembers the memes already shown in one search, so the same
// picture stored in several folders appears once.
//
// File names alone do not count: generic names (image.png) repeat across
// folders for different pictures.
type dupKeys struct {
	ids    map[int64]bool
	hashes map[string]bool
}

// newDupKeys starts with shown: memes already on screen, or the source of "Similar".
func newDupKeys(shown ...memesearch.Meme) *dupKeys {
	k := &dupKeys{ids: map[int64]bool{}, hashes: map[string]bool{}}
	for _, m := range shown {
		k.add(m)
	}
	return k
}

func (k *dupKeys) add(m memesearch.Meme) {
	k.ids[m.ID] = true
	if h := contentHash(m.Filename); h != "" {
		k.hashes[h] = true
	}
}

// seen reports whether m is a meme already shown or has the content hash of one.
func (k *dupKeys) seen(m memesearch.Meme) bool {
	if k.ids[m.ID] {
		return true
	}
	h := contentHash(m.Filename)
	return h != "" && k.hashes[h]
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
