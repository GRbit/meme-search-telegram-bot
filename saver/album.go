package saver

import (
	"context"
	"sync"
	"time"

	"github.com/go-telegram/bot"
)

type albumKey struct {
	chatID int64
	group  string
}

type albumState struct {
	b       *bot.Bot
	chatID  int64
	ctx     context.Context
	lang    string
	replyTo int

	pending, saved, already, duplicate, failed int

	// gen invalidates timers armed before a later item arrived.
	gen int
}

func (s *albumState) total() int { return s.saved + s.already + s.duplicate + s.failed }

// albums collects per-item results so a Telegram album gets one reply, sent
// after the debounce passes with no item in flight.
type albums struct {
	mu       sync.Mutex
	items    map[albumKey]*albumState
	debounce time.Duration
	flush    func(albumState)
	// after is a seam for tests; production uses time.AfterFunc.
	after func(d time.Duration, f func())
}

func newAlbums(debounce time.Duration, flush func(albumState)) *albums {
	return &albums{
		items:    map[albumKey]*albumState{},
		debounce: debounce,
		flush:    flush,
		after:    func(d time.Duration, f func()) { time.AfterFunc(d, f) },
	}
}

// begin registers an item before its (slow) download starts, so the timer
// cannot fire while a sibling is still being saved.
func (a *albums) begin(key albumKey, init albumState) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.items[key]
	if st == nil {
		init.ctx = context.WithoutCancel(init.ctx)
		st = &init
		a.items[key] = st
	}
	st.pending++
	st.gen++
}

func (a *albums) finish(key albumKey, out outcome) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := a.items[key]
	switch out {
	case outSaved:
		st.saved++
	case outAlready:
		st.already++
	case outDuplicate:
		st.duplicate++
	default:
		st.failed++
	}
	st.pending--
	if st.pending > 0 {
		return
	}
	st.gen++
	gen := st.gen
	a.after(a.debounce, func() { a.fire(key, gen) })
}

func (a *albums) fire(key albumKey, gen int) {
	a.mu.Lock()
	st := a.items[key]
	if st == nil || st.gen != gen || st.pending > 0 {
		a.mu.Unlock()
		return
	}
	delete(a.items, key)
	a.mu.Unlock()
	a.flush(*st)
}
