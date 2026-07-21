package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// chainTokens tries each provider in order and returns the first that yields
// credentials. It only moves on to the next provider when the previous one
// errors, so the ordering encodes preference (e.g. file first, Keychain
// fallback). If every provider fails, all errors are joined so the log shows
// exactly why each source came up empty.
type chainTokens struct {
	providers []TokenProvider
}

func newChainTokens(providers ...TokenProvider) TokenProvider {
	return &chainTokens{providers: providers}
}

func (c *chainTokens) Credentials() ([]byte, error) {
	var errs []error
	for _, p := range c.providers {
		cred, err := p.Credentials()
		if err == nil {
			return cred, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
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

// cliRefresher rotates an expired OAuth token by running `claude update`. The
// CLI renews and persists the token as a side effect of any invocation, and
// `update` is the cheapest one: no model call, so no subscription quota spent.
type cliRefresher struct{}

func newCLIRefresher() TokenRefresher { return &cliRefresher{} }

func (c *cliRefresher) command(ctx context.Context) *exec.Cmd {
	return exec.CommandContext(ctx, "claude", "update")
}

func (c *cliRefresher) Refresh(ctx context.Context) error {
	cmd := c.command(ctx)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("claude update: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
