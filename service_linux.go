package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// Platform entry points delegating to the portable systemd implementation in
// service_systemd.go.

func runInstall(args []string, run shellRunner) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return fmt.Errorf("resolve executable symlink: %w", err)
	}
	unitPath, err := systemdUnitPath()
	if err != nil {
		return err
	}
	return systemdInstall(unitPath, executable, args, os.Getenv("PATH"), os.Getuid(), run)
}

func runUnload(run shellRunner) error {
	unitPath, err := systemdUnitPath()
	if err != nil {
		return err
	}
	return systemdUnload(unitPath, run)
}

func runStatus(run shellRunner) (statusInfo, error) { return systemdStatus(run) }
