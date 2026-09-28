package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixedTokens is a TokenProvider stub returning a preset blob or error.
type fixedTokens struct {
	cred []byte
	err  error
}

func (f *fixedTokens) Credentials() ([]byte, error) { return f.cred, f.err }

// TestCLIRefresher_RunsLocalUsageCommand pins the refresh invocation: the local
// /usage command (refreshes through the CLI, no model call) behind a guard
// model, JSON output for the local-command check, and no transcript or MCP
// servers for the throwaway session.
func TestCLIRefresher_RunsLocalUsageCommand(t *testing.T) {
	cmd := (&cliRefresher{}).command(context.Background())
	want := []string{
		"claude", "-p", "/usage",
		"--model", refreshGuardModel,
		"--output-format", "json",
		"--no-session-persistence",
		"--strict-mcp-config",
	}
	if strings.Join(cmd.Args, " ") != strings.Join(want, " ") {
		t.Errorf("refresh command args = %v, want %v", cmd.Args, want)
	}
	if cmd.Dir != os.TempDir() {
		t.Errorf("refresh command dir = %q, want %q", cmd.Dir, os.TempDir())
	}
}

func TestCheckLocalUsage(t *testing.T) {
	t.Run("local /usage run passes", func(t *testing.T) {
		// Trimmed from real `claude -p /usage --output-format json` output (2.1.283).
		out := []byte(`{"type":"result","subtype":"success","is_error":false,"num_turns":0,
			"total_cost_usd":0,"local_command":"usage",
			"result":"Current session: 10% used · resets Sep 28, 11:09am (UTC)"}`)
		if err := checkLocalUsage(out); err != nil {
			t.Errorf("checkLocalUsage = %v, want nil", err)
		}
	})

	t.Run("text forwarded to the model fails", func(t *testing.T) {
		// What a CLI that doesn't run /usage locally returns: one turn, stopped
		// by the guard model's 404.
		out := []byte(`{"type":"result","subtype":"success","is_error":true,"num_turns":1,
			"api_error_status":404,"result":"There's an issue with the selected model (claude-usage-agent-no-inference)."}`)
		err := checkLocalUsage(out)
		if err == nil || !strings.Contains(err.Error(), "not run as a local command") {
			t.Errorf("checkLocalUsage = %v, want not-a-local-command error", err)
		}
	})

	t.Run("non-JSON output fails", func(t *testing.T) {
		if err := checkLocalUsage([]byte("Error: something broke")); err == nil {
			t.Error("checkLocalUsage = nil, want error for non-JSON output")
		}
	})
}

// TestFileTokens_HonorsClaudeConfigDir: like Claude Code itself, the file
// provider must look in $CLAUDE_CONFIG_DIR/.credentials.json when the env var
// is set, falling back to ~/.claude only when it isn't.
func TestFileTokens_HonorsClaudeConfigDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	blob := []byte(`{"claudeAiOauth":{"accessToken":"from-config-dir"}}`)
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), blob, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := newFileTokens().Credentials()
	if err != nil {
		t.Fatalf("Credentials err = %v", err)
	}
	if string(got) != string(blob) {
		t.Errorf("Credentials = %s, want the CLAUDE_CONFIG_DIR blob", got)
	}
}

func TestFreshestTokens(t *testing.T) {
	now := time.Now()
	live := credJSONExpiring(t, "live", now.Add(7*time.Hour))
	stale := credJSONExpiring(t, "stale", now.Add(-20*time.Hour))
	tokenOf := func(t *testing.T, raw []byte) string {
		t.Helper()
		creds, err := accessToken(raw)
		if err != nil {
			t.Fatalf("accessToken: %v", err)
		}
		return creds.Token
	}

	t.Run("later expiry wins regardless of order", func(t *testing.T) {
		// A stale credentials file must not shadow the live Keychain token.
		for _, order := range [][]TokenProvider{
			{&fixedTokens{cred: live}, &fixedTokens{cred: stale}},
			{&fixedTokens{cred: stale}, &fixedTokens{cred: live}},
		} {
			got, err := newFreshestTokens(order...).Credentials()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tok := tokenOf(t, got); tok != "live" {
				t.Errorf("got token %q, want live", tok)
			}
		}
	})

	t.Run("tie keeps provider order", func(t *testing.T) {
		exp := now.Add(time.Hour)
		got, err := newFreshestTokens(
			&fixedTokens{cred: credJSONExpiring(t, "keychain", exp)},
			&fixedTokens{cred: credJSONExpiring(t, "file", exp)},
		).Credentials()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tok := tokenOf(t, got); tok != "keychain" {
			t.Errorf("got token %q, want keychain", tok)
		}
	})

	t.Run("blob without expiresAt ranks below one with it", func(t *testing.T) {
		got, err := newFreshestTokens(
			&fixedTokens{cred: credJSON(t, "no-expiry")},
			&fixedTokens{cred: stale},
		).Credentials()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tok := tokenOf(t, got); tok != "stale" {
			t.Errorf("got token %q, want stale", tok)
		}
	})

	t.Run("failing or tokenless providers are skipped", func(t *testing.T) {
		got, err := newFreshestTokens(
			&fixedTokens{err: errors.New("keychain locked")},
			&fixedTokens{cred: []byte(`{"claudeAiOauth":{"accessToken":""}}`)},
			&fixedTokens{cred: live},
		).Credentials()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tok := tokenOf(t, got); tok != "live" {
			t.Errorf("got token %q, want live", tok)
		}
	})

	t.Run("all fail, errors joined", func(t *testing.T) {
		errA := errors.New("keychain empty")
		errB := errors.New("file missing")
		_, err := newFreshestTokens(&fixedTokens{err: errA}, &fixedTokens{err: errB}).Credentials()
		if err == nil {
			t.Fatal("expected error when all providers fail")
		}
		if !errors.Is(err, errA) || !errors.Is(err, errB) {
			t.Fatalf("joined error missing a cause: %v", err)
		}
	})
}
