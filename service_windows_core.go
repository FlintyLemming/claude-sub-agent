package main

// Windows service management via Task Scheduler (schtasks). Kept free of
// build tags so the logic is unit-tested on every platform; the thin
// platform entry points live in service_windows.go.

import (
	"fmt"
	"log"
	"strings"
)

// taskName is the Task Scheduler task the agent registers under.
const taskName = "claude-usage-agent"

// taskCommand builds the schtasks /TR value: quoted executable, the daemon
// subcommand, then the effective flags. Note schtasks caps /TR at 261 chars;
// the default flag set stays well under it.
func taskCommand(executable string, args []string) string {
	parts := []string{`"` + executable + `"`, "daemon"}
	parts = append(parts, args...)
	return strings.Join(parts, " ")
}

// schtasksInstall registers a logon task for the daemon and starts it
// immediately. /F overwrites an existing task with the same name, so
// re-installing with new flags takes effect cleanly.
func schtasksInstall(executable string, args []string, run shellRunner) error {
	// Stop a running instance first so the freshly installed binary/flags take
	// over; best-effort, the task may simply not exist yet.
	if _, err := run("schtasks", "/End", "/TN", taskName); err != nil {
		log.Printf("install: schtasks /End (pre-existing?): %v", err)
	}

	tr := taskCommand(executable, args)
	if out, err := run("schtasks", "/Create", "/F", "/SC", "ONLOGON", "/TN", taskName, "/TR", tr); err != nil {
		return fmt.Errorf("schtasks /Create: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := run("schtasks", "/Run", "/TN", taskName); err != nil {
		return fmt.Errorf("schtasks /Run: %w: %s", err, strings.TrimSpace(string(out)))
	}
	log.Printf("install: registered and started scheduled task %s", taskName)
	return nil
}

// schtasksUnload stops and deletes the scheduled task. A missing task is not
// an error for uninstall.
func schtasksUnload(run shellRunner) error {
	if _, err := run("schtasks", "/End", "/TN", taskName); err != nil {
		log.Printf("uninstall: schtasks /End: %v", err)
	}
	if out, err := run("schtasks", "/Delete", "/F", "/TN", taskName); err != nil {
		msg := strings.ToLower(string(out) + err.Error())
		if !strings.Contains(msg, "cannot find") {
			return fmt.Errorf("schtasks /Delete: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	log.Printf("uninstall: removed scheduled task %s", taskName)
	return nil
}

// schtasksStatus queries Task Scheduler for the task's state. A query error
// whose output says the task doesn't exist reports not-loaded; other errors
// surface to the caller.
func schtasksStatus(run shellRunner) (statusInfo, error) {
	info := statusInfo{Label: taskName}
	out, err := run("schtasks", "/Query", "/TN", taskName, "/FO", "LIST")
	if err != nil {
		msg := strings.ToLower(string(out) + err.Error())
		if strings.Contains(msg, "cannot find") {
			info.Loaded = false
			info.State = "not loaded"
			return info, nil
		}
		return info, fmt.Errorf("schtasks /Query: %w: %s", err, strings.TrimSpace(string(out)))
	}
	info.Loaded = true

	status := firstMatch(string(out), `Status:\s+(\S+)`)
	switch status {
	case "Running":
		info.State = "running"
	case "":
		info.State = "loaded"
	default:
		info.State = strings.ToLower(status)
	}
	return info, nil
}
