package playout

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/liquidsoap"
)

var devOutput = StreamOutput{
	Host: "icecast", Port: 8000, Mount: "/radio", Codec: "mp3", BitrateKbps: 128, SampleRateHz: 44100, Channels: 2,
	Name: "Radio Atlantida",
}

func TestStreamOutputCommand(t *testing.T) {
	t.Parallel()
	o := devOutput
	o.Description = "line\nbreak \"quoted\""
	cmd, err := o.Command()
	require.NoError(t, err)
	require.Equal(t, `output.set {"host":"icecast","port":8000,"mount":"/radio","tls":false,"codec":"mp3",`+
		`"bitrate_kbps":128,"sample_rate_hz":44100,"channels":2,"name":"Radio Atlantida",`+
		`"description":"line\nbreak \"quoted\"","genre":""}`, cmd)
	require.NotContains(t, cmd, "\n", "one telnet line")
}

// outputCommands returns the output.set commands the fake received.
func (c *fakeControl) outputCommands() []string {
	var out []string
	for _, cmd := range c.commands {
		if strings.HasPrefix(cmd, "output.set ") {
			out = append(out, cmd)
		}
	}
	return out
}

// newOutputHarness is a harness whose log is kept, at INFO.
func newOutputHarness(t *testing.T) (*harness, *bytes.Buffer) {
	t.Helper()
	h := newHarness(t)
	h.set.Output = devOutput
	var logs bytes.Buffer
	h.svc.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return h, &logs
}

func TestApplyOutput(t *testing.T) {
	t.Parallel()
	h, logs := newOutputHarness(t)
	want, err := devOutput.Command()
	require.NoError(t, err)

	// The first pass sends it once.
	require.NoError(t, h.svc.pass(t.Context()))
	require.Equal(t, []string{want}, h.ctl.outputCommands())
	require.Contains(t, logs.String(), "stream output applied")

	// Equal settings: nothing is sent.
	require.NoError(t, h.svc.pass(t.Context()))
	require.Len(t, h.ctl.outputCommands(), 1)

	// A changed setting sends the new output.
	h.set.Output.Codec, h.set.Output.Channels = "vorbis", 1
	require.NoError(t, h.svc.pass(t.Context()))
	require.Len(t, h.ctl.outputCommands(), 2)
	require.Contains(t, h.ctl.outputCommands()[1], `"codec":"vorbis"`)

	// A new connection (resync) sends it again; the script answers
	// unchanged and the output counts as applied.
	h.ctl.gen++
	h.ctl.reply = func(string) []string { return []string{"OK unchanged"} }
	require.NoError(t, h.svc.pass(t.Context()))
	require.Len(t, h.ctl.outputCommands(), 3)
	require.NoError(t, h.svc.pass(t.Context()))
	require.Len(t, h.ctl.outputCommands(), 3, "unchanged is applied: not sent again")
}

func TestApplyOutputRefused(t *testing.T) {
	t.Parallel()
	h, logs := newOutputHarness(t)
	h.svc.push.gen = 1 // no resync noise in the log
	h.ctl.reply = func(string) []string { return []string{"ERROR mount must start with /"} }

	// Refused: an ERROR (a panel alert), the pass still fills the queues.
	h.store = newFakeStore(item(1, 0))
	h.svc.store = h.store
	require.NoError(t, h.svc.pass(t.Context()))
	require.Equal(t, []uint64{1}, h.store.queued(), "the queues are filled anyway")
	require.Equal(t, 1, strings.Count(logs.String(), "level=ERROR"))
	require.Contains(t, logs.String(), "ERROR mount must start with /")

	// Retried at the next pass; the same refusal is not logged again
	// within retryLog.
	require.NoError(t, h.svc.pass(t.Context()))
	require.Len(t, h.ctl.outputCommands(), 2)
	require.Equal(t, 1, strings.Count(logs.String(), "level=ERROR"))

	// After retryLog it is logged again.
	h.now = h.now.Add(retryLog)
	require.NoError(t, h.svc.pass(t.Context()))
	require.Equal(t, 2, strings.Count(logs.String(), "level=ERROR"))

	// A script without the command (an older contract) is refused the
	// same way.
	h.ctl.reply = func(string) []string {
		return []string{`ERROR: unknown command, type "help" to get a list of commands.`}
	}
	require.NoError(t, h.svc.pass(t.Context()))
	require.Equal(t, 3, strings.Count(logs.String(), "level=ERROR"), "a new reply is logged at once")

	// Accepted at last: applied, and no more retries.
	h.ctl.reply = nil
	require.NoError(t, h.svc.pass(t.Context()))
	n := len(h.ctl.outputCommands())
	h.now = h.now.Add(time.Second)
	require.NoError(t, h.svc.pass(t.Context()))
	require.Len(t, h.ctl.outputCommands(), n)
}

func TestApplyOutputLinkDown(t *testing.T) {
	t.Parallel()
	h, _ := newOutputHarness(t)
	h.ctl.down = true
	require.ErrorIs(t, h.svc.applyOutput(t.Context(), h.set), liquidsoap.ErrUnavailable)
	require.Empty(t, h.svc.push.output, "retried when the link is back")
	h.ctl.down = false
	require.NoError(t, h.svc.applyOutput(t.Context(), h.set))
	require.Len(t, h.ctl.outputCommands(), 1)
}
