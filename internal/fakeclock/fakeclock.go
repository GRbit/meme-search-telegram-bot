// Package fakeclock is a manually advanced clock for tests.
//
// It lives outside _test files because several packages (mediacache, its
// limiter, search) drive time through it.
package fakeclock

import (
	"sync"
	"time"
)

// Clock is a clock whose time moves only on Advance.
type Clock struct {
	mu      sync.Mutex
	now     time.Time
	waiters map[*waiter]struct{}
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

// New returns a clock set to start.
func New(start time.Time) *Clock {
	return &Clock{now: start, waiters: map[*waiter]struct{}{}}
}

// Now returns the current fake time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After returns a channel that receives once the clock reaches now+d, and a
// stop func that forgets the waiter.
func (c *Clock) After(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := &waiter{at: c.now.Add(d), ch: make(chan time.Time, 1)}
	if d <= 0 {
		w.ch <- c.now
		return w.ch, func() {}
	}
	c.waiters[w] = struct{}{}
	return w.ch, func() {
		c.mu.Lock()
		delete(c.waiters, w)
		c.mu.Unlock()
	}
}

// Advance moves the clock forward and fires every waiter that is due.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for w := range c.waiters {
		if !w.at.After(c.now) {
			w.ch <- c.now
			delete(c.waiters, w)
		}
	}
}

// Waiters returns how many pending After calls exist.
func (c *Clock) Waiters() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// WaitForWaiters polls until at least n waiters are pending or timeout passes,
// so a test can advance time only after the code under test went to sleep.
func (c *Clock) WaitForWaiters(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.Waiters() >= n {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return c.Waiters() >= n
}
