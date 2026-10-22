package memesearch

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grbit/meme-search-telegram-bot/internal/fakeclock"
)

// watch runs WatchCapabilities on a fake clock and records every delay it
// sleeps for.
type watch struct {
	clk    *fakeclock.Clock
	mu     sync.Mutex
	delays []time.Duration
}

func startWatch(t *testing.T, c *HTTPClient, interval time.Duration) *watch {
	t.Helper()
	w := &watch{clk: fakeclock.New(time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))}
	c.now = w.clk.Now
	c.after = func(d time.Duration) (<-chan time.Time, func()) {
		w.mu.Lock()
		w.delays = append(w.delays, d)
		w.mu.Unlock()
		return w.clk.After(d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.WatchCapabilities(ctx, interval)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return w
}

// sleeping waits until the watcher sleeps for the n-th time and returns that delay.
func (w *watch) sleeping(t *testing.T, n int) time.Duration {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		w.mu.Lock()
		got := len(w.delays)
		var d time.Duration
		if got >= n {
			d = w.delays[n-1]
		}
		w.mu.Unlock()
		if got >= n && w.clk.Waiters() == 1 {
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("watcher did not sleep a %d. time (slept %d times)", n, got)
		}
		time.Sleep(time.Millisecond)
	}
}

// A bot started next to a still booting meme-search must not wait the full
// interval (6h in production) to see the server's features.
func TestWatchRetriesSoonWhenTheServerIsDown(t *testing.T) {
	f := newFakeAPI(t)
	f.down.Store(true)
	c := f.client(t)
	w := startWatch(t, c, 6*time.Hour)
	if d := w.sleeping(t, 1); d != time.Minute {
		t.Fatalf("first retry after %s, want 1m", d)
	}
	if c.Capabilities().FolderList {
		t.Fatal("capability on while the server is down")
	}
	f.down.Store(false)
	w.clk.Advance(time.Minute)
	if d := w.sleeping(t, 2); d != 6*time.Hour {
		t.Fatalf("after a decided probe the next one is in %s, want the interval", d)
	}
	if got := c.Capabilities(); !got.FolderList || !got.Listing || !got.FolderFilter {
		t.Fatalf("capabilities after the server came up = %+v", got)
	}
}

func TestWatchBackoffDoublesUpToTheInterval(t *testing.T) {
	f := newFakeAPI(t)
	f.down.Store(true)
	c := f.client(t)
	w := startWatch(t, c, 10*time.Minute)
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	for i, d := range want {
		if got := w.sleeping(t, i+1); got != d {
			t.Fatalf("delay %d = %s, want %s", i+1, got, d)
		}
		w.clk.Advance(d)
	}
	// Up again: back to the normal schedule, and a later outage starts the backoff anew.
	f.down.Store(false)
	if got := w.sleeping(t, len(want)+1); got != 10*time.Minute {
		t.Fatalf("delay after recovery = %s", got)
	}
	f.down.Store(true)
	w.clk.Advance(10 * time.Minute)
	if got := w.sleeping(t, len(want)+2); got != time.Minute {
		t.Fatalf("delay after a new outage = %s, want 1m", got)
	}
}

func TestUndecidedProbeWarnsInsteadOfSummary(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	f := newFakeAPI(t)
	f.down.Store(true)
	c, err := New(Config{URL: f.srv.URL, Token: testToken, Timeout: 5 * time.Second}, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	c.Probe(t.Context())
	out := buf.String()
	if strings.Contains(out, `msg="memesearch capabilities"`) {
		t.Fatalf("summary logged for an undecided probe:\n%s", out)
	}
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "could not be checked") {
		t.Fatalf("no warning about the unchecked server:\n%s", out)
	}

	buf.Reset()
	f.down.Store(false)
	c.Probe(t.Context())
	c.Probe(t.Context())
	if n := strings.Count(buf.String(), `msg="memesearch capabilities"`); n != 1 {
		t.Fatalf("summaries = %d, want 1 on the first decided probe:\n%s", n, buf.String())
	}
}
