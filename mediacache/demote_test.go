package mediacache

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/grbit/meme-search-telegram-bot/mediacache/internal/limiter"
	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

const (
	alice = int64(1)
	bob   = int64(2)
)

func memesByID(ids ...int64) []memesearch.Meme {
	ms := make([]memesearch.Meme, len(ids))
	for i, id := range ids {
		ms[i] = memesearch.Meme{ID: id}
	}
	return ms
}

// newSlowEnv uploads one photo per minute, so after the first upload every
// further job sits in the queue until the test advances the clock.
func newSlowEnv(t *testing.T, ids ...int64) *env {
	e := newEnv(t, Config{CacheChatID: chatID, UploadsPerMin: 1})
	for _, id := range ids {
		e.api.content[id] = []byte{byte(id)}
	}
	e.run()
	return e
}

func (e *env) waitQueued(rt, bg int) {
	e.t.Helper()
	eventually(e.t, "queue lengths", func() bool {
		return e.c.queue.Len(limiter.Realtime) == rt && e.c.queue.Len(limiter.Background) == bg
	})
}

func (e *env) inFlight() int {
	e.c.mu.Lock()
	defer e.c.mu.Unlock()
	return e.c.enqueued
}

// drain advances the clock a minute per upload until want uploads happened and
// returns the captions in upload order.
func (e *env) drain(want int) []string {
	e.t.Helper()
	for len(e.sender.sent()) < want {
		n := len(e.sender.sent())
		if !e.clk.WaitForWaiters(1, 5*time.Second) {
			e.t.Fatal("queue not waiting for the rate limit")
		}
		e.clk.Advance(time.Minute)
		eventually(e.t, "next upload", func() bool { return len(e.sender.sent()) > n })
	}
	var caps []string
	for _, s := range e.sender.sent() {
		caps = append(caps, s.caption)
	}
	return caps
}

// Alice types "c" (meme 2, 3), then "cat" (meme 4) while bob's current query
// also wants meme 3: only meme 2 is left with nobody waiting for it.
func TestNewInlineQueryDemotesUsersOlderJobs(t *testing.T) {
	e := newSlowEnv(t, 1, 2, 3, 4)
	e.c.EnqueueFor(alice, memesByID(1)...)
	eventually(t, "first upload", func() bool { return len(e.sender.sent()) == 1 })
	e.c.EnqueueFor(alice, memesByID(2, 3)...)
	e.waitQueued(2, 0)
	e.c.EnqueueFor(bob, memesByID(3)...)
	e.c.EnqueueFor(alice, memesByID(4)...)
	e.waitQueued(2, 1)
	// The demoted job gives back its in-flight slot, or demoted jobs would
	// fill the bound and new queries would be dropped.
	eventually(t, "in-flight count", func() bool { return e.inFlight() == 2 })

	want := []string{"meme 1", "meme 3", "meme 4", "meme 2"}
	if got := e.drain(4); !slices.Equal(got, want) {
		t.Fatalf("upload order = %q, want %q", got, want)
	}
	eventually(t, "per-user state cleared", func() bool {
		e.c.mu.Lock()
		defer e.c.mu.Unlock()
		return len(e.c.current) == 0 && len(e.c.calls) == 0
	})
}

func TestDemotedJobIsPromotedWhenWantedAgain(t *testing.T) {
	e := newSlowEnv(t, 1, 2, 3)
	e.c.EnqueueFor(alice, memesByID(1)...)
	eventually(t, "first upload", func() bool { return len(e.sender.sent()) == 1 })
	e.c.EnqueueFor(alice, memesByID(2)...)
	e.waitQueued(1, 0)
	e.c.EnqueueFor(alice, memesByID(3)...)
	e.waitQueued(1, 1)
	e.c.EnqueueFor(bob, memesByID(2)...)
	e.waitQueued(2, 0)
	eventually(t, "in-flight count", func() bool { return e.inFlight() == 2 })
	if got, want := e.drain(3), []string{"meme 1", "meme 3", "meme 2"}; !slices.Equal(got, want) {
		t.Fatalf("upload order = %q, want %q", got, want)
	}
}

// Someone blocked in Resolve is waiting for the upload right now.
func TestResolveWaiterKeepsJobRealtime(t *testing.T) {
	e := newSlowEnv(t, 1, 2, 3)
	e.c.EnqueueFor(alice, memesByID(1)...)
	eventually(t, "first upload", func() bool { return len(e.sender.sent()) == 1 })
	e.c.EnqueueFor(alice, memesByID(2)...)
	e.waitQueued(1, 0)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _, _ = e.c.Resolve(ctx, memesearch.Meme{ID: 2}) }()
	eventually(t, "Resolve joined", func() bool {
		e.c.mu.Lock()
		defer e.c.mu.Unlock()
		cl := e.c.calls[2]
		return cl != nil && cl.pinned
	})
	e.c.EnqueueFor(alice, memesByID(3)...)
	e.waitQueued(2, 0)
}

// The second query can arrive before the first query's job reached the queue;
// the job must still land at background priority.
func TestDemotionBeforeJobIsQueued(t *testing.T) {
	e := newSlowEnv(t, 1, 2, 3)
	e.c.EnqueueFor(alice, memesByID(1)...)
	eventually(t, "first upload", func() bool { return len(e.sender.sent()) == 1 })
	e.c.EnqueueFor(alice, memesByID(2)...)
	e.c.EnqueueFor(alice, memesByID(3)...)
	e.waitQueued(1, 1)
	if got, want := e.drain(3), []string{"meme 1", "meme 3", "meme 2"}; !slices.Equal(got, want) {
		t.Fatalf("upload order = %q, want %q", got, want)
	}
}
