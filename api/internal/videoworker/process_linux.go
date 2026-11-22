//go:build linux

package videoworker

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// configureMediaProcess controls only this tracked child and its process group.
// It is not containment: hostile code may escape the group or access API mounts.
func configureMediaProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killMediaGroup(cmd) }
}
func killMediaGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
