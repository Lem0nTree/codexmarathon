//go:build windows

package companion

import (
	"os"
	"os/exec"
)

// Windows does not expose a portable process-group Ctrl+C API through the Go
// standard library. Process.Signal(os.Interrupt) is still the least
// surprising graceful request for console-attached Codex; the timeout path
// always escalates to Process.Kill so auth replacement cannot race a live
// process.
func configureProcessCommand(_ *exec.Cmd) {}

func interruptProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Signal(os.Interrupt)
}

func terminateProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func killProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
