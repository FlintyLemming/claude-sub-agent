package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// Platform entry points delegating to the portable schtasks implementation in
// service_windows_core.go.

func runInstall(args []string, run shellRunner) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return fmt.Errorf("resolve executable symlink: %w", err)
	}
	return schtasksInstall(executable, args, run)
}

func runUnload(run shellRunner) error { return schtasksUnload(run) }

func runStatus(run shellRunner) (statusInfo, error) { return schtasksStatus(run) }
