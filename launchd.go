package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// launchdLabel is the reverse-DNS label the agent registers under.
const launchdLabel = "com.user.claude-usage-agent"

// plistName is the on-disk filename under ~/Library/LaunchAgents.
const plistName = "com.user.claude-usage-agent.plist"

// logName is the redirect target under ~/Library/Logs.
const logName = "claude-usage-agent.log"

// plistPath returns ~/Library/LaunchAgents/<plistName>.
func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", plistName), nil
}

// logPath returns ~/Library/Logs/<logName>.
func logPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", logName), nil
}

// renderPlist builds the launchd plist XML for the daemon. executable is the
// absolute path to this binary; args are the user's effective flags appended
// after `daemon`. The plist sets KeepAlive + RunAtLoad and redirects stdio to
// the shared log file.
func renderPlist(executable string, args []string) (string, error) {
	lp, err := logPath()
	if err != nil {
		return "", err
	}
	progArgs := []string{executable, "daemon"}
	progArgs = append(progArgs, args...)

	var b strings.Builder
	fmt.Fprintln(&b, `<?xml version="1.0" encoding="UTF-8"?>`)
	fmt.Fprintln(&b, `<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">`)
	fmt.Fprintln(&b, `<plist version="1.0">`)
	fmt.Fprintln(&b, `<dict>`)
	fmt.Fprintf(&b, "    <key>Label</key>\n    <string>%s</string>\n", launchdLabel)
	fmt.Fprintln(&b, `    <key>ProgramArguments</key>`)
	fmt.Fprintln(&b, `    <array>`)
	for _, a := range progArgs {
		fmt.Fprintf(&b, "        <string>%s</string>\n", escapeXML(a))
	}
	fmt.Fprintln(&b, `    </array>`)
	// launchd starts agents with a minimal PATH (/usr/bin:/bin:...), which
	// breaks the keepalive/refresh `claude` invocations when the CLI lives in
	// e.g. /opt/homebrew/bin. Bake the installing user's PATH into the plist.
	if path := os.Getenv("PATH"); path != "" {
		fmt.Fprintln(&b, `    <key>EnvironmentVariables</key>`)
		fmt.Fprintln(&b, `    <dict>`)
		fmt.Fprintf(&b, "        <key>PATH</key>\n        <string>%s</string>\n", escapeXML(path))
		fmt.Fprintln(&b, `    </dict>`)
	}
	fmt.Fprintf(&b, "    <key>StandardOutPath</key>\n    <string>%s</string>\n", escapeXML(lp))
	fmt.Fprintf(&b, "    <key>StandardErrorPath</key>\n    <string>%s</string>\n", escapeXML(lp))
	fmt.Fprintln(&b, `    <key>KeepAlive</key>`)
	fmt.Fprintln(&b, `    <true/>`)
	fmt.Fprintln(&b, `    <key>RunAtLoad</key>`)
	fmt.Fprintln(&b, `    <true/>`)
	fmt.Fprintln(&b, `</dict>`)
	fmt.Fprintln(&b, `</plist>`)
	return b.String(), nil
}

// escapeXML quotes characters that are illegal in XML element text.
func escapeXML(s string) string {
	r := strings.NewReplacer(
		`&`, `&amp;`,
		`<`, `&lt;`,
		`>`, `&gt;`,
		`"`, `&#34;`,
		`'`, `&#39;`,
	)
	return r.Replace(s)
}

// launchctl is swappable so tests can avoid touching the real system.
type shellRunner func(name string, args ...string) ([]byte, error)

func realShell() shellRunner {
	return func(name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).Output()
	}
}

// runInstall writes the plist to ~/Library/LaunchAgents and loads it. If an
// agent with the same label is already loaded it is unloaded first so the new
// plist takes effect cleanly. The log dir is created if missing.
func runInstall(args []string, run shellRunner) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return fmt.Errorf("resolve executable symlink: %w", err)
	}

	content, err := renderPlist(executable, args)
	if err != nil {
		return err
	}

	pp, err := plistPath()
	if err != nil {
		return err
	}
	// Unload an existing agent first so we overwrite cleanly.
	if _, err := run("launchctl", "unload", pp); err != nil {
		// Unload failing usually just means it wasn't loaded; non-fatal.
		log.Printf("install: launchctl unload (pre-existing?) : %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(pp), 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents dir: %w", err)
	}
	if err := os.WriteFile(pp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write plist: %w", err)
	}
	if _, err := run("launchctl", "load", pp); err != nil {
		return fmt.Errorf("launchctl load: %w", err)
	}
	log.Printf("install: wrote %s and loaded %s", pp, launchdLabel)
	return nil
}

// runUnload deletes the plist and unloads the agent.
func runUnload(run shellRunner) error {
	pp, err := plistPath()
	if err != nil {
		return err
	}
	if _, err := run("launchctl", "unload", pp); err != nil {
		log.Printf("uninstall: launchctl unload: %v", err)
	}
	if err := os.Remove(pp); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove plist: %w", err)
	}
	log.Printf("uninstall: removed %s", pp)
	return nil
}

// status summarises what launchd reports for the agent.
type statusInfo struct {
	Label    string
	Loaded   bool
	PID      string // empty if not running
	LastExit string // empty if never exited
	State    string // human-readable, e.g. "running" / "exited"
}

// runStatus queries launchctl for the agent's state. When launchctl reports
// the service as unknown ("Could not find service"), the agent is reported as
// not-loaded. Any other launchctl error (missing binary, permission denied,
// broken launchd) is surfaced to the caller rather than masquerading as
// "not loaded".
func runStatus(run shellRunner) (statusInfo, error) {
	info := statusInfo{Label: launchdLabel}
	uid := os.Getuid()
	out, err := run("launchctl", "print", fmt.Sprintf("gui/%d/%s", uid, launchdLabel))
	if err != nil {
		msg := string(out) + err.Error()
		if strings.Contains(strings.ToLower(msg), "could not find service") {
			info.Loaded = false
			info.State = "not loaded"
			return info, nil
		}
		return info, fmt.Errorf("launchctl print: %w: %s", err, strings.TrimSpace(string(out)))
	}
	info.Loaded = true
	text := string(out)

	info.PID = firstMatch(text, `pid = (\d+)`)
	info.LastExit = firstMatch(text, `last exit code = (\S+)`)

	switch {
	case info.PID != "":
		info.State = "running"
	case info.LastExit != "" && info.LastExit != "0":
		info.State = "crashed (last exit " + info.LastExit + ")"
	default:
		info.State = "loaded (not running)"
	}
	return info, nil
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
