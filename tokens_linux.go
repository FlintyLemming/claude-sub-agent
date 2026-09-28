package main

// newPlatformTokens returns the Linux credential source. Claude Code on Linux
// has no keyring backend — credentials live only in ~/.claude/.credentials.json
// (or $CLAUDE_CONFIG_DIR).
func newPlatformTokens() TokenProvider {
	return newFileTokens()
}
