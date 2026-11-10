package playout

import (
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

// safetyReports is how many heartbeats in a row must report the safety tone
// before it counts as on air: a short gap between tracks raises no warning.
const safetyReports = 2

// safetyState is whether the safety tone is on air, from the heartbeats of
// Liquidsoap. It lives in memory: the heartbeat repeats it every 5 s, so it
// heals by itself after a restart. Service.mu guards it.
type safetyState struct {
	onAir bool
	// onAirSince is when it was counted on air; zero when it is not.
	onAirSince time.Time
	// reports counts the heartbeats in a row that reported the tone.
	reports int
	// lastHeartbeat is when the last heartbeat arrived; zero when none has
	// arrived since the last reset.
	lastHeartbeat time.Time
}

func (st *safetyState) since() *time.Time {
	if st.onAirSince.IsZero() {
		return nil
	}
	return new(st.onAirSince)
}

// safetyHeartbeat records one heartbeat received at at. A nil report changes
// nothing (the script does not report it) but still counts as a heartbeat.
func (s *Service) safetyHeartbeat(at time.Time, report *bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.safety.lastHeartbeat = at
	switch {
	case report == nil:
	case !*report:
		s.safety.reports = 0
		s.setSafetyLocked(false, at)
	default:
		s.safety.reports = min(s.safety.reports+1, safetyReports)
		if s.safety.reports == safetyReports {
			s.setSafetyLocked(true, at)
		}
	}
}

// safetyExpire resets the state when heartbeats have been silent longer
// than timeout; the outage banner covers that case. The partial count goes
// too: a report before the silence and one after it are not in a row.
func (s *Service) safetyExpire(now time.Time, timeout time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.safety.lastHeartbeat.IsZero() || now.Sub(s.safety.lastHeartbeat) <= timeout {
		return
	}
	s.safety.lastHeartbeat = time.Time{}
	s.safety.reports = 0
	s.setSafetyLocked(false, now)
}

// setSafetyLocked changes the state and, only when it changed, sends
// playout.safety. It publishes under mu (Publish does not block) so that
// the events leave in the order of the changes.
func (s *Service) setSafetyLocked(onAir bool, at time.Time) {
	if s.safety.onAir == onAir {
		return
	}
	s.safety.onAir = onAir
	s.safety.onAirSince = time.Time{}
	if onAir {
		s.safety.onAirSince = at
		s.log.Warn("safety tone on air: the plan and the emergency playlist have nothing to play")
	} else {
		s.log.Info("safety tone off air")
	}
	s.pub.Publish(ws.SafetyEvent{
		Type: ws.EventSafety, Time: at,
		Data: ws.SafetyData{OnAir: onAir, Since: s.safety.since()},
	})
}
