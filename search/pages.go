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
	similarPrefix = CallbackPrefix + "s:"
	// everywherePrefix reruns a session's query without tags and folders.
	everywherePrefix = CallbackPrefix + "e:"

	// maxStoredMemes is the default of Config.PageMemes, small enough for a
	// small VPS: a meme with its description is roughly 0.5-1 KB, so about
	// 10-20 MB at worst. An evicted session's "More" answers "search again".
	maxStoredMemes = 20_000
)

// session is one search whose results can be paged without calling the API
// again, until the fetched memes run out and the server offers more.
type session struct {
	token   string
	owner   int64
	chat    int64
	created time.Time
	// query and scope are kept for what reruns the search: "Search
	// everywhere", "Similar" and fetching more.
	query string
	scope scope
	// exclude is never shown, nor its copies (the source of "Similar"), or nil.
	exclude *memesearch.Meme

	// mu guards memes and nextOffset: "More" appends while other presses read.
	mu    sync.Mutex
	memes []memesearch.Meme
	// nextOffset is the server's meta.next_offset; 0 means no more to fetch
	// (last page, offset 200, an older server, or similar by id).
	nextOffset int

	// used and counted belong to pageStore and are guarded by its mutex.
	used    time.Time
	counted int
}

// snapshot returns the memes and next offset. Appends never change elements
// already in the slice, so the returned slice stays valid without the lock.
func (s *session) snapshot() ([]memesearch.Meme, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.memes, s.nextOffset
}

// pageStore keeps sessions in memory; a restart loses them and the user gets
// search.expired, which is acceptable for a 1h paging window.
//
// One bound: the total of stored memes, since offset paging lets one session
// hold 220 memes with their descriptions. Over it, the least recently used
// session goes first. A session without memes (an empty result kept for
// "Search everywhere") counts as one, so those cannot pile up either.
type pageStore struct {
	mu       sync.Mutex
	ttl      time.Duration
	maxMemes int
	total    int // memes counted over all sessions
	byID     map[string]*session
}

func newPageStore(ttl time.Duration, maxMemes int) *pageStore {
	return &pageStore{ttl: ttl, maxMemes: maxMemes, byID: map[string]*session{}}
}

// put stores s under a fresh token. Expired sessions are dropped first, then
// least recently used ones while the budget is exceeded.
func (p *pageStore) put(s *session, now time.Time) string {
	tok := newToken()
	n := max(len(s.memes), 1) // s is not shared yet
	p.mu.Lock()
	defer p.mu.Unlock()
	s.token = tok
	s.used = s.created
	s.counted = n
	if p.total+n > p.maxMemes {
		for k, v := range p.byID {
			if p.expired(v, now) {
				p.remove(k)
			}
		}
	}
	p.byID[tok] = s
	p.total += n
	p.trim(s)
	return tok
}

// grow records n memes appended to s and evicts other sessions over the budget.
func (p *pageStore) grow(s *session, n int, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.byID[s.token]; !ok {
		return // evicted or expired meanwhile: nothing is counted for it
	}
	s.counted += n
	s.used = now
	p.total += n
	p.trim(s)
}

// trim evicts least recently used sessions other than keep until the meme
// budget holds; keep alone may exceed it (at most 220 memes).
func (p *pageStore) trim(keep *session) {
	for p.total > p.maxMemes && p.evictLRU(keep) {
	}
}

func (p *pageStore) evictLRU(keep *session) bool {
	var lru *session
	for _, v := range p.byID {
		if v != keep && (lru == nil || v.used.Before(lru.used)) {
			lru = v
		}
	}
	if lru == nil {
		return false
	}
	p.remove(lru.token)
	return true
}

func (p *pageStore) remove(tok string) {
	if s, ok := p.byID[tok]; ok {
		p.total -= s.counted
		delete(p.byID, tok)
	}
}

// get returns the session or nil when it is unknown or expired, and marks it used.
func (p *pageStore) get(tok string, now time.Time) *session {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.byID[tok]
	if !ok {
		return nil
	}
	if p.expired(s, now) {
		p.remove(tok)
		return nil
	}
	s.used = now
	return s
}

// memes returns the number of memes counted over all sessions.
func (p *pageStore) memes() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.total
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

// similarData points at one meme of a session by its index in the results.
func similarData(tok string, idx int) string {
	return similarPrefix + tok + ":" + strconv.Itoa(idx)
}

// parseSimilar parses "srch:s:<token>:<index>".
func parseSimilar(data string) (tok string, idx int, ok bool) {
	rest, ok := strings.CutPrefix(data, similarPrefix)
	if !ok {
		return "", 0, false
	}
	tok, num, ok := strings.Cut(rest, ":")
	if !ok || tok == "" {
		return "", 0, false
	}
	idx, err := strconv.Atoi(num)
	if err != nil || idx < 0 {
		return "", 0, false
	}
	return tok, idx, true
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

func snapshotMemes(s *session) []memesearch.Meme {
	ms, _ := s.snapshot()
	return ms
}
