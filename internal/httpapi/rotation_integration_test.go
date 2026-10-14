//go:build integration

package httpapi_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/grid"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/errcodes"
)

// waitOperation waits for an operation to finish and expects it completed.
func (a *libraryAPI) waitOperation(id *uint64) dto.Operation {
	a.t.Helper()
	require.NotNil(a.t, id, "the response names its build operation")
	var op dto.Operation
	require.Eventually(a.t, func() bool {
		a.ok(http.StatusOK, http.MethodGet, fmt.Sprintf("/api/operations/%d", *id), nil, &op)
		return op.Status != "running"
	}, 10*time.Second, 20*time.Millisecond)
	require.Equal(a.t, "completed", op.Status, "%+v", op)
	return op
}

// tomorrowNoon is 12:00 station time tomorrow: always within the 48-hour
// plan horizon, and never started.
func tomorrowNoon(t *testing.T) (date string, weekday int, start time.Time) {
	t.Helper()
	loc := stationTZ(t)
	d := time.Now().In(loc).AddDate(0, 0, 1)
	start = time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, loc).UTC()
	return d.Format(time.DateOnly), grid.ISOWeekday(d), start
}

func (a *libraryAPI) plan() dto.TrackPlan {
	a.t.Helper()
	from := time.Now().UTC().Truncate(time.Hour)
	q := fmt.Sprintf("/api/track-plan?from=%s&to=%s", from.Format(time.RFC3339), from.Add(72*time.Hour).Format(time.RFC3339))
	var p dto.TrackPlan
	a.ok(http.StatusOK, http.MethodGet, q, nil, &p)
	return p
}

// filler adds a 3-minute track in no playlist: the best random filler for
// the 3 minutes a 3 × 19 minute clock leaves.
func (f *clockFixture) filler() uint64 {
	return f.exec(`INSERT INTO tracks (title, file_path, type_id, duration_ms) VALUES ('Filler', 'filler/f.mp3', $1, 180000) RETURNING id`,
		f.typeID("music"))
}

func TestTrackPlanFollowsGridAndClock(t *testing.T) {
	f := newClockFixture(t)
	filler := f.filler()

	var saved dto.ClockSaveResult
	f.ok(http.StatusCreated, http.MethodPost, "/api/clocks", map[string]any{
		"name": "Day",
		"elements": []any{
			element(f.music, 19), element(f.music, 19), element(f.music, 19, "filler_mode", "random"),
		},
	}, &saved)
	require.Equal(t, "complete", saved.Clock.Status)
	f.waitOperation(saved.BuildOperationID)
	require.Empty(t, f.plan().Items, "the clock is not in the grid yet")

	date, weekday, start := tomorrowNoon(t)
	var assigned dto.GridAssignResult
	f.ok(http.StatusOK, http.MethodPost, "/api/grid/assign", map[string]any{
		"clock_id": saved.Clock.ID, "weekdays": []int{weekday}, "hours": []int{12}, "valid_from": date, "valid_to": date,
	}, &assigned)
	op := f.waitOperation(assigned.BuildOperationID)
	require.Contains(t, op.ResultSummary, "built 1 hours")

	p := f.plan()
	require.Len(t, p.Items, 1)
	h := p.Items[0]
	require.Equal(t, start, h.HourStartUTC)
	require.Equal(t, saved.Clock.ID, h.Clock.ID)
	require.Equal(t, assigned.Created[0].ID, *h.GridSlotID)
	require.Len(t, h.Items, 4)
	for i, it := range h.Items[:3] {
		require.Equal(t, "clock", it.Source)
		require.Equal(t, "planned", it.Status)
		require.Equal(t, saved.Clock.Elements[i].ID, *it.ElementID)
		require.Equal(t, start.Add(time.Duration(i)*19*time.Minute), it.PlannedStartUTC)
	}
	require.Equal(t, filler, h.Items[3].TrackID)
	require.Equal(t, "filler_random", h.Items[3].Source)
	require.False(t, h.Items[3].Truncated, "3:00 closes the gap exactly")

	// A rebuild of a current hour is a no-op; a clock change rebuilds it.
	var again dto.ClockSaveResult
	f.ok(http.StatusOK, http.MethodPost, fmt.Sprintf("/api/clocks/%d/save-incomplete", saved.Clock.ID), nil, &again)
	op = f.waitOperation(again.BuildOperationID)
	require.Contains(t, op.ResultSummary, "built 0 hours")
	elements := make([]any, len(saved.Clock.Elements))
	for i, e := range saved.Clock.Elements {
		elements[i] = element(f.music, 19, "id", e.ID, "filler_mode", e.FillerMode)
	}
	f.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/clocks/%d", saved.Clock.ID), map[string]any{
		"name": "Day 2", "elements": elements,
	}, &again)
	op = f.waitOperation(again.BuildOperationID)
	require.Contains(t, op.ResultSummary, "built 1 hours")
	require.True(t, f.plan().Items[0].BuiltAt.After(h.BuiltAt))

	// Clearing the grid clears the plan, so the clock can be deleted.
	f.ok(http.StatusOK, http.MethodPost, "/api/grid/unassign", map[string]any{
		"weekdays": []int{weekday}, "hours": []int{12}, "valid_from": date, "valid_to": date,
	}, &assigned)
	op = f.waitOperation(assigned.BuildOperationID)
	require.Contains(t, op.ResultSummary, "cleared 1")
	require.Empty(t, f.plan().Items)
	f.ok(http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/api/clocks/%d", saved.Clock.ID), nil, nil)
}

