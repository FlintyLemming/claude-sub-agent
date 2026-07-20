package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// daemonConfig wires the running daemon. Constructed by main from the resolved
// flags/env; sane defaults already applied there.
type daemonConfig struct {
	collectInterval time.Duration
	collector       *Collector
	pusher          *Pusher
}

// runDaemon runs the collect loop until it receives SIGINT/SIGTERM.
func runDaemon(cfg daemonConfig) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	done := make(chan struct{})
	go func() {
		collectLoop(ctx, cfg)
		close(done)
	}()

	log.Printf("daemon: started (collect every %s)", cfg.collectInterval)

	<-done
	log.Printf("daemon: stopped")
}

// maxBackoff caps how long a 429 can push the next collect attempt out.
const maxBackoff = 30 * time.Minute

// rateLimitDelay picks the wait after a 429. The server's Retry-After wins,
// clamped between the collect interval and maxBackoff; without a header the
// previous wait doubles (capped), mirroring the reference monitors.
func rateLimitDelay(retryAfter, prevWait, interval time.Duration) time.Duration {
	if retryAfter > 0 {
		return min(max(retryAfter, interval), maxBackoff)
	}
	return min(prevWait*2, maxBackoff)
}

// collectLoop runs collect+push, backing off on 429 rate-limit errors.
// On success or non-rate-limit errors the interval resets to cfg.collectInterval.
func collectLoop(ctx context.Context, cfg daemonConfig) {
	interval := cfg.collectInterval
	nextWait := interval

	rateLimited, retryAfter := safeCollect(ctx, cfg)
	if rateLimited {
		nextWait = rateLimitDelay(retryAfter, nextWait, interval)
		log.Printf("collect: rate limited, backing off — next attempt in %s", nextWait)
	}

	timer := time.NewTimer(nextWait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			rateLimited, retryAfter = safeCollect(ctx, cfg)
			if rateLimited {
				nextWait = rateLimitDelay(retryAfter, nextWait, interval)
				log.Printf("collect: rate limited, backing off — next attempt in %s", nextWait)
			} else {
				nextWait = interval
			}
			timer.Reset(nextWait)
		}
	}
}

// safeCollect wraps collectOnce in panic recovery and reports whether the
// cycle was rate-limited plus any server-requested retry delay.
func safeCollect(ctx context.Context, cfg daemonConfig) (rateLimited bool, retryAfter time.Duration) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("collect: panic recovered: %v", r)
		}
	}()
	return collectOnce(ctx, cfg)
}

// collectOnce is the per-tick collect+push pipeline. Reports whether the API
// responded with 429 (and the Retry-After delay, if any) so callers can back off.
func collectOnce(ctx context.Context, cfg daemonConfig) (rateLimited bool, retryAfter time.Duration) {
	start := time.Now()
	payload, ops, err := cfg.collector.Collect(ctx)
	if err != nil {
		log.Printf("collect: %v ops=%v (%v)", err, ops, time.Since(start))
		return errors.Is(err, ErrRateLimit), retryAfterFrom(err)
	}
	if err := cfg.pusher.Push(ctx, payload); err != nil {
		log.Printf("collect: push failed ops=%v err=%v (%v)", ops, err, time.Since(start))
		return false, 0
	}
	log.Printf("collect: ok ops=%v (%v)", ops, time.Since(start))
	return false, 0
}
