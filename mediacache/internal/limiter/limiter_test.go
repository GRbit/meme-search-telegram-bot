package limiter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/grbit/meme-search-telegram-bot/internal/fakeclock"
)

type got struct {
	v   string
	p   Priority
	err error
}

func newQueue(t *testing.T, cfg Config) (*Queue[string], *fakeclock.Clock) {
	t.Helper()
	if cfg.RealtimeCap == 0 {
		cfg.RealtimeCap = 100
	}
	if cfg.BackgroundCap == 0 {
		cfg.BackgroundCap = 100
	}
	clk := fakeclock.New(time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC))
	return New[string](cfg, clk, slog.New(slog.DiscardHandler)), clk
}

func mustPush(t *testing.T, q *Queue[string], p Priority, v string) {
	t.Helper()
	if ok, err := q.Push(p, v, v); err != nil || !ok {
		t.Fatalf("Push(%q) = %v, %v", v, ok, err)
	}
}

// nextNow calls Next and fails unless it returns without time passing.
func nextNow(t *testing.T, q *Queue[string]) got {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	v, p, err := q.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return got{v, p, err}
}

// nextAsync starts Next in a goroutine and waits until it sleeps on the clock,
// so a missing result proves Next is blocked, not just slow.
func nextAsync(t *testing.T, ctx context.Context, q *Queue[string], clk *fakeclock.Clock, waiters int) <-chan got {
	t.Helper()
	ch := make(chan got, 1)
	go func() {
		v, p, err := q.Next(ctx)
		ch <- got{v, p, err}
	}()
	if waiters > 0 && !clk.WaitForWaiters(waiters, 2*time.Second) {
		t.Fatal("Next did not go to sleep")
	}
	return ch
}

func blocked(t *testing.T, ch <-chan got) {
	t.Helper()
	select {
	case g := <-ch:
		t.Fatalf("Next returned %+v, want it blocked", g)
	case <-time.After(5 * time.Millisecond):
	}
}

func receive(t *testing.T, ch <-chan got) got {
	t.Helper()
	select {
	case g := <-ch:
		return g
	case <-time.After(2 * time.Second):
		t.Fatal("Next did not return")
		return got{}
	}
}

func TestRealtimeJumpsAheadOfBackground(t *testing.T) {
	q, _ := newQueue(t, Config{PerMin: 100, BackgroundPerMin: 100})
	for i := range 10 {
		mustPush(t, q, Background, fmt.Sprintf("bg%d", i))
	}
	mustPush(t, q, Realtime, "rt")
	if g := nextNow(t, q); g.v != "rt" || g.p != Realtime {
		t.Fatalf("first = %+v, want rt", g)
	}
	for i := range 10 {
		if g := nextNow(t, q); g.v != fmt.Sprintf("bg%d", i) || g.p != Background {
			t.Fatalf("job %d = %+v", i, g)
		}
	}
}

func TestFIFOWithinPriority(t *testing.T) {
	q, _ := newQueue(t, Config{PerMin: 100, BackgroundPerMin: 100})
	order := []struct {
		p Priority
		v string
	}{{Background, "b1"}, {Realtime, "r1"}, {Background, "b2"}, {Realtime, "r2"}, {Realtime, "r3"}, {Background, "b3"}}
	for _, o := range order {
		mustPush(t, q, o.p, o.v)
	}
	for _, want := range []string{"r1", "r2", "r3", "b1", "b2", "b3"} {
		if g := nextNow(t, q); g.v != want {
			t.Fatalf("got %q, want %q", g.v, want)
		}
	}
}

func TestTotalRateCap(t *testing.T) {
	q, clk := newQueue(t, Config{PerMin: 3, BackgroundPerMin: 3})
	for i := range 4 {
		mustPush(t, q, Realtime, fmt.Sprintf("r%d", i))
	}
	for range 3 {
		nextNow(t, q)
	}
	ch := nextAsync(t, t.Context(), q, clk, 1)
	clk.Advance(59 * time.Second)
	if !clk.WaitForWaiters(1, 2*time.Second) {
		t.Fatal("Next woke early and did not sleep again")
	}
	blocked(t, ch)
	clk.Advance(time.Second)
	if g := receive(t, ch); g.v != "r3" {
		t.Fatalf("got %+v, want r3", g)
	}
}

