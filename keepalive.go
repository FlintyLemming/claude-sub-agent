package main

import (
	"context"
	"log"
	"time"
)

// keepaliveInterval is how often the daemon pings the CLI to keep the 5-hour
// session window warm. Defaults to 30m per the design doc §5.
const keepaliveTimeout = 30 * time.Second

// Keepalive runs a single no-op Claude Code invocation so the CLI refreshes
// its session and OAuth token. It is best-effort: failures are logged and
// swallowed so the daemon's collect loop is never disrupted.
type Keepalive struct {
	Refresher TokenRefresher
}

// Run performs one keepalive ping. Returns nil on success; on failure logs and
// still returns nil (keepalive is never fatal).
func (k *Keepalive) Run(ctx context.Context) error {
	kctx, cancel := context.WithTimeout(ctx, keepaliveTimeout)
	defer cancel()
	start := time.Now()
	if err := k.Refresher.Refresh(kctx); err != nil {
		log.Printf("keepalive: failed after %v: %v", time.Since(start), err)
		return nil
	}
	log.Printf("keepalive: ok (%v)", time.Since(start))
	return nil
}
