// Package limiter is the upload queue of the media cache: strict priorities,
// FIFO within a priority, and per-minute caps.
//
// A higher priority job always goes ahead of every lower priority job,
// whatever the arrival order, because a user waiting for an album must not sit
// behind hours of warmup. Rates are sliding one-minute windows, matching
// Telegram's "N messages per minute per chat" limit; a window allows a burst,
// which keeps a five-photo album fast.
package limiter

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Priority orders jobs; a lower value runs first.
type Priority int

// Priorities, highest first.
const (
	Realtime Priority = iota
	Background
	numPriorities
)

func (p Priority) String() string {
	if p == Realtime {
		return "realtime"
	}
	return "background"
}

// Clock is the time source; tests inject a fake one.
type Clock interface {
	Now() time.Time
	// After returns a channel that fires once d has passed, and a func that
	// releases the timer early.
	After(d time.Duration) (<-chan time.Time, func())
}

// Errors returned by Queue.
var (
	ErrClosed = errors.New("limiter: queue closed")
	ErrFull   = errors.New("limiter: queue full")
)

const window = time.Minute

// Config sets the rates and capacities; all values must be positive.
type Config struct {
	PerMin           int // all jobs
	BackgroundPerMin int // cap for background jobs, counted inside PerMin
	RealtimeCap      int
	BackgroundCap    int
}

type entry[T any] struct {
	key string
	v   T
}

// Queue is a priority FIFO queue with rate limits. Next is meant for a single
// consumer; Push and the rest are safe from any goroutine.
type Queue[T any] struct {
	cfg   Config
	clock Clock
	log   *slog.Logger

	mu          sync.Mutex
	items       [numPriorities][]entry[T]
	keys        map[string]struct{}
	starts      []time.Time // every job handed out within the window
	bgStarts    []time.Time
	pausedUntil time.Time
	closed      bool

	wake chan struct{}
	done chan struct{}
}

// New returns an empty queue.
func New[T any](cfg Config, clock Clock, log *slog.Logger) *Queue[T] {
	return &Queue[T]{
		cfg:   cfg,
		clock: clock,
		log:   log,
		keys:  map[string]struct{}{},
		wake:  make(chan struct{}, 1),
		done:  make(chan struct{}),
	}
}

// Push appends v to its priority. An empty key disables deduplication;
// otherwise a key already queued is not added again and Push returns false.
func (q *Queue[T]) Push(p Priority, key string, v T) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return false, ErrClosed
	}
	if key != "" {
		if _, dup := q.keys[key]; dup {
			q.log.Debug("upload queue: duplicate skipped", "key", key, "priority", p.String())
			return false, nil
		}
	}
	if len(q.items[p]) >= q.capacity(p) {
		q.log.Debug("upload queue: full", "key", key, "priority", p.String(), "len", len(q.items[p]))
		return false, ErrFull
	}
	q.add(p, entry[T]{key, v}, false)
	q.log.Debug("upload queue: pushed", "key", key, "priority", p.String(), "len", len(q.items[p]))
	return true, nil
}

// PushFront puts v at the head of its priority, ignoring capacity: it is for
// retrying a job that was already handed out, which must not be lost.
func (q *Queue[T]) PushFront(p Priority, key string, v T) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return ErrClosed
	}
	q.add(p, entry[T]{key, v}, true)
	q.log.Debug("upload queue: requeued at front", "key", key, "priority", p.String())
	return nil
}

func (q *Queue[T]) add(p Priority, e entry[T], front bool) {
	if e.key != "" {
		q.keys[e.key] = struct{}{}
	}
	if front {
		q.items[p] = append([]entry[T]{e}, q.items[p]...)
	} else {
		q.items[p] = append(q.items[p], e)
	}
	q.signal()
}

func (q *Queue[T]) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *Queue[T]) capacity(p Priority) int {
	if p == Realtime {
		return q.cfg.RealtimeCap
	}
	return q.cfg.BackgroundCap
}

// Next blocks until a job may run and returns it. It ends with ctx's error or
// ErrClosed.
func (q *Queue[T]) Next(ctx context.Context) (T, Priority, error) {
	var zero T
	for {
		v, p, wait, err := q.try()
		if err != nil {
			return zero, 0, err
		}
		if wait == 0 {
			return v, p, nil
		}
		var timer <-chan time.Time
		stop := func() {}
		if wait > 0 {
			timer, stop = q.clock.After(wait)
		}
		select {
		case <-timer:
		case <-q.wake:
		case <-q.done:
		case <-ctx.Done():
			stop()
			return zero, 0, ctx.Err()
		}
		stop()
	}
}

