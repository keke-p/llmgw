// Package limits implements FIFO concurrency limits (per key and per
// provider) with a queue timeout.
package limits

import (
	"context"
	"errors"
	"sync"
	"time"

	"llmgw/internal/clock"
)

// ErrQueueTimeout is returned when no slot became free within the timeout.
var ErrQueueTimeout = errors.New("queue timeout")

type waiter struct {
	ch      chan struct{}
	granted bool
}

// Semaphore is a FIFO counting semaphore whose limit can change at runtime.
type Semaphore struct {
	mu      sync.Mutex
	limit   int
	inUse   int
	waiters []*waiter
}

// setLimit updates the limit and admits waiters if it grew. Caller holds mu.
func (s *Semaphore) setLimitLocked(n int) {
	s.limit = n
	for s.inUse < s.limit && len(s.waiters) > 0 {
		w := s.waiters[0]
		s.waiters = s.waiters[1:]
		w.granted = true
		s.inUse++
		close(w.ch)
	}
}

// Acquire waits (FIFO) for a slot. timeout <= 0 waits without limit.
func (s *Semaphore) Acquire(ctx context.Context, c clock.Clock, limit int, timeout time.Duration) error {
	s.mu.Lock()
	s.setLimitLocked(limit)
	if s.inUse < s.limit && len(s.waiters) == 0 {
		s.inUse++
		s.mu.Unlock()
		return nil
	}
	w := &waiter{ch: make(chan struct{})}
	s.waiters = append(s.waiters, w)
	s.mu.Unlock()

	timedOut := make(chan struct{})
	if timeout > 0 {
		t := c.AfterFunc(timeout, func() { close(timedOut) })
		defer t.Stop()
	}
	var err error
	select {
	case <-w.ch:
		return nil
	case <-timedOut:
		err = ErrQueueTimeout
	case <-ctx.Done():
		err = ctx.Err()
	}
	s.mu.Lock()
	if w.granted { // granted concurrently with the timeout/cancel: give it back
		s.mu.Unlock()
		s.Release()
		return err
	}
	for i, x := range s.waiters {
		if x == w {
			s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
			break
		}
	}
	s.mu.Unlock()
	return err
}

// Release frees a slot, handing it to the oldest waiter if possible.
func (s *Semaphore) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inUse--
	s.setLimitLocked(s.limit)
}

// Stats returns (in use, waiting).
func (s *Semaphore) Stats() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inUse, len(s.waiters)
}

// Registry holds named semaphores ("key:3", "provider:1").
type Registry struct {
	mu    sync.Mutex
	sems  map[string]*Semaphore
	Clock clock.Clock
}

// NewRegistry returns an empty registry.
func NewRegistry(c clock.Clock) *Registry {
	return &Registry{sems: map[string]*Semaphore{}, Clock: c}
}

func (r *Registry) get(name string) *Semaphore {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sems[name]
	if s == nil {
		s = &Semaphore{}
		r.sems[name] = s
	}
	return s
}

// Acquire takes a slot of the named semaphore with the current limit. A limit
// <= 0 means unlimited (no slot is taken). The returned release function must
// be called exactly once.
func (r *Registry) Acquire(ctx context.Context, name string, limit int, timeout time.Duration) (func(), error) {
	if limit <= 0 {
		return func() {}, nil
	}
	s := r.get(name)
	if err := s.Acquire(ctx, r.Clock, limit, timeout); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(s.Release) }, nil
}

// Stats returns (in use, waiting) for a named semaphore.
func (r *Registry) Stats(name string) (int, int) {
	r.mu.Lock()
	s := r.sems[name]
	r.mu.Unlock()
	if s == nil {
		return 0, 0
	}
	return s.Stats()
}
