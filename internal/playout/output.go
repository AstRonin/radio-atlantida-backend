package playout

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
)

// StreamOutput is the Icecast output of the stream (the stream.* settings,
// §8.8) as Liquidsoap applies it: the argument of the script's output.set
// command and the body of GET /api/playout/output. It holds no secret: the
// source password stays in Liquidsoap's environment.
//
// The JSON encoding is part of the script contract
// (dev/liquidsoap/radio.liq): every field is always sent.
type StreamOutput struct {
	Host         string `json:"host"`
	Port         int    `json:"port"`
	Mount        string `json:"mount"`
	TLS          bool   `json:"tls"`
	Codec        string `json:"codec"`
	BitrateKbps  int    `json:"bitrate_kbps"`
	SampleRateHz int    `json:"sample_rate_hz"`
	Channels     int    `json:"channels"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Genre        string `json:"genre"`
}

// Replies of the script to output.set; anything else is a refusal
// ("ERROR <reason>") or a script without the command.
const (
	outputSetCommand = "output.set"
	outputApplied    = "OK applied"
	outputUnchanged  = "OK unchanged"
)

// Command returns the telnet command that applies the output:
// output.set <compact JSON>. JSON escapes line breaks, so it is one line.
func (o StreamOutput) Command() (string, error) {
	b, err := json.Marshal(o)
	if err != nil {
		return "", fmt.Errorf("encode stream output: %w", err)
	}
	return outputSetCommand + " " + string(b), nil
}

// Output returns the stream output the settings resolve to (saved values
// or defaults), for Liquidsoap's startup fetch.
func (s *Service) Output(ctx context.Context) (StreamOutput, error) {
	set, err := s.settings(ctx)
	if err != nil {
		return StreamOutput{}, fmt.Errorf("read playout settings: %w", err)
	}
	return set.Output, nil
}

// applyOutput sends the stream output to Liquidsoap after a reconnect or a
// settings change (§6.3: a reconnect, not a restart). The script answers
// "OK unchanged" when its output already equals it, so sending it on every
// new connection never interrupts the stream.
//
// A refusal does not fail the pass: the queues must still be filled, and
// the running output stays on air. It is logged as an ERROR (a panel alert)
// and retried at the next pass; the log repeats at most once per retryLog
// while the reply stays the same. A transport error fails the pass like
// every other command.
func (s *Service) applyOutput(ctx context.Context, set Settings) error {
	cmd, err := set.Output.Command()
	if err != nil {
		return err
	}
	if cmd == s.push.output {
		return nil
	}
	lines, err := s.ctl.Command(ctx, cmd)
	if err != nil {
		return fmt.Errorf("apply stream output: %w", err)
	}
	reply := strings.TrimSpace(strings.Join(lines, " "))
	switch reply {
	case outputApplied:
		s.log.Info("stream output applied", outputAttrs(set.Output)...)
	case outputUnchanged:
	default:
		now := s.now()
		if reply != s.push.outputErr || now.Sub(s.push.outputErrAt) >= retryLog {
			s.log.Error("stream output not applied",
				append(outputAttrs(set.Output), "reply", reply, logging.KeyError, "liquidsoap refused output.set: "+reply)...)
			s.push.outputErr, s.push.outputErrAt = reply, now
		}
		return nil
	}
	s.push.output = cmd
	s.push.outputErr, s.push.outputErrAt = "", time.Time{}
	return nil
}

func outputAttrs(o StreamOutput) []any {
	return []any{
		"host", o.Host, "port", o.Port, "mount", o.Mount, "tls", o.TLS, "codec", o.Codec,
		"bitrate_kbps", o.BitrateKbps, "sample_rate_hz", o.SampleRateHz, "channels", o.Channels,
	}
}
