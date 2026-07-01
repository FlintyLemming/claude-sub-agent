package main

import (
	"context"
	"fmt"
	"os/exec"
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
