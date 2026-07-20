package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"
)

// usage prints a short top-level help listing the subcommands.
func usage() {
	fmt.Fprint(os.Stderr, `claude-usage-agent — stateless collector that forwards Claude Code usage to a local service.

Usage:
  claude-usage-agent <command> [flags]

Commands:
  daemon      Run continuously: collect every 5m.
  collect     Collect once and push (debug/manual).
  install     Register the background service (with current flags).
  uninstall   Unregister the background service.
  status      Show the service status.

Flags (daemon / collect):
  --push-url             HTTP endpoint to POST payloads to; must be an
                         ai-plan-insight v2 push endpoint
                         /api/push/v2/{instance_id}
                         (env CLAUDE_USAGE_PUSH_URL)
  --push-token           Bearer token matching the server's push_auth_secret
                         (env CLAUDE_USAGE_PUSH_TOKEN)
  --interval             Collect interval (default 5m)
                         (env CLAUDE_USAGE_INTERVAL)

Precedence: flag > env > default.
`)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("")

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "daemon":
		cmdDaemon()
	case "collect":
		cmdCollect()
	case "install":
		cmdInstall()
	case "uninstall":
		cmdUninstall()
	case "status":
		cmdStatus()
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

// runtimeFlags holds the effective daemon/collect configuration after flag/env
// resolution. Fields are populated by parseRuntimeFlags.
type runtimeFlags struct {
	pushURL   string
	pushToken string
	interval  time.Duration
	// effectiveArgs is the flag form of the resolved values, persisted into the
	// service definition by `install`.
	effectiveArgs []string
}

// parseRuntimeFlags resolves flags and environment for daemon/collect/install.
// restArgs is os.Args after the subcommand. precedence: flag > env > default.
func parseRuntimeFlags(restArgs []string) (runtimeFlags, error) {
	fs := flag.NewFlagSet("claude-usage-agent", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	cfg := runtimeFlags{
		pushURL:   envOr("CLAUDE_USAGE_PUSH_URL", "http://localhost:8000/api/push/v2/claude-personal"),
		pushToken: envOr("CLAUDE_USAGE_PUSH_TOKEN", ""),
		interval:  envDurOr("CLAUDE_USAGE_INTERVAL", 5*time.Minute),
	}
	fs.StringVar(&cfg.pushURL, "push-url", cfg.pushURL, "HTTP endpoint to POST payloads to (v2: /api/push/v2/{instance_id})")
	fs.StringVar(&cfg.pushToken, "push-token", cfg.pushToken, "Bearer token matching the server's push_auth_secret")
	fs.DurationVar(&cfg.interval, "interval", cfg.interval, "collect interval")

	// Deprecated, ignored: accepted so a service definition written by an older
	// version (which passed these) doesn't crash-loop the new binary.
	var deprecatedKeepalive bool
	var deprecatedKeepaliveInterval time.Duration
	fs.BoolVar(&deprecatedKeepalive, "keepalive", true, "deprecated: ignored")
	fs.DurationVar(&deprecatedKeepaliveInterval, "keepalive-interval", 30*time.Minute, "deprecated: ignored")

	if err := fs.Parse(restArgs); err != nil {
		return cfg, err
	}

	// Reconstruct the effective args in flag form so `install` can persist
	// exactly what would have run.
	cfg.effectiveArgs = []string{
		"--push-url=" + cfg.pushURL,
		"--interval=" + cfg.interval.String(),
	}
	if cfg.pushToken != "" {
		cfg.effectiveArgs = append(cfg.effectiveArgs, "--push-token="+cfg.pushToken)
	}
	return cfg, nil
}

func cmdDaemon() {
	cfg, err := parseRuntimeFlags(os.Args[2:])
	if err != nil {
		os.Exit(2)
	}
	runDaemon(daemonConfig{
		collectInterval: cfg.interval,
		collector:       newCollector(),
		pusher:          NewPusher(cfg.pushURL, cfg.pushToken),
	})
}

func cmdCollect() {
	cfg, err := parseRuntimeFlags(os.Args[2:])
	if err != nil {
		os.Exit(2)
	}
	ctx := context.Background()
	collectOnce(ctx, daemonConfig{
		collectInterval: cfg.interval,
		collector:       newCollector(),
		pusher:          NewPusher(cfg.pushURL, cfg.pushToken),
	})
}

func cmdInstall() {
	cfg, err := parseRuntimeFlags(os.Args[2:])
	if err != nil {
		os.Exit(2)
	}
	if err := runInstall(cfg.effectiveArgs, realShell()); err != nil {
		fmt.Fprintf(os.Stderr, "install: %v\n", err)
		os.Exit(1)
	}
}

func cmdUninstall() {
	if err := runUnload(realShell()); err != nil {
		fmt.Fprintf(os.Stderr, "uninstall: %v\n", err)
		os.Exit(1)
	}
}

func cmdStatus() {
	info, err := runStatus(realShell())
	if err != nil {
		fmt.Fprintf(os.Stderr, "status: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("label:      %s\n", info.Label)
	fmt.Printf("state:      %s\n", info.State)
	if info.PID != "" {
		fmt.Printf("pid:        %s\n", info.PID)
	}
	if info.LastExit != "" {
		fmt.Printf("last exit:  %s\n", info.LastExit)
	}
}

// newCollector wires the production token + CLI implementations. The token
// source is platform-specific (see tokens_darwin.go / tokens_windows.go).
func newCollector() *Collector {
	return &Collector{
		Tokens:    newPlatformTokens(),
		Refresher: newCLIRefresher(),
		API:       newUsageAPI(UsageAPIURL),
	}
}

// ── env helpers ────────────────────────────────────────────────────────────

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envDurOr(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envBoolOr(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		switch v {
		case "true", "1", "yes":
			return true
		case "false", "0", "no":
			return false
		}
	}
	return def
}
