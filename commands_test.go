package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fixedTokens is a TokenProvider stub returning a preset blob or error.
type fixedTokens struct {
	cred []byte
	err  error
}

func (f *fixedTokens) Credentials() ([]byte, error) { return f.cred, f.err }

// TestCLIRefresher_UsesUpdateCommand pins the refresh invocation to
// `claude update`: it rotates an expired OAuth token as a side effect without
// consuming any model quota, unlike a `claude -p` prompt.
func TestCLIRefresher_UsesUpdateCommand(t *testing.T) {
	r := &cliRefresher{}
	cmd := r.command(context.Background())
	if len(cmd.Args) != 2 || cmd.Args[0] != "claude" || cmd.Args[1] != "update" {
		t.Errorf("refresh command args = %v, want [claude update]", cmd.Args)
	}
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

func TestChainTokens(t *testing.T) {
	errA := errors.New("file missing")
	errB := errors.New("keychain empty")

	t.Run("first provider succeeds, later ones not consulted", func(t *testing.T) {
		second := &fixedTokens{err: errB}
		chain := newChainTokens(&fixedTokens{cred: []byte("from-file")}, second)
		got, err := chain.Credentials()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(got) != "from-file" {
			t.Fatalf("got %q, want %q", got, "from-file")
		}
	})

	t.Run("falls back to second when first fails", func(t *testing.T) {
		chain := newChainTokens(&fixedTokens{err: errA}, &fixedTokens{cred: []byte("from-keychain")})
		got, err := chain.Credentials()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(got) != "from-keychain" {
			t.Fatalf("got %q, want %q", got, "from-keychain")
		}
	})

	t.Run("all fail, errors joined", func(t *testing.T) {
		chain := newChainTokens(&fixedTokens{err: errA}, &fixedTokens{err: errB})
		_, err := chain.Credentials()
		if err == nil {
			t.Fatal("expected error when all providers fail")
		}
		if !errors.Is(err, errA) || !errors.Is(err, errB) {
			t.Fatalf("joined error missing a cause: %v", err)
		}
	})
}
