package main

// Linux service management via a systemd user unit. Kept free of build tags so
// the logic is unit-tested on every platform; the thin platform entry points
// live in service_linux.go.

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// systemdUnit is the systemd user unit the agent registers under.
const systemdUnit = "claude-usage-agent.service"

// systemdUnitPath returns $XDG_CONFIG_HOME/systemd/user/<unit> (XDG_CONFIG_HOME
// defaulting to ~/.config), where `systemctl --user` picks up user units.
func systemdUnitPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "systemd", "user", systemdUnit), nil
}

var (
	// Quoted unit-file strings take C escapes, and "%" starts a specifier.
	systemdEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, `%`, `%%`)
	// ExecStart= also expands "$VAR", so a literal "$" is doubled there.
	systemdExecEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, `%`, `%%`, `$`, `$$`)
)

func systemdExecArg(s string) string { return `"` + systemdExecEscaper.Replace(s) + `"` }

func systemdEnvAssignment(s string) string { return `"` + systemdEscaper.Replace(s) + `"` }

// renderSystemdUnit builds the unit file for the daemon. executable is the
// absolute path to this binary; args are the user's effective flags appended
// after `daemon`. pathEnv, when set, is baked in as PATH: the user manager's
// default PATH lacks ~/.local/bin, where the Claude CLI usually lives, and the
// token refresh has to find `claude`.
func renderSystemdUnit(executable string, args []string, pathEnv string) string {
	words := []string{systemdExecArg(executable), "daemon"}
	for _, a := range args {
		words = append(words, systemdExecArg(a))
	}

	var b strings.Builder
	fmt.Fprintln(&b, "[Unit]")
	fmt.Fprintln(&b, "Description=Claude Code usage collector")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "[Service]")
	fmt.Fprintf(&b, "ExecStart=%s\n", strings.Join(words, " "))
	if pathEnv != "" {
		fmt.Fprintf(&b, "Environment=%s\n", systemdEnvAssignment("PATH="+pathEnv))
	}
	fmt.Fprintln(&b, "Restart=always")
	fmt.Fprintln(&b, "RestartSec=10")
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "[Install]")
	fmt.Fprintln(&b, "WantedBy=default.target")
	return b.String()
}

// systemdInstall writes the unit, enables it for future logins, and restarts
// it so a new binary or new flags take effect right away. The unit is written
// 0600 because ExecStart carries the push token.
func systemdInstall(unitPath, executable string, args []string, pathEnv string, uid int, run shellRunner) error {
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return fmt.Errorf("create systemd user unit dir: %w", err)
	}
	if err := os.WriteFile(unitPath, []byte(renderSystemdUnit(executable, args, pathEnv)), 0o600); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}
	// WriteFile keeps the mode of an existing file; tighten a unit left by an
	// earlier install.
	if err := os.Chmod(unitPath, 0o600); err != nil {
		return fmt.Errorf("chmod unit: %w", err)
	}

	for _, step := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", systemdUnit},
		{"--user", "restart", systemdUnit},
	} {
		if out, err := run("systemctl", step...); err != nil {
			return fmt.Errorf("systemctl %s: %w: %s", strings.Join(step, " "), err, strings.TrimSpace(string(out)))
		}
	}
	log.Printf("install: wrote %s and started %s", unitPath, systemdUnit)

	// Without lingering the user manager, and the agent with it, only runs
	// while the user has a login session.
	out, err := run("loginctl", "show-user", strconv.Itoa(uid), "--property=Linger", "--value")
	if err == nil && strings.TrimSpace(string(out)) == "no" {
		log.Printf("install: lingering is off, so the agent stops at logout and won't start at boot until you log in; run `loginctl enable-linger` to keep it running")
	}
	return nil
}

// systemdUnload stops and disables the unit and deletes its file. A unit that
// is already gone is not an error for uninstall.
func systemdUnload(unitPath string, run shellRunner) error {
	if _, err := run("systemctl", "--user", "disable", "--now", systemdUnit); err != nil {
		log.Printf("uninstall: systemctl --user disable --now: %v", err)
	}
	if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove unit: %w", err)
	}
	if out, err := run("systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %w: %s", err, strings.TrimSpace(string(out)))
	}
	log.Printf("uninstall: removed %s", unitPath)
	return nil
}

// systemdStatus reports the unit's state from `systemctl --user show`, which
// prints LoadState=not-found (and exits 0) for a unit that isn't installed.
func systemdStatus(run shellRunner) (statusInfo, error) {
	info := statusInfo{Label: systemdUnit}
	out, err := run("systemctl", "--user", "show", systemdUnit,
		"--property=LoadState,ActiveState,SubState,MainPID,ExecMainStatus")
	if err != nil {
		return info, fmt.Errorf("systemctl --user show: %w: %s", err, strings.TrimSpace(string(out)))
	}
	props := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	if props["LoadState"] == "not-found" {
		info.State = "not loaded"
		return info, nil
	}
	info.Loaded = true

	if pid := props["MainPID"]; pid != "" && pid != "0" {
		info.PID = pid
	} else {
		info.LastExit = props["ExecMainStatus"]
	}
	switch {
	case info.PID != "":
		info.State = "running"
	case props["ActiveState"] == "failed" || props["SubState"] == "auto-restart":
		info.State = "crashed (last exit " + info.LastExit + ")"
	default:
		info.State = "loaded (not running)"
	}
	return info, nil
}
