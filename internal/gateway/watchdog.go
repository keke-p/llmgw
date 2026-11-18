package gateway

import (
	"context"
	"errors"
	"sync"
	"time"

	"llmgw/internal/clock"
	"llmgw/internal/gwerr"
)

// timeoutCause is the context cancellation cause used when a phase timer fires.
type timeoutCause struct {
	Phase string
	Limit time.Duration
}

func (t *timeoutCause) Error() string { return "timeout: " + t.Phase }

// errClientGone is the cancellation cause when writing to the client fails.
var errClientGone = errors.New("client disconnected")

// watchdog manages the phase timers of one attempt. When a timer fires the
// attempt context is cancelled with a *timeoutCause, which closes the upstream
// connection.
type watchdog struct {
	mu      sync.Mutex
	clock   clock.Clock
	cancel  context.CancelCauseFunc
	timers  map[string]clock.Timer
	sentAt  time.Time
	stopped bool
}

func newWatchdog(c clock.Clock, cancel context.CancelCauseFunc) *watchdog {
	return &watchdog{clock: c, cancel: cancel, timers: map[string]clock.Timer{}}
}

// arm (re)starts the timer for phase. d <= 0 disarms it (unlimited).
func (w *watchdog) arm(phase string, d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.armLocked(phase, d, d)
}

// armLimit is like arm but reports limit (instead of d) in the error message;
// used for max_duration where d is the remaining time.
func (w *watchdog) armLimit(phase string, d, limit time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.armLocked(phase, d, limit)
}

func (w *watchdog) armLocked(phase string, d, limit time.Duration) {
	if w.stopped {
		return
	}
	if t := w.timers[phase]; t != nil {
		t.Stop()
		delete(w.timers, phase)
	}
	if d <= 0 {
		return
	}
	cause := &timeoutCause{Phase: phase, Limit: limit}
	w.timers[phase] = w.clock.AfterFunc(d, func() { w.cancel(cause) })
}

func (w *watchdog) disarm(phase string) { w.arm(phase, 0) }

// sent records that the request was fully written to upstream: the connect
// timer stops and the TTFT timer starts. Only the first call counts.
func (w *watchdog) sent(ttft time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.sentAt.IsZero() || w.stopped {
		return
	}
	w.sentAt = w.clock.Now()
	w.armLocked(gwerr.PhaseConnect, 0, 0)
	w.armLocked(gwerr.PhaseTTFT, ttft, ttft)
}

func (w *watchdog) sentTime() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sentAt
}

func (w *watchdog) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	for k, t := range w.timers {
		t.Stop()
		delete(w.timers, k)
	}
}
