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

// newPlatformTokens returns the macOS credential source: whichever of the
// Keychain (where the CLI keeps its live token) and the credentials file holds
// the later-expiring token, with the Keychain winning ties. Either source alone
// is enough, so a machine that stores creds in only one of them still collects.
func newPlatformTokens() TokenProvider {
	return newFreshestTokens(newSecurityTokens(), newFileTokens())
}
