package search

import (
	"crypto/rand"
	"encoding/base64"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

const (
	morePrefix    = CallbackPrefix + "m:"
	similarPrefix = CallbackPrefix + "s:" // reserved for "similar" buttons (phase 4)

	// maxSessions bounds memory: at 20 memes per search this is a few MB at worst.
	maxSessions = 5000
)

// session is one search whose results can be paged without calling the API again.
type session struct {
	token   string
	owner   int64
	chat    int64
	memes   []memesearch.Meme
	created time.Time
}

// pageStore keeps sessions in memory; a restart loses them and the user gets
// search.expired, which is acceptable for a 1h paging window.
type pageStore struct {
	mu   sync.Mutex
	ttl  time.Duration
	max  int
	byID map[string]*session
}

func newPageStore(ttl time.Duration, maxN int) *pageStore {
	return &pageStore{ttl: ttl, max: maxN, byID: map[string]*session{}}
}

// put stores s under a fresh token. Expired sessions are dropped first; if the
// store is still full the oldest goes, so a flood of searches cannot grow it.
func (p *pageStore) put(s *session, now time.Time) string {
	tok := newToken()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.byID) >= p.max {
		var oldest *session
		for k, v := range p.byID {
			if p.expired(v, now) {
				delete(p.byID, k)
				continue
			}
			if oldest == nil || v.created.Before(oldest.created) {
				oldest = v
			}
		}
		if len(p.byID) >= p.max && oldest != nil {
			delete(p.byID, oldest.token)
		}
	}
	s.token = tok
	p.byID[tok] = s
	return tok
}

// get returns the session or nil when it is unknown or expired.
func (p *pageStore) get(tok string, now time.Time) *session {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.byID[tok]
	if !ok {
		return nil
	}
	if p.expired(s, now) {
		delete(p.byID, tok)
		return nil
	}
	return s
}

func (p *pageStore) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.byID)
}

func (p *pageStore) expired(s *session, now time.Time) bool {
	return now.Sub(s.created) >= p.ttl
}

// newToken returns 12 base64url characters: 72 random bits keep tokens
// unguessable while callback data stays well under Telegram's 64-byte limit.
func newToken() string {
	var b [9]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on supported platforms
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func moreData(tok string, page int) string {
	return morePrefix + tok + ":" + strconv.Itoa(page)
}

// parseMore parses "srch:m:<token>:<page>".
func parseMore(data string) (tok string, page int, ok bool) {
	rest, ok := strings.CutPrefix(data, morePrefix)
	if !ok {
		return "", 0, false
	}
	tok, num, ok := strings.Cut(rest, ":")
	if !ok || tok == "" {
		return "", 0, false
	}
	page, err := strconv.Atoi(num)
	if err != nil || page < 1 {
		return "", 0, false
	}
	return tok, page, true
}

// pageCount is the number of pages n results fill.
func pageCount(n, per int) int {
	return (n + per - 1) / per
}

// pageSlice returns the memes of 1-based page, or nil when it is out of range.
func pageSlice(ms []memesearch.Meme, page, per int) []memesearch.Meme {
	if page < 1 || page > pageCount(len(ms), per) {
		return nil
	}
	start := (page - 1) * per
	return ms[start:min(start+per, len(ms))]
}

// searchQuery trims text and reports whether it is a query the API accepts.
func searchQuery(text string) (string, bool) {
	q := strings.TrimSpace(text)
	return q, q != "" && utf8.RuneCountInString(q) <= memesearch.MaxQueryLen
}
