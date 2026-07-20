package main

import (
	"errors"
	"strings"
	"testing"
)

// TestTaskCommand_QuotesExecutableAndArgs: the schtasks /TR value must quote
// the exe path (spaces in %USERPROFILE% paths are common) and append the
// daemon subcommand plus the effective flags.
func TestTaskCommand_QuotesExecutableAndArgs(t *testing.T) {
	got := taskCommand(`C:\Users\Jane Doe\bin\claude-usage-agent.exe`, []string{
		"--push-url=http://localhost:8000/api/push/v2/claude-personal",
		"--interval=5m0s",
	})
	if !strings.HasPrefix(got, `"C:\Users\Jane Doe\bin\claude-usage-agent.exe" daemon `) {
		t.Errorf("taskCommand = %q, want quoted exe + daemon prefix", got)
	}
	if !strings.Contains(got, "--interval=5m0s") {
		t.Errorf("taskCommand = %q, missing flags", got)
	}
}

func TestSchtasksInstall_CreatesAndStartsTask(t *testing.T) {
	var calls []shellCall
	runner := func(name string, args ...string) ([]byte, error) {
		calls = append(calls, shellCall{name: name, args: args})
		return nil, nil
	}

	if err := schtasksInstall(`C:\bin\agent.exe`, []string{"--interval=5m0s"}, runner); err != nil {
		t.Fatalf("schtasksInstall err = %v", err)
	}

	var created, started bool
	for _, c := range calls {
		if c.name != "schtasks" || len(c.args) == 0 {
			continue
		}
		switch c.args[0] {
		case "/Create":
			created = true
			joined := strings.Join(c.args, " ")
			for _, want := range []string{"/SC ONLOGON", "/TN " + taskName, "/F"} {
				if !strings.Contains(joined, want) {
					t.Errorf("schtasks /Create missing %q: %v", want, c.args)
				}
			}
		case "/Run":
			started = true
		}
	}
	if !created {
		t.Error("expected schtasks /Create call")
	}
	if !started {
		t.Error("expected schtasks /Run call (install should start the task immediately)")
	}
}

func TestSchtasksUnload_EndsAndDeletesTask(t *testing.T) {
	var calls []shellCall
	runner := func(name string, args ...string) ([]byte, error) {
		calls = append(calls, shellCall{name: name, args: args})
		return nil, nil
	}

	if err := schtasksUnload(runner); err != nil {
		t.Fatalf("schtasksUnload err = %v", err)
	}

	var ended, deleted bool
	for _, c := range calls {
		if c.name != "schtasks" || len(c.args) == 0 {
			continue
		}
		switch c.args[0] {
		case "/End":
			ended = true
		case "/Delete":
			deleted = true
		}
	}
	if !ended || !deleted {
		t.Errorf("calls = %+v, want /End and /Delete", calls)
	}
}

func TestSchtasksUnload_MissingTaskIsNotAnError(t *testing.T) {
	runner := func(name string, args ...string) ([]byte, error) {
		return []byte("ERROR: The system cannot find the file specified."), errors.New("exit status 1")
	}
	if err := schtasksUnload(runner); err != nil {
		t.Errorf("schtasksUnload on missing task err = %v, want nil", err)
	}
}

func TestSchtasksStatus_RunningTask(t *testing.T) {
	runner := func(name string, args ...string) ([]byte, error) {
		return []byte("TaskName: \\claude-usage-agent\r\nStatus:        Running\r\n"), nil
	}
	info, err := schtasksStatus(runner)
	if err != nil {
		t.Fatalf("schtasksStatus err = %v", err)
	}
	if !info.Loaded {
		t.Error("expected Loaded=true")
	}
	if info.State != "running" {
		t.Errorf("State = %q, want running", info.State)
	}
}

func TestSchtasksStatus_TaskNotRegistered(t *testing.T) {
	runner := func(name string, args ...string) ([]byte, error) {
		return []byte("ERROR: The system cannot find the file specified."), errors.New("exit status 1")
	}
	info, err := schtasksStatus(runner)
	if err != nil {
		t.Fatalf("schtasksStatus err = %v", err)
	}
	if info.Loaded {
		t.Error("expected Loaded=false for unregistered task")
	}
	if info.State != "not loaded" {
		t.Errorf("State = %q, want 'not loaded'", info.State)
	}
}

func TestSchtasksStatus_RealErrorSurfaced(t *testing.T) {
	runner := func(name string, args ...string) ([]byte, error) {
		return []byte("ERROR: Access is denied."), errors.New("exit status 1")
	}
	if _, err := schtasksStatus(runner); err == nil {
		t.Fatal("schtasksStatus err = nil, want error for non-not-found failure")
	}
}
