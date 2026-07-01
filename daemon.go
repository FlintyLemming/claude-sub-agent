package main

import (
	"context"
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
		runLoop(ctx, "collect", cfg.collectInterval, func() {
			collectOnce(ctx, cfg)
		})
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

// collectOnce is the per-tick collect+push pipeline used by both the daemon's
// collect loop and the `collect` subcommand.
func collectOnce(ctx context.Context, cfg daemonConfig) {
	start := time.Now()
	payload, ops, err := cfg.collector.Collect(ctx)
	if err != nil {
		log.Printf("collect: %v ops=%v (%v)", err, ops, time.Since(start))
		return
	}
	if err := cfg.pusher.Push(ctx, payload); err != nil {
		log.Printf("collect: push failed ops=%v err=%v (%v)", ops, err, time.Since(start))
		return
	}
	log.Printf("collect: ok ops=%v (%v)", ops, time.Since(start))
}
