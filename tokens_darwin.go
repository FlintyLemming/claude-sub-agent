package main

import (
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

// newPlatformTokens returns the macOS credential chain: the credentials file
// first, falling back to Keychain — either source alone is enough, so a
// machine that stores creds in only one of them still collects.
func newPlatformTokens() TokenProvider {
	return newChainTokens(newFileTokens(), newSecurityTokens())
}
