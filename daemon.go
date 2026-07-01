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
	collectInterval   time.Duration
	keepaliveEnabled  bool
	keepaliveInterval time.Duration
	collector         *Collector
	pusher            *Pusher
	keepalive         *Keepalive
}

// runDaemon runs the collect ticker and (optionally) the keepalive ticker as
// independent goroutines sharing one cancellable context. It blocks until it
// receives SIGINT/SIGTERM, then waits for both loops to stop.
func runDaemon(cfg daemonConfig) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var loops []func(context.Context)

	loops = append(loops, func(ctx context.Context) {
		collectLoop(ctx, cfg)
	})

	if cfg.keepaliveEnabled {
		loops = append(loops, func(ctx context.Context) {
			runLoop(ctx, "keepalive", cfg.keepaliveInterval, func() {
				_ = cfg.keepalive.Run(ctx)
			})
		})
	}

	done := make(chan struct{})
	for _, loop := range loops {
		loop := loop
		go func() {
			loop(ctx)
			done <- struct{}{}
		}()
	}

	if cfg.keepaliveEnabled {
		log.Printf("daemon: started (collect every %s, keepalive every %s)",
			cfg.collectInterval, cfg.keepaliveInterval)
	} else {
		log.Printf("daemon: started (collect every %s, keepalive disabled)",
			cfg.collectInterval)
	}

	// Wait for all loops to finish shutting down.
	for range loops {
		<-done
	}
	log.Printf("daemon: stopped")
}

// runLoop calls fn immediately, then every interval until ctx is cancelled.
// Each fn invocation is isolated: a panic or long fn never breaks the ticker.
func runLoop(ctx context.Context, name string, interval time.Duration, fn func()) {
	// Fire once at startup so a freshly launched daemon reports immediately.
	safe(fn, name)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			safe(fn, name)
		}
	}
}

// safe runs fn recovering from panics so a single failure can't kill the loop.
func safe(fn func(), name string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("%s: panic recovered: %v", name, r)
		}
	}()
	fn()
}

// collectLoop runs collect+push with exponential backoff on 429 rate-limit errors.
// On success or non-rate-limit errors the interval resets to cfg.collectInterval.
func collectLoop(ctx context.Context, cfg daemonConfig) {
	const maxBackoff = 30 * time.Minute
	interval := cfg.collectInterval
	nextWait := interval

	rateLimited := safeCollect(ctx, cfg)
	if rateLimited {
		nextWait = min(nextWait*2, maxBackoff)
		log.Printf("collect: rate limited, backing off — next attempt in %s", nextWait)
	}

	timer := time.NewTimer(nextWait)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			rateLimited = safeCollect(ctx, cfg)
			if rateLimited {
				nextWait = min(nextWait*2, maxBackoff)
				log.Printf("collect: rate limited, backing off — next attempt in %s", nextWait)
			} else {
				nextWait = interval
			}
			timer.Reset(nextWait)
		}
	}
}

// safeCollect wraps collectOnce in panic recovery and returns true when the
// collect cycle was rate-limited (so the caller can apply a backoff).
func safeCollect(ctx context.Context, cfg daemonConfig) (rateLimited bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("collect: panic recovered: %v", r)
		}
	}()
	return collectOnce(ctx, cfg)
}

// collectOnce is the per-tick collect+push pipeline. Returns true if the API
// responded with 429 (rate limited) so callers can apply backoff.
func collectOnce(ctx context.Context, cfg daemonConfig) bool {
	start := time.Now()
	payload, ops, err := cfg.collector.Collect(ctx)
	if err != nil {
		log.Printf("collect: %v ops=%v (%v)", err, ops, time.Since(start))
		return errors.Is(err, ErrRateLimit)
	}
	if err := cfg.pusher.Push(ctx, payload); err != nil {
		log.Printf("collect: push failed ops=%v err=%v (%v)", ops, err, time.Since(start))
		return false
	}
	log.Printf("collect: ok ops=%v (%v)", ops, time.Since(start))
	return false
}
