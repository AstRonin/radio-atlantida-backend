//go:build unix

package media

import (
	"context"
	"errors"
	"os/exec"
	"syscall"
)

// commandContext returns a command that runs in its own process group, so
// cancelling ctx kills the command and every process it started. Setpgid and
// kill(-pgid) exist on Linux (the Docker image) and macOS (development). The
// build tag keeps the package compiling on other systems (commandContext in
// proc_other.go).
func commandContext(ctx context.Context, bin string, args ...string) *exec.Cmd {
	//nolint:gosec // G204: bin is ffmpeg or ffprobe from PATH; callers pass paths validated inside MEDIA_ROOT, one argument each (no shell).
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = processWaitDelay
	return cmd
}

// killGroup sends SIGKILL to the process group of cmd. The child leads its
// group (Setpgid with pgid 0), so its PID is the group ID. A group that is gone
// already is not an error.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
