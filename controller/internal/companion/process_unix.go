//go:build !windows

package companion

import (
	"os"
	"os/exec"
	"syscall"
)

func configureProcessCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func interruptProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// Signal the process group so an interactive Codex child/app-server gets
	// the same graceful shutdown request as its parent.
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
}

func terminateProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
}

func killProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

var _ os.Signal = syscall.SIGINT