// try hands out a job if one may run now. Otherwise wait says how long until
// that can change (-1: only a Push can change it).
func (q *Queue[T]) try() (v T, p Priority, wait time.Duration, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return v, 0, 0, ErrClosed
	}
	now := q.clock.Now()
	if now.Before(q.pausedUntil) {
		return v, 0, q.pausedUntil.Sub(now), nil
	}
	q.starts = prune(q.starts, now)
	q.bgStarts = prune(q.bgStarts, now)
	totalWait := time.Duration(0)
	if len(q.starts) >= q.cfg.PerMin {
		totalWait = q.starts[0].Add(window).Sub(now)
	}
	switch {
	case len(q.items[Realtime]) > 0:
		if totalWait > 0 {
			q.log.Debug("upload queue: realtime waits for rate", "wait", totalWait)
			return v, 0, totalWait, nil
		}
		return q.pop(Realtime, now), Realtime, 0, nil
	case len(q.items[Background]) > 0:
		bgWait := time.Duration(0)
		if len(q.bgStarts) >= q.cfg.BackgroundPerMin {
			bgWait = q.bgStarts[0].Add(window).Sub(now)
		}
		if w := max(totalWait, bgWait); w > 0 {
			q.log.Debug("upload queue: background waits for rate", "wait", w)
			return v, 0, w, nil
		}
		return q.pop(Background, now), Background, 0, nil
	}
	return v, 0, -1, nil
}

func (q *Queue[T]) pop(p Priority, now time.Time) T {
	e := q.items[p][0]
	var zero entry[T]
	q.items[p][0] = zero // drop the reference so a big payload can be collected
	q.items[p] = q.items[p][1:]
	if e.key != "" {
		delete(q.keys, e.key)
	}
	q.starts = append(q.starts, now)
	if p == Background {
		q.bgStarts = append(q.bgStarts, now)
	}
	q.log.Debug("upload queue: job started", "key", e.key, "priority", p.String(),
		"left_realtime", len(q.items[Realtime]), "left_background", len(q.items[Background]))
	return e.v
}

func prune(ts []time.Time, now time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].Add(window).After(now) {
		i++
	}
	return ts[i:]
}

// Pause stops handing out jobs for d (Telegram's retry_after). Overlapping
// pauses keep the later end.
func (q *Queue[T]) Pause(d time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if until := q.clock.Now().Add(d); until.After(q.pausedUntil) {
		q.pausedUntil = until
	}
	q.log.Debug("upload queue: paused", "for", d)
	q.signal()
}

// Move puts the queued job with key at priority to, at its head or tail. It
// reports false when no job with key is queued at another priority or to is
// full; the job then stays where it was.
func (q *Queue[T]) Move(key string, to Priority, front bool) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if key == "" || q.closed {
		return false
	}
	if _, queued := q.keys[key]; !queued {
		return false
	}
	for p := range q.items {
		from := Priority(p)
		if from == to {
			continue
		}
		for i, e := range q.items[from] {
			if e.key != key {
				continue
			}
			if len(q.items[to]) >= q.capacity(to) {
				q.log.Debug("upload queue: move refused, target full", "key", key, "to", to.String())
				return false
			}
			q.items[from] = append(q.items[from][:i], q.items[from][i+1:]...)
			q.add(to, e, front)
			q.log.Debug("upload queue: moved", "key", key, "from", from.String(), "to", to.String(), "front", front)
			return true
		}
	}
	return false
}

// DropBackground removes every queued background job and returns how many.
func (q *Queue[T]) DropBackground() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.items[Background])
	for _, e := range q.items[Background] {
		delete(q.keys, e.key)
	}
	q.items[Background] = nil
	return n
}

// Close makes Next and Push fail with ErrClosed and returns the jobs that were
// still queued, highest priority first, so the caller can fail their waiters.
func (q *Queue[T]) Close() []T {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil
	}
	q.closed = true
	close(q.done)
	var left []T
	for p := range q.items {
		for _, e := range q.items[p] {
			left = append(left, e.v)
		}
		q.items[p] = nil
	}
	clear(q.keys)
	return left
}

// Len returns the number of queued jobs of priority p.
func (q *Queue[T]) Len(p Priority) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items[p])
}
