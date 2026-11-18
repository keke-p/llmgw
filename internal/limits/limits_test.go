package limits

import (
	"context"
	"errors"
	"testing"
	"time"

	"llmgw/internal/clock"
)

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFIFOAndRelease(t *testing.T) {
	c := clock.NewFake()
	r := NewRegistry(c)
	ctx := context.Background()
	rel1, err := r.Acquire(ctx, "p", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	order := make(chan int, 3)
	rels := make(chan func(), 3)
	for i := 1; i <= 3; i++ {
		go func(i int) {
			rel, err := r.Acquire(ctx, "p", 1, 0)
			if err != nil {
				t.Error(err)
				return
			}
			order <- i
			rels <- rel
		}(i)
		waitFor(t, func() bool { _, w := r.Stats("p"); return w == i }) // enqueue in order
	}
	rel1()
	for want := 1; want <= 3; want++ {
		if got := <-order; got != want {
			t.Fatalf("FIFO violated: got %d want %d", got, want)
		}
		(<-rels)()
	}
	if inUse, waiting := r.Stats("p"); inUse != 0 || waiting != 0 {
		t.Errorf("stats = %d/%d", inUse, waiting)
	}
}

func TestQueueTimeout(t *testing.T) {
	c := clock.NewFake()
	r := NewRegistry(c)
	ctx := context.Background()
	rel, _ := r.Acquire(ctx, "k", 1, 0)
	done := make(chan error, 1)
	go func() {
		_, err := r.Acquire(ctx, "k", 1, 120*time.Second)
		done <- err
	}()
	waitFor(t, func() bool { return c.Active() == 1 })
	c.Advance(119 * time.Second)
	select {
	case <-done:
		t.Fatal("timed out too early")
	case <-time.After(10 * time.Millisecond):
	}
	c.Advance(time.Second)
	if err := <-done; !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("err = %v", err)
	}
	rel()
	if inUse, waiting := r.Stats("k"); inUse != 0 || waiting != 0 {
		t.Errorf("timed-out waiter must leave the queue: %d/%d", inUse, waiting)
	}
}

func TestCancelAndLimitChange(t *testing.T) {
	c := clock.NewFake()
	r := NewRegistry(c)
	rel, _ := r.Acquire(context.Background(), "p", 1, 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := r.Acquire(ctx, "p", 1, 0); done <- err }()
	waitFor(t, func() bool { _, w := r.Stats("p"); return w == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	// Raising the limit admits a new request immediately.
	rel2, err := r.Acquire(context.Background(), "p", 2, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	rel()
	rel2()
	rel2() // idempotent
	if inUse, _ := r.Stats("p"); inUse != 0 {
		t.Errorf("in use = %d", inUse)
	}
	if rel, err := r.Acquire(context.Background(), "x", 0, 0); err != nil || rel == nil {
		t.Error("limit 0 must be unlimited")
	}
}
