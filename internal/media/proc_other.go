//go:build !unix

package media

import (
	"context"
	"os/exec"
)

// commandContext returns a command killed on ctx end. Without process groups
// (not a platform the backend runs on) only the direct child is killed.
func commandContext(ctx context.Context, bin string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = processWaitDelay
	return cmd
}
