package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderPlist_ContainsRequiredKeys(t *testing.T) {
	got, err := renderPlist("/usr/local/bin/claude-usage-agent", []string{
		"--push-url=http://localhost:8000/api/push/claude",
		"--interval=5m0s",
		"--keepalive=true",
	})
	if err != nil {
		t.Fatalf("renderPlist err = %v", err)
	}

	mustContain := []string{
		`<?xml version="1.0" encoding="UTF-8"?>`,
		`<key>Label</key>`,
		fmt.Sprintf("<string>%s</string>", launchdLabel),
		`<key>ProgramArguments</key>`,
		`<key>KeepAlive</key>`,
		`<true/>`,
		`<key>RunAtLoad</key>`,
		`<key>StandardOutPath</key>`,
		`<key>StandardErrorPath</key>`,
		`<string>/usr/local/bin/claude-usage-agent</string>`,
		`<string>daemon</string>`,
		`<string>--push-url=http://localhost:8000/api/push/claude</string>`,
		`<string>--keepalive=true</string>`,
	}
	for _, want := range mustContain {
		if !strings.Contains(got, want) {
			t.Errorf("plist missing %q\n--- plist ---\n%s", want, got)
		}
	}
}

func TestRenderPlist_XMLEscapesArgs(t *testing.T) {
	// An arg containing XML-special characters must be escaped, not dropped.
	got, err := renderPlist("/bin/x", []string{`--push-url=http://h/?a=1&b<2`})
	if err != nil {
		t.Fatalf("renderPlist err = %v", err)
	}
	if strings.Contains(got, "&b<2") {
		t.Errorf("plist did not escape ampersand/lt:\n%s", got)
	}
	if !strings.Contains(got, "&amp;") {
		t.Errorf("plist missing escaped ampersand:\n%s", got)
	}
}

func TestRenderPlist_LogPathInHome(t *testing.T) {
	got, err := renderPlist("/bin/x", nil)
	if err != nil {
		t.Fatalf("renderPlist err = %v", err)
	}
	home, _ := os.UserHomeDir()
	wantLog := filepath.Join(home, "Library", "Logs", logName)
	if !strings.Contains(got, wantLog) {
		t.Errorf("plist missing log path %q:\n%s", wantLog, got)
	}
}

func TestRunInstall_WritesPlistAndLoads(t *testing.T) {
	// Redirect the plist + log locations into a tempdir so the test never
	// touches the real ~/Library. We do this by temporarily swapping the
	// path-resolving functions — but those are package funcs. Instead we
	// exercise runInstall via a stub shellRunner that records the launchctl
	// calls, and verify the written plist file by overriding HOME.
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	var calls []shellCall
	runner := func(name string, args ...string) ([]byte, error) {
		calls = append(calls, shellCall{name: name, args: args})
		return nil, nil
	}

	err := runInstall([]string{"--interval=5m0s"}, runner)
	if err != nil {
		t.Fatalf("runInstall err = %v", err)
	}

	// launchctl unload (best-effort) then load must have run.
	var loaded, unloaded bool
	for _, c := range calls {
		if c.name == "launchctl" && len(c.args) >= 1 && c.args[0] == "unload" {
			unloaded = true
		}
		if c.name == "launchctl" && len(c.args) >= 1 && c.args[0] == "load" {
			loaded = true
		}
	}
	if !loaded {
		t.Error("expected launchctl load to be called")
	}
	// unload may or may not appear depending on whether the file pre-existed;
	// it's best-effort. We only assert it doesn't error out the flow.
	_ = unloaded

	// The plist file must exist in the redirected HOME.
	pp := filepath.Join(dir, "Library", "LaunchAgents", plistName)
	content, err := os.ReadFile(pp)
	if err != nil {
		t.Fatalf("plist not written: %v", err)
	}
	if !strings.Contains(string(content), launchdLabel) {
		t.Errorf("plist content missing label:\n%s", content)
	}
}

func TestRunUninstall_RemovesPlist(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	pp := filepath.Join(dir, "Library", "LaunchAgents", plistName)
	if err := os.MkdirAll(filepath.Dir(pp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pp, []byte("dummy"), 0o644); err != nil {
		t.Fatal(err)
	}

	runner := func(name string, args ...string) ([]byte, error) { return nil, nil }
	if err := runUnload(runner); err != nil {
		t.Fatalf("runUnload err = %v", err)
	}
	if _, err := os.Stat(pp); !os.IsNotExist(err) {
		t.Errorf("plist still exists after uninstall")
	}
}

func TestRunUninstall_NoPlistIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	runner := func(name string, args ...string) ([]byte, error) { return nil, nil }
	if err := runUnload(runner); err != nil {
		t.Errorf("runUnload on missing plist err = %v, want nil", err)
	}
}

func TestRunStatus_Running(t *testing.T) {
	runner := func(name string, args ...string) ([]byte, error) {
		// Approximate launchctl print output for a running service.
		return []byte("pid = 12345\nlast exit code = 0\nstate = running\n"), nil
	}
	info, err := runStatus(runner)
	if err != nil {
		t.Fatalf("runStatus err = %v", err)
	}
	if !info.Loaded {
		t.Error("expected Loaded=true")
	}
	if info.PID != "12345" {
		t.Errorf("PID = %q, want 12345", info.PID)
	}
	if info.State != "running" {
		t.Errorf("State = %q, want running", info.State)
	}
}

func TestRunStatus_NotLoaded(t *testing.T) {
	runner := func(name string, args ...string) ([]byte, error) {
		return nil, fmt.Errorf("Could not find service")
	}
	info, err := runStatus(runner)
	if err != nil {
		t.Fatalf("runStatus err = %v", err)
	}
	if info.Loaded {
		t.Error("expected Loaded=false for unknown service")
	}
	if info.State != "not loaded" {
		t.Errorf("State = %q, want 'not loaded'", info.State)
	}
}

func TestRunStatus_RealErrorSurfaced(t *testing.T) {
	// A launchctl failure that is NOT "service not found" must be returned as
	// an error rather than silently reported as "not loaded".
	runner := func(name string, args ...string) ([]byte, error) {
		return []byte("launchctl: permission denied"), fmt.Errorf("exit status 1")
	}
	info, err := runStatus(runner)
	if err == nil {
		t.Fatal("runStatus err = nil, want error for non-not-found launchctl failure")
	}
	if info.Loaded {
		t.Error("expected Loaded=false")
	}
	if info.State == "not loaded" {
		t.Errorf("State = 'not loaded', but this was a real error not an unload")
	}
}

func TestRunStatus_Crashed(t *testing.T) {
	runner := func(name string, args ...string) ([]byte, error) {
		// Service is loaded but no pid, last exit non-zero.
		return []byte("last exit code = 1\n"), nil
	}
	info, _ := runStatus(runner)
	if !strings.Contains(info.State, "crashed") {
		t.Errorf("State = %q, want 'crashed'", info.State)
	}
	if info.LastExit != "1" {
		t.Errorf("LastExit = %q, want 1", info.LastExit)
	}
}

type shellCall struct {
	name string
	args []string
}
