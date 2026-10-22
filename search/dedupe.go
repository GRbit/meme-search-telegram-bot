package search

import (
	"path"
	"regexp"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

// savedName matches the stem of a name the saver writes:
// <YYYY-MM-DD>_<source>_<message id>_<first 20 hex of the SHA-256>.
//
// Only the whole layout counts. A 20-hex word alone could be anything (an id,
// part of a hash of something else), and a false match would hide a different
// meme; names that merely look similar keep their memes apart.
var savedName = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}_.+_\d+_([0-9a-f]{20})$`)

// contentHash returns the content hash part of a name the saver wrote, or "".
func contentHash(filename string) string {
	if sm := savedName.FindStringSubmatch(filename[:len(filename)-len(path.Ext(filename))]); sm != nil {
		return sm[1]
	}
	return ""
}

// dupKeys remembers the memes already shown in one search, so the same
// picture stored in several folders appears once.
type dupKeys struct {
	ids    map[int64]bool
	names  map[string]bool
	hashes map[string]bool
}

// newDupKeys starts with shown: memes already on screen, or the source of "Similar".
func newDupKeys(shown ...memesearch.Meme) *dupKeys {
	k := &dupKeys{ids: map[int64]bool{}, names: map[string]bool{}, hashes: map[string]bool{}}
	for _, m := range shown {
		k.add(m)
	}
	return k
}

func (k *dupKeys) add(m memesearch.Meme) {
	k.ids[m.ID] = true
	if m.Filename != "" {
		k.names[m.Filename] = true
	}
	if h := contentHash(m.Filename); h != "" {
		k.hashes[h] = true
	}
}

// seen reports whether m is a meme already shown, has the same file name as
// one, or the same saved content hash.
func (k *dupKeys) seen(m memesearch.Meme) bool {
	if k.ids[m.ID] || (m.Filename != "" && k.names[m.Filename]) {
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
