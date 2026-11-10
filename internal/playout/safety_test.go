package playout

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

// safetyStep is one heartbeat after the clock moved by 5 s; report nil is a
// heartbeat without the field.
type safetyStep struct {
	report *bool
}

var (
	reportOn  = safetyStep{report: new(true)}
	reportOff = safetyStep{report: new(false)}
	noReport  = safetyStep{}
)

func TestSafetyHeartbeats(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		steps     []safetyStep
		wantOnAir bool
		// wantSinceStep is the step that turned it on (0-based); -1: not on air.
		wantSinceStep int
		// wantEvents are the on_air values of the events, in order.
		wantEvents []bool
	}{
		{name: "one true heartbeat", steps: []safetyStep{reportOn}, wantSinceStep: -1},
		{
			name: "two true heartbeats", steps: []safetyStep{reportOn, reportOn},
			wantOnAir: true, wantSinceStep: 1, wantEvents: []bool{true},
		},
		{
			name: "more true heartbeats", steps: []safetyStep{reportOn, reportOn, reportOn, reportOn},
			wantOnAir: true, wantSinceStep: 1, wantEvents: []bool{true},
		},
		{
			name: "a false heartbeat turns it off at once", steps: []safetyStep{reportOn, reportOn, reportOff},
			wantSinceStep: -1, wantEvents: []bool{true, false},
		},
		{name: "true false true", steps: []safetyStep{reportOn, reportOff, reportOn}, wantSinceStep: -1},
		{
			name: "on again needs two after a false", steps: []safetyStep{reportOn, reportOn, reportOff, reportOn},
			wantSinceStep: -1, wantEvents: []bool{true, false},
		},
		{name: "no field when off", steps: []safetyStep{noReport, noReport}, wantSinceStep: -1},
		{
			name: "no field when on", steps: []safetyStep{reportOn, reportOn, noReport, noReport},
			wantOnAir: true, wantSinceStep: 1, wantEvents: []bool{true},
		},
		{
			name: "no field does not break the run", steps: []safetyStep{reportOn, noReport, reportOn},
			wantOnAir: true, wantSinceStep: 2, wantEvents: []bool{true},
		},
		{name: "false when already off sends nothing", steps: []safetyStep{reportOff, reportOff}, wantSinceStep: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			var since time.Time
			for i, step := range tt.steps {
				h.now = h.now.Add(5 * time.Second)
				if i == tt.wantSinceStep {
					since = h.now
				}
				require.NoError(t, h.svc.Callback(t.Context(), Event{Kind: EventHeartbeat, Time: h.now, SafetyOnAir: step.report}))
			}

			st, err := h.svc.Status(t.Context())
			require.NoError(t, err)
			require.Equal(t, tt.wantOnAir, st.SafetyOnAir)
			if tt.wantSinceStep >= 0 {
				require.NotNil(t, st.SafetySince)
				require.Equal(t, since, *st.SafetySince)
			} else {
				require.Nil(t, st.SafetySince)
			}

			var got []bool
			for _, e := range h.pub.events {
				ev, ok := e.(ws.SafetyEvent)
				require.True(t, ok, "only safety events: %T", e)
				require.Equal(t, ws.EventSafety, ev.Type)
				require.Equal(t, ev.Data.OnAir, ev.Data.Since != nil, "since only while on air")
				got = append(got, ev.Data.OnAir)
			}
			require.Equal(t, tt.wantEvents, got)
		})
	}
}

func TestSafetyExpire(t *testing.T) {
	t.Parallel()
	const timeout = 10 * time.Second
	tests := []struct {
		name       string
		steps      []safetyStep
		wait       time.Duration
		linkDown   bool
		after      []safetyStep
		wantOnAir  bool
		wantEvents []bool
	}{
		{
			name: "silent past the timeout resets it", steps: []safetyStep{reportOn, reportOn}, wait: timeout + time.Second,
			wantEvents: []bool{true, false},
		},
		{
			name: "the link down does not stop the reset", steps: []safetyStep{reportOn, reportOn}, wait: timeout + time.Second,
			linkDown: true, wantEvents: []bool{true, false},
		},
		{
			name: "exactly the timeout is not silent", steps: []safetyStep{reportOn, reportOn}, wait: timeout,
			wantOnAir: true, wantEvents: []bool{true},
		},
		{name: "silence when off sends nothing", steps: []safetyStep{reportOff}, wait: timeout + time.Second},
		{name: "no heartbeat yet sends nothing", wait: timeout + time.Second},
		{
			name: "a report before the silence does not count", steps: []safetyStep{reportOn}, wait: timeout + time.Second,
			after: []safetyStep{reportOn},
		},
		{
			name: "on air again after the silence", steps: []safetyStep{reportOn, reportOn}, wait: timeout + time.Second,
			after: []safetyStep{reportOn, reportOn}, wantOnAir: true, wantEvents: []bool{true, false, true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.set.HeartbeatTimeout = timeout
			beat := func(steps []safetyStep) {
				for _, step := range steps {
					h.now = h.now.Add(time.Second)
					require.NoError(t, h.svc.Callback(t.Context(), Event{Kind: EventHeartbeat, Time: h.now, SafetyOnAir: step.report}))
				}
			}
			beat(tt.steps)

			h.now = h.now.Add(tt.wait)
			h.ctl.down = tt.linkDown
			err := h.svc.pass(t.Context())
			if tt.linkDown {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			_ = h.svc.pass(t.Context()) // a second pass sends nothing more
			beat(tt.after)

			st, err := h.svc.Status(t.Context())
			require.NoError(t, err)
			require.Equal(t, tt.wantOnAir, st.SafetyOnAir)
			var got []bool
			for _, e := range h.pub.events {
				if ev, ok := e.(ws.SafetyEvent); ok {
					got = append(got, ev.Data.OnAir)
				}
			}
			require.Equal(t, tt.wantEvents, got)
		})
	}
}
