package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// freshestTokens reads every provider and returns the credentials whose access
// token expires last. On macOS the CLI keeps its live token in the Keychain, but
// a ~/.claude/.credentials.json can linger beside it (older tools, Claude
// Desktop's embedded engine) holding a long-dead token — always preferring one
// source would let a stale copy shadow the live one. Ties keep provider order,
// and a blob without expiresAt ranks below any that has one. Providers that
// fail or hold no token are skipped; if none yields a token, all their errors
// are joined so the log shows exactly why each source came up empty.
type freshestTokens struct {
	providers []TokenProvider
}

func newFreshestTokens(providers ...TokenProvider) TokenProvider {
	return &freshestTokens{providers: providers}
}

func (f *freshestTokens) Credentials() ([]byte, error) {
	var best []byte
	var bestExpiry time.Time
	var errs []error
	for _, p := range f.providers {
		raw, err := p.Credentials()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		creds, err := accessToken(raw)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if best == nil || creds.ExpiresAt.After(bestExpiry) {
			best, bestExpiry = raw, creds.ExpiresAt
		}
	}
	if best == nil {
		return nil, errors.Join(errs...)
	}
	return best, nil
}

// fileTokens reads credentials from the Claude Code credentials file:
// $CLAUDE_CONFIG_DIR/.credentials.json when set, else ~/.claude/.credentials.json.
type fileTokens struct {
	path string
}

func newFileTokens() TokenProvider {
	return &fileTokens{path: defaultCredentialsPath()}
}

// defaultCredentialsPath resolves the Claude Code credentials file location the
// same way the CLI does: $CLAUDE_CONFIG_DIR/.credentials.json when the env var
// is set, else ~/.claude/.credentials.json. Returns "" only when the home dir
// can't be determined (callers then surface a clear read error).
func defaultCredentialsPath() string {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".claude")
	}
	return filepath.Join(dir, ".credentials.json")
}

func (f *fileTokens) Credentials() ([]byte, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		return nil, fmt.Errorf("read credentials file: %w", err)
	}
	return data, nil
}

// fallbackUserAgent is used when `claude --version` is unavailable; a real
// CLI version string, kept roughly current so the UA stays plausible.
const fallbackUserAgent = "claude-code/2.1.201"

var (
	userAgentOnce   sync.Once
	userAgentCached string
)

// claudeUserAgent returns "claude-code/<version>" using the installed CLI's
// version. The subprocess runs once per process (the daemon restarts on
// upgrade anyway); on any failure the static fallback is used.
func claudeUserAgent() string {
	userAgentOnce.Do(func() {
		userAgentCached = fallbackUserAgent
		out, err := exec.Command("claude", "--version").Output()
		if err != nil {
			return
		}
		// Output format: "2.1.201 (Claude Code)".
		version, _, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
		if version != "" {
			userAgentCached = "claude-code/" + version
		}
	})
	return userAgentCached
}

// refreshGuardModel is passed as --model to the refresh invocation. /usage is a
// local command and never reaches a model; should some CLI version forward the
// text to the model instead, a nonexistent model makes that request fail with a
// 404 before any quota is spent.
const refreshGuardModel = "claude-usage-agent-no-inference"

// refreshTimeout bounds one CLI refresh run (normally ~2s) so a wedged CLI
// can't stall the collect loop.
const refreshTimeout = 2 * time.Minute

// cliRefresher has the Claude Code CLI rotate an expired OAuth token. `claude
// -p /usage` runs the local /usage command, whose usage fetch first refreshes
// an expired token through the CLI's own path: it takes the shared
// ~/.claude/.oauth_refresh.lock, re-reads the credentials under it, and writes
// the rotated token back to the store the CLI itself reads (Keychain on macOS,
// the credentials file elsewhere). No model is called, so no quota is spent.
//
// `claude update` never touches OAuth, and refreshing against the token
// endpoint ourselves would spend the CLI's single-use refresh token behind its
// back, which forces a /login the next time the CLI refreshes.
type cliRefresher struct{}

func newCLIRefresher() TokenRefresher { return &cliRefresher{} }

func (c *cliRefresher) command(ctx context.Context) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "claude", "-p", "/usage",
		"--model", refreshGuardModel,
		"--output-format", "json",
		"--no-session-persistence", // no transcript for this throwaway session
		"--strict-mcp-config",      // don't spawn the user's MCP servers
	)
	// Run from a neutral directory instead of wherever the service manager
	// started us (launchd uses /), so no project config gets picked up.
	cmd.Dir = os.TempDir()
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// cliResult is the subset of `claude -p --output-format json` output the
// refresher checks.
type cliResult struct {
	LocalCommand string `json:"local_command"`
	NumTurns     int    `json:"num_turns"`
	Result       string `json:"result"`
}

func (c *cliRefresher) Refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()

	out, err := c.command(ctx).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("claude -p /usage: %w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return fmt.Errorf("claude -p /usage: %w", err)
	}
	return checkLocalUsage(out)
}

// checkLocalUsage verifies that `claude -p /usage` output came from the local
// /usage command. Only that path goes through the token refresh; anything else
// means this CLI sent the text to the model (where the guard model stopped it).
func checkLocalUsage(out []byte) error {
	var res cliResult
	if err := json.Unmarshal(out, &res); err != nil {
		return fmt.Errorf("claude -p /usage: unexpected output: %w", err)
	}
	if res.LocalCommand != "usage" {
		return fmt.Errorf("claude -p /usage was not run as a local command (num_turns=%d): %s",
			res.NumTurns, strings.TrimSpace(res.Result))
	}
	return nil
}
