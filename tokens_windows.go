package main

// newPlatformTokens returns the Windows credential source. Claude Code on
// Windows has no Keychain equivalent — credentials live only in
// %USERPROFILE%\.claude\.credentials.json (or $CLAUDE_CONFIG_DIR).
func newPlatformTokens() TokenProvider {
	return newFileTokens()
}