func TestBackgroundCapDoesNotHoldRealtime(t *testing.T) {
	q, clk := newQueue(t, Config{PerMin: 5, BackgroundPerMin: 2})
	for i := range 3 {
		mustPush(t, q, Background, fmt.Sprintf("b%d", i))
	}
	nextNow(t, q)
	nextNow(t, q)
	ch := nextAsync(t, t.Context(), q, clk, 1)
	blocked(t, ch)
	mustPush(t, q, Realtime, "rt")
	if g := receive(t, ch); g.v != "rt" {
		t.Fatalf("got %+v, want rt while background is capped", g)
	}
	ch = nextAsync(t, t.Context(), q, clk, 1)
	blocked(t, ch)
	clk.Advance(time.Minute)
	if g := receive(t, ch); g.v != "b2" {
		t.Fatalf("got %+v, want b2 after the window", g)
	}
}

func TestBackgroundWaitsWhileRealtimeWaits(t *testing.T) {
	q, clk := newQueue(t, Config{PerMin: 1, BackgroundPerMin: 1})
	mustPush(t, q, Background, "b1")
	nextNow(t, q)
	mustPush(t, q, Background, "b2")
	mustPush(t, q, Realtime, "rt")
	ch := nextAsync(t, t.Context(), q, clk, 1)
	clk.Advance(time.Minute)
	if g := receive(t, ch); g.v != "rt" {
		t.Fatalf("got %+v, want rt", g)
	}
}

func TestPause(t *testing.T) {
	q, clk := newQueue(t, Config{PerMin: 10, BackgroundPerMin: 10})
	mustPush(t, q, Realtime, "rt")
	q.Pause(5 * time.Second)
	ch := nextAsync(t, t.Context(), q, clk, 1)
	clk.Advance(4 * time.Second)
	if !clk.WaitForWaiters(1, 2*time.Second) {
		t.Fatal("Next woke early and did not sleep again")
	}
	blocked(t, ch)
	clk.Advance(time.Second)
	if g := receive(t, ch); g.v != "rt" {
		t.Fatalf("got %+v", g)
	}
}

func TestPushFrontRunsFirstInItsPriority(t *testing.T) {
	q, _ := newQueue(t, Config{PerMin: 10, BackgroundPerMin: 10})
	mustPush(t, q, Realtime, "r1")
	if err := q.PushFront(Realtime, "retry", "retry"); err != nil {
		t.Fatal(err)
	}
	if g := nextNow(t, q); g.v != "retry" {
		t.Fatalf("got %+v", g)
	}
}

func TestDedupAndCapacity(t *testing.T) {
	q, _ := newQueue(t, Config{PerMin: 10, BackgroundPerMin: 10, BackgroundCap: 2})
	mustPush(t, q, Background, "a")
	if ok, err := q.Push(Background, "a", "a"); ok || err != nil {
		t.Fatalf("duplicate Push = %v, %v; want false, nil", ok, err)
	}
	mustPush(t, q, Background, "b")
	if _, err := q.Push(Background, "c", "c"); !errors.Is(err, ErrFull) {
		t.Fatalf("Push over capacity = %v, want ErrFull", err)
	}
	if g := nextNow(t, q); g.v != "a" {
		t.Fatal(g)
	}
	// Once handed out, the key may be queued again.
	mustPush(t, q, Background, "a")
	if n := q.DropBackground(); n != 2 {
		t.Fatalf("DropBackground = %d, want 2", n)
	}
	if q.Len(Background) != 0 {
		t.Fatal("background not empty")
	}
}

func TestCtxCancelUnblocksNext(t *testing.T) {
	q, clk := newQueue(t, Config{PerMin: 10, BackgroundPerMin: 10})
	ctx, cancel := context.WithCancel(t.Context())
	ch := nextAsync(t, ctx, q, clk, 0)
	blocked(t, ch)
	cancel()
	if g := receive(t, ch); !errors.Is(g.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", g.err)
	}
}

func TestCloseReturnsQueuedAndUnblocks(t *testing.T) {
	q, clk := newQueue(t, Config{PerMin: 1, BackgroundPerMin: 1})
	mustPush(t, q, Realtime, "r1")
	nextNow(t, q)
	mustPush(t, q, Realtime, "r2")
	mustPush(t, q, Background, "b1")
	ch := nextAsync(t, t.Context(), q, clk, 1)
	left := q.Close()
	if g := receive(t, ch); !errors.Is(g.err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", g.err)
	}
	if len(left) != 2 || left[0] != "r2" || left[1] != "b1" {
		t.Fatalf("Close returned %v", left)
	}
	if _, err := q.Push(Realtime, "x", "x"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Push after Close = %v", err)
	}
}
