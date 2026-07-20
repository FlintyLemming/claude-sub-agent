package main

import (
	"os/exec"
	"regexp"
)

// shellRunner abstracts external command execution so tests can avoid touching
// the real system (launchctl on macOS, schtasks on Windows).
type shellRunner func(name string, args ...string) ([]byte, error)

func realShell() shellRunner {
	return func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).Output()
	}
}

// statusInfo summarises what the platform service manager reports for the agent.
type statusInfo struct {
	Label    string
	Loaded   bool
	PID      string // empty if not running
	LastExit string // empty if never exited
	State    string // human-readable, e.g. "running" / "exited"
}

// firstMatch returns the first capture group of pattern in text, or "".
func firstMatch(text, pattern string) string {
	re := regexp.MustCompile(pattern)
	m := re.FindStringSubmatch(text)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}
