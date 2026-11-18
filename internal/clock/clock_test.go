package clock

import (
	"testing"
	"time"
)

func TestFakeAdvance(t *testing.T) {
	c := NewFake()
	var fired []string
	c.AfterFunc(3*time.Second, func() { fired = append(fired, "b") })
	c.AfterFunc(1*time.Second, func() { fired = append(fired, "a") })
	stopped := c.AfterFunc(2*time.Second, func() { fired = append(fired, "x") })
	stopped.Stop()

	c.Advance(2 * time.Second)
	if len(fired) != 1 || fired[0] != "a" {
		t.Fatalf("fired = %v", fired)
	}
	c.Advance(time.Second)
	if len(fired) != 2 || fired[1] != "b" {
		t.Fatalf("fired = %v", fired)
	}
	if c.Active() != 0 {
		t.Errorf("active = %d", c.Active())
	}
}

func TestFakeResetAfterFire(t *testing.T) {
	c := NewFake()
	n := 0
	var tm Timer
	tm = c.AfterFunc(time.Second, func() { n++ })
	c.Advance(time.Second)
	tm.Reset(time.Second)
	c.Advance(time.Second)
	if n != 2 {
		t.Fatalf("n = %d", n)
	}
	// Re-arming from inside the callback (like a ticker) must work.
	ticks := 0
	var tk Timer
	tk = c.AfterFunc(time.Second, func() { ticks++; tk.Reset(time.Second) })
	c.Advance(5 * time.Second)
	if ticks != 5 {
		t.Fatalf("ticks = %d", ticks)
	}
}
