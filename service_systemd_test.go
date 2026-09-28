package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRenderSystemdUnit_ContainsRequiredKeys(t *testing.T) {
	got := renderSystemdUnit("/home/jane/.local/bin/claude-usage-agent", []string{
		"--push-url=http://localhost:8000/api/push/v2/claude-personal",
		"--interval=5m0s",
	}, "/usr/bin:/home/jane/.local/bin")

	mustContain := []string{
		"[Service]",
		`ExecStart="/home/jane/.local/bin/claude-usage-agent" daemon ` +
			`"--push-url=http://localhost:8000/api/push/v2/claude-personal" "--interval=5m0s"`,
		`Environment="PATH=/usr/bin:/home/jane/.local/bin"`,
		"Restart=always",
		"[Install]",
		"WantedBy=default.target",
	}
	for _, want := range mustContain {
		if !strings.Contains(got, want) {
			t.Errorf("unit missing %q\n--- unit ---\n%s", want, got)
		}
	}
}

// TestRenderSystemdUnit_EscapesSpecialCharacters: a push token may contain
// characters systemd would otherwise interpret — "$" (env expansion in
// ExecStart), "%" (specifiers), quotes and backslashes.
func TestRenderSystemdUnit_EscapesSpecialCharacters(t *testing.T) {
	got := renderSystemdUnit("/opt/my agent/claude-usage-agent",
		[]string{`--push-token=a$b%c"d\e`}, `/bin:/weird%dir$x`)

	for _, want := range []string{
		`ExecStart="/opt/my agent/claude-usage-agent" daemon "--push-token=a$$b%%c\"d\\e"`,
		// Environment= does no "$" expansion, so only "%" is doubled there.
		`Environment="PATH=/bin:/weird%%dir$x"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("unit missing %q\n--- unit ---\n%s", want, got)
		}
	}
}

func TestRenderSystemdUnit_OmitsEmptyPath(t *testing.T) {
	if got := renderSystemdUnit("/bin/agent", nil, ""); strings.Contains(got, "Environment=") {
		t.Errorf("unit has an Environment= line with no PATH to bake:\n%s", got)
	}
}

func TestSystemdInstall_WritesUnitAndStartsIt(t *testing.T) {
	unitPath := filepath.Join(t.TempDir(), "systemd", "user", systemdUnit)
	var calls []shellCall
	runner := func(name string, args ...string) ([]byte, error) {
		calls = append(calls, shellCall{name: name, args: args})
		if name == "loginctl" {
			return []byte("no\n"), nil
		}
		return nil, nil
	}

	if err := systemdInstall(unitPath, "/bin/agent", []string{"--interval=5m0s"}, "/usr/bin", 1000, runner); err != nil {
		t.Fatalf("systemdInstall err = %v", err)
	}

	content, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("unit not written: %v", err)
	}
	if !strings.Contains(string(content), `"--interval=5m0s"`) {
		t.Errorf("unit missing flags:\n%s", content)
	}
	if runtime.GOOS != "windows" { // Windows file modes don't carry Unix permissions.
		if fi, err := os.Stat(unitPath); err == nil && fi.Mode().Perm() != 0o600 {
			t.Errorf("unit mode = %v, want 0600 (it carries the push token)", fi.Mode().Perm())
		}
	}

	var systemctl []string
	for _, c := range calls {
		if c.name == "systemctl" {
			systemctl = append(systemctl, strings.Join(c.args, " "))
		}
	}
	want := []string{
		"--user daemon-reload",
		"--user enable " + systemdUnit,
		"--user restart " + systemdUnit,
	}
	if strings.Join(systemctl, "|") != strings.Join(want, "|") {
		t.Errorf("systemctl calls = %q, want %q", systemctl, want)
	}
	if last := calls[len(calls)-1]; last.name != "loginctl" || !strings.Contains(strings.Join(last.args, " "), "show-user 1000") {
		t.Errorf("last call = %+v, want the loginctl linger check", last)
	}
}

func TestSystemdInstall_StopsOnSystemctlError(t *testing.T) {
	unitPath := filepath.Join(t.TempDir(), systemdUnit)
	var restarted bool
	runner := func(name string, args ...string) ([]byte, error) {
		if name == "systemctl" && len(args) > 1 {
			switch args[1] {
			case "enable":
				return []byte("Failed to connect to bus"), errors.New("exit status 1")
			case "restart":
				restarted = true
			}
		}
		return nil, nil
	}

	err := systemdInstall(unitPath, "/bin/agent", nil, "", 1000, runner)
	if err == nil || !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Errorf("systemdInstall err = %v, want the enable failure with its output", err)
	}
	if restarted {
		t.Error("restart ran after enable failed")
	}
}

func TestSystemdUnload_RemovesUnit(t *testing.T) {
	unitPath := filepath.Join(t.TempDir(), systemdUnit)
	if err := os.WriteFile(unitPath, []byte("[Service]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls []string
	runner := func(name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}

	if err := systemdUnload(unitPath, runner); err != nil {
		t.Fatalf("systemdUnload err = %v", err)
	}
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Errorf("unit still present after uninstall (stat err = %v)", err)
	}
	want := []string{
		"systemctl --user disable --now " + systemdUnit,
		"systemctl --user daemon-reload",
	}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Errorf("calls = %q, want %q", calls, want)
	}
}

func TestSystemdUnload_MissingUnitIsNotAnError(t *testing.T) {
	runner := func(name string, args ...string) ([]byte, error) {
		if len(args) > 1 && args[1] == "disable" {
			return nil, errors.New("Unit file claude-usage-agent.service does not exist")
		}
		return nil, nil
	}
	if err := systemdUnload(filepath.Join(t.TempDir(), systemdUnit), runner); err != nil {
		t.Errorf("systemdUnload err = %v, want nil for an absent unit", err)
	}
}

func TestSystemdStatus(t *testing.T) {
	cases := []struct {
		name, out  string
		wantLoaded bool
		wantState  string
		wantPID    string
	}{
		{
			name:       "running",
			out:        "LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=4242\nExecMainStatus=0\n",
			wantLoaded: true, wantState: "running", wantPID: "4242",
		},
		{
			name:      "not installed",
			out:       "LoadState=not-found\nActiveState=inactive\nSubState=dead\nMainPID=0\nExecMainStatus=0\n",
			wantState: "not loaded",
		},
		{
			name:       "restarting after a crash",
			out:        "LoadState=loaded\nActiveState=activating\nSubState=auto-restart\nMainPID=0\nExecMainStatus=2\n",
			wantLoaded: true, wantState: "crashed (last exit 2)",
		},
		{
			name:       "stopped",
			out:        "LoadState=loaded\nActiveState=inactive\nSubState=dead\nMainPID=0\nExecMainStatus=0\n",
			wantLoaded: true, wantState: "loaded (not running)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := func(name string, args ...string) ([]byte, error) { return []byte(tc.out), nil }
			info, err := systemdStatus(runner)
			if err != nil {
				t.Fatalf("systemdStatus err = %v", err)
			}
			if info.Loaded != tc.wantLoaded || info.State != tc.wantState || info.PID != tc.wantPID {
				t.Errorf("status = %+v, want loaded=%v state=%q pid=%q", info, tc.wantLoaded, tc.wantState, tc.wantPID)
			}
		})
	}
}
