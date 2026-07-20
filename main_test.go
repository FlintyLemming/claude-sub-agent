package main

import (
	"testing"
	"time"
)

func TestParseRuntimeFlags_Defaults(t *testing.T) {
	cfg, err := parseRuntimeFlags(nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if cfg.pushURL != "http://localhost:8000/api/push/v2/claude-personal" {
		t.Errorf("pushURL = %q", cfg.pushURL)
	}
	if cfg.pushToken != "" {
		t.Errorf("pushToken = %q, want empty default", cfg.pushToken)
	}
	if cfg.interval != 5*time.Minute {
		t.Errorf("interval = %v", cfg.interval)
	}
}

func TestParseRuntimeFlags_FlagOverridesEnv(t *testing.T) {
	t.Setenv("CLAUDE_USAGE_PUSH_URL", "http://from-env:9999")
	t.Setenv("CLAUDE_USAGE_INTERVAL", "1m")
	cfg, err := parseRuntimeFlags([]string{
		"--push-url=http://from-flag:1234",
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	// Flag beats env.
	if cfg.pushURL != "http://from-flag:1234" {
		t.Errorf("pushURL = %q, want flag value", cfg.pushURL)
	}
	// Env beats default when no flag given.
	if cfg.interval != time.Minute {
		t.Errorf("interval = %v, want 1m from env", cfg.interval)
	}
}

// TestParseRuntimeFlags_DeprecatedKeepaliveAccepted: a plist written by an
// older version still passes --keepalive/--keepalive-interval; the new binary
// must parse (and ignore) them instead of crash-looping under launchd.
func TestParseRuntimeFlags_DeprecatedKeepaliveAccepted(t *testing.T) {
	cfg, err := parseRuntimeFlags([]string{
		"--keepalive=true",
		"--keepalive-interval=30m0s",
		"--interval=2m",
	})
	if err != nil {
		t.Fatalf("err = %v, want deprecated flags to parse cleanly", err)
	}
	if cfg.interval != 2*time.Minute {
		t.Errorf("interval = %v", cfg.interval)
	}
}

func TestParseRuntimeFlags_EffectiveArgsPersisted(t *testing.T) {
	// install must persist the resolved config as explicit flags so the daemon
	// launched by launchd uses the same values the user installed with.
	cfg, err := parseRuntimeFlags([]string{"--interval=2m"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	joined := ""
	for _, a := range cfg.effectiveArgs {
		joined += a + " "
	}
	for _, want := range []string{
		"--push-url=http://localhost:8000/api/push/v2/claude-personal",
		"--interval=2m0s",
	} {
		if !containsStr(joined, want) {
			t.Errorf("effectiveArgs missing %q: %v", want, cfg.effectiveArgs)
		}
	}
	// No token configured → no --push-token arg persisted.
	if containsStr(joined, "--push-token=") {
		t.Errorf("effectiveArgs should omit --push-token when unset: %v", cfg.effectiveArgs)
	}
	// Keepalive is gone: deprecated flags must not be re-persisted into the plist.
	for _, banned := range []string{"--keepalive=", "--keepalive-interval="} {
		if containsStr(joined, banned) {
			t.Errorf("effectiveArgs must not persist %q: %v", banned, cfg.effectiveArgs)
		}
	}
}

func TestParseRuntimeFlags_PushToken(t *testing.T) {
	t.Setenv("CLAUDE_USAGE_PUSH_TOKEN", "from-env")
	cfg, err := parseRuntimeFlags(nil)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if cfg.pushToken != "from-env" {
		t.Errorf("pushToken = %q, want from-env", cfg.pushToken)
	}

	cfg, err = parseRuntimeFlags([]string{"--push-token=from-flag"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if cfg.pushToken != "from-flag" {
		t.Errorf("pushToken = %q, want flag to beat env", cfg.pushToken)
	}
	if !containsStr(joinArgs(cfg.effectiveArgs), "--push-token=from-flag") {
		t.Errorf("effectiveArgs missing --push-token: %v", cfg.effectiveArgs)
	}
}

func joinArgs(args []string) string {
	joined := ""
	for _, a := range args {
		joined += a + " "
	}
	return joined
}

func TestEnvBoolOr_Values(t *testing.T) {
	cases := map[string]bool{
		"true": true, "1": true, "yes": true,
		"false": false, "0": false, "no": false,
		"":        true, // unset/default handled by caller; here empty falls to default
		"garbage": true, // unrecognized falls to default
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			if got := envBoolOr("X", true); got != true {
				t.Errorf("default path = %v", got)
			}
			t.Setenv("X", in)
			if got := envBoolOr("X", true); got != want {
				t.Errorf("envBoolOr(%q) = %v, want %v", in, got, want)
			}
		})
	}
}
