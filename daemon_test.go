package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestRunLoop_FiresImmediatelyThenCancels verifies the loop calls fn once at
// startup (so a fresh daemon reports immediately) and stops cleanly on cancel.
func TestRunLoop_FiresImmediatelyThenCancels(t *testing.T) {
	var calls int32
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		runLoop(ctx, "test", time.Hour, func() {
			atomic.AddInt32(&calls, 1)
		})
		close(done)
	}()

	// Give the immediate fire a moment to land.
	if !waitFor(func() bool { return atomic.LoadInt32(&calls) >= 1 }, time.Second) {
		t.Fatal("loop did not fire immediately at startup")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop did not stop after cancel")
	}

	// No more calls should arrive after cancellation.
	immediate := atomic.LoadInt32(&calls)
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != immediate {
		t.Errorf("calls increased after cancel: %d -> %d", immediate, got)
	}
}

// TestRunLoop_TicksPeriodically verifies the interval timer keeps firing.
func TestRunLoop_TicksPeriodically(t *testing.T) {
	var calls int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go runLoop(ctx, "test", 20*time.Millisecond, func() {
		atomic.AddInt32(&calls, 1)
	})

	// Initial fire + at least 2 ticks within 200ms.
	if !waitFor(func() bool { return atomic.LoadInt32(&calls) >= 3 }, time.Second) {
		t.Fatalf("only %d calls after 1s, want >=3", atomic.LoadInt32(&calls))
	}
}

func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}