func TestFillerReminders(t *testing.T) {
	f := newClockFixture(t)
	filler := f.filler()

	// Short with holder none: incomplete, so the hour waits for a reminder.
	var saved dto.ClockSaveResult
	f.ok(http.StatusCreated, http.MethodPost, "/api/clocks", map[string]any{
		"name":     "Short",
		"elements": []any{element(f.music, 19), element(f.music, 19), element(f.music, 19)},
	}, &saved)
	require.Equal(t, "incomplete", saved.Clock.Status)
	date, weekday, start := tomorrowNoon(t)
	var assigned dto.GridAssignResult
	f.ok(http.StatusOK, http.MethodPost, "/api/grid/assign", map[string]any{
		"clock_id": saved.Clock.ID, "weekdays": []int{weekday}, "hours": []int{12}, "valid_from": date, "valid_to": date,
	}, &assigned)
	f.waitOperation(assigned.BuildOperationID)

	var reqs dto.FillerRequestList
	f.ok(http.StatusOK, http.MethodGet, "/api/filler-requests", nil, &reqs)
	require.Len(t, reqs.Items, 1)
	r := reqs.Items[0]
	require.Equal(t, "short_clock", r.Reason)
	require.Equal(t, "none", r.Mode)
	require.Equal(t, "pending", r.Status)
	require.Equal(t, start, r.HourStartUTC)
	require.Equal(t, start.Add(-2*time.Hour), r.DeadlineAt)
	h := f.plan().Items[0]
	require.Len(t, h.Items, 3, "the gap waits")
	require.Equal(t, r.ID, *h.FillerRequestID)

	answer := fmt.Sprintf("/api/filler-requests/%d/answer", r.ID)
	// A track planned in the same hour breaks separation; nothing changes.
	f.fieldError("track_id", errcodes.Blocked, http.MethodPost, answer, map[string]any{"track_id": h.Items[0].TrackID})
	f.fieldError("track_id", errcodes.UnknownID, http.MethodPost, answer, map[string]any{"track_id": 999999})
	f.problem(http.StatusNotFound, errcodes.FillerRequestNotFound, http.MethodPost, "/api/filler-requests/999999/answer",
		map[string]any{"track_id": filler})
	require.Len(t, f.plan().Items[0].Items, 3)

	var answered dto.FillerRequest
	f.ok(http.StatusOK, http.MethodPost, answer, map[string]any{"track_id": filler}, &answered)
	require.Equal(t, "answered", answered.Status)
	require.Equal(t, filler, *answered.AnswerTrackID)
	h2 := f.plan().Items[0]
	require.Len(t, h2.Items, 4)
	require.Equal(t, "filler_answer", h2.Items[3].Source)
	require.Equal(t, filler, h2.Items[3].TrackID)
	require.Equal(t, h.Seed, h2.Seed, "an answer rebuilds the hour with its seed")
	for i := range 3 {
		require.Equal(t, h.Items[i].TrackID, h2.Items[i].TrackID, "same seed, same picks")
	}
	f.problem(http.StatusConflict, errcodes.FillerRequestClosed, http.MethodPost, answer, map[string]any{"track_id": filler})

	// A forced rebuild asks again; when the deadline passes, the default
	// mode (the global fallback: random) fills the gap.
	var accepted dto.OperationAccepted
	f.ok(http.StatusAccepted, http.MethodPost, "/api/track-plan/rebuild", map[string]any{
		"from": start.Format(time.RFC3339), "to": start.Add(time.Hour).Format(time.RFC3339),
	}, &accepted)
	f.waitOperation(&accepted.OperationID)
	f.ok(http.StatusOK, http.MethodGet, "/api/filler-requests", nil, &reqs)
	require.Len(t, reqs.Items, 1)
	require.NotEqual(t, r.ID, reqs.Items[0].ID)
	require.Len(t, f.plan().Items[0].Items, 3)

	// Within playout.prepush_minutes of its start the hour is frozen: its
	// items may be in Liquidsoap's queue.
	f.clock.set(start.Add(-2 * time.Minute))
	f.problem(http.StatusConflict, errcodes.HourStarted, http.MethodPost,
		fmt.Sprintf("/api/filler-requests/%d/answer", reqs.Items[0].ID), map[string]any{"track_id": filler})

	f.clock.set(start.Add(-time.Hour))
	summary, err := f.rotation.RunJob(t.Context())
	require.NoError(t, err)
	require.Contains(t, summary, "1 reminders defaulted")
	f.ok(http.StatusOK, http.MethodGet, "/api/filler-requests?status=defaulted", nil, &reqs)
	require.Len(t, reqs.Items, 1)
	h3 := f.plan().Items[0]
	require.Len(t, h3.Items, 4)
	require.Equal(t, "filler_random", h3.Items[3].Source)
	f.problem(http.StatusConflict, errcodes.FillerRequestClosed, http.MethodPost,
		fmt.Sprintf("/api/filler-requests/%d/answer", reqs.Items[0].ID), map[string]any{"track_id": filler})

	f.fieldError("to", errcodes.Range, http.MethodPost, "/api/track-plan/rebuild", map[string]any{
		"from": start.Format(time.RFC3339), "to": start.Format(time.RFC3339),
	})
}
