//go:build unix

package media

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The runner of the media tools kills the child and the processes it started
// when its context ends: a grandchild of a cancelled tool does not survive it.
func TestRunCancelKillsTheChildAndItsDescendants(t *testing.T) {
	t.Parallel()
	for _, tool := range []string{"sh", "sleep"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	pidFile := filepath.Join(t.TempDir(), "pid")
	f := &FFmpeg{bin: "sh"} // the runner of ffmpeg: f.run starts f.bin with args
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := f.run(ctx, "-c", `sleep 60 & echo $! > "$0"; wait`, pidFile)
		done <- err
	}()

	var pid int
	require.Eventually(t, func() bool {
		b, err := os.ReadFile(pidFile) //nolint:gosec // G304: the test's own temporary file
		if err != nil || len(strings.TrimSpace(string(b))) == 0 {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil
	}, 10*time.Second, 20*time.Millisecond, "the tool started its descendant")

	cancel()
	select {
	case err := <-done:
		require.Error(t, err, "a cancelled run fails")
	case <-time.After(15 * time.Second):
		require.FailNow(t, "the run did not return after cancel")
	}
	require.Eventually(t, func() bool { return !processAlive(pid) }, 10*time.Second, 50*time.Millisecond,
		"descendant %d is gone after cancel", pid)
}

// processAlive reports whether pid runs. A zombie has ended; it is not alive.
func processAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return !errors.Is(err, syscall.ESRCH)
	}
	//nolint:gosec // G204: a fixed tool with a number argument.
	out, err := exec.CommandContext(context.Background(), "ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	s := strings.TrimSpace(string(out))
	return s != "" && !strings.HasPrefix(s, "Z")
}
