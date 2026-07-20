package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// securityTokens reads credentials from macOS Keychain via the `security`
// tool. Matches the reference project's invocation exactly.
type securityTokens struct {
	service string
}

func newSecurityTokens() TokenProvider {
	return &securityTokens{service: KeychainService}
}

func (s *securityTokens) Credentials() ([]byte, error) {
	cmd := exec.Command("security", "find-generic-password", "-s", s.service, "-w")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("security find-generic-password: %w", err)
	}
	return []byte(strings.TrimSpace(string(out))), nil
}

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

// fileTokens reads credentials from ~/.claude/.credentials.json file.
// This is used as a fallback when Keychain access fails.
type fileTokens struct {
	path string
}

func newFileTokens() TokenProvider {
	home, err := os.UserHomeDir()
	if err != nil {
		// Fallback to security tokens if we can't determine home dir
		return newSecurityTokens()
	}
	return &fileTokens{
		path: filepath.Join(home, ".claude", ".credentials.json"),
	}
}

func (f *fileTokens) Credentials() ([]byte, error) {
	data, err := os.ReadFile(f.path)
	if err != nil {
		return nil, fmt.Errorf("read credentials file: %w", err)
	}
	return data, nil
}

// cliRefresher keeps the session/token alive by running a no-op Claude Code
// command. The CLI refreshes and persists its OAuth token as a side effect.
type cliRefresher struct{}

func newCLIRefresher() TokenRefresher { return &cliRefresher{} }

func (c *cliRefresher) Refresh(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "claude", "--print", "--model", "haiku", "-p", "hi")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("claude keepalive: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
