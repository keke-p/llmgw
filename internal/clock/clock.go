// Package clock abstracts time so that timeout logic can be tested without
// waiting in real time.
package clock

import (
	"sort"
	"sync"
	"time"
)

// Timer is the subset of *time.Timer used by llmgw.
type Timer interface {
	Stop() bool
	Reset(d time.Duration) bool
}

// Clock provides the current time and callback timers.
type Clock interface {
	Now() time.Time
	// AfterFunc calls f in its own goroutine (real clock) or synchronously from
	// Advance (fake clock) once d has elapsed.
	AfterFunc(d time.Duration, f func()) Timer
}

// Real is the wall clock.
type Real struct{}

func (Real) Now() time.Time { return time.Now() }

func (Real) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// Fake is a manually advanced clock for tests.
type Fake struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
	seq    int
}

// NewFake returns a fake clock starting at a fixed instant.
func NewFake() *Fake {
	return &Fake{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

type fakeTimer struct {
	c      *Fake
	at     time.Time
	seq    int
	f      func()
	active bool
}

func (c *Fake) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Fake) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	t := &fakeTimer{c: c, at: c.now.Add(d), seq: c.seq, f: f, active: true}
	c.timers = append(c.timers, t)
	return t
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.active = false
	return was
}

func (t *fakeTimer) Reset(d time.Duration) bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.active = true
	t.at = t.c.now.Add(d)
	t.c.seq++
	t.seq = t.c.seq
	return was
}

// Advance moves the clock forward by d, firing due timers in order.
func (c *Fake) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	for {
		var due []*fakeTimer
		for _, t := range c.timers {
			if t.active && !t.at.After(target) {
				due = append(due, t)
			}
		}
		if len(due) == 0 {
			break
		}
		sort.Slice(due, func(i, j int) bool {
			if due[i].at.Equal(due[j].at) {
				return due[i].seq < due[j].seq
			}
			return due[i].at.Before(due[j].at)
		})
		t := due[0]
		t.active = false
		if t.at.After(c.now) {
			c.now = t.at
		}
		c.mu.Unlock()
		t.f()
		c.mu.Lock()
	}
	c.now = target
	c.mu.Unlock()
}

// Active returns the number of armed timers.
func (c *Fake) Active() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, t := range c.timers {
		if t.active {
			n++
		}
	}
	return n
}
