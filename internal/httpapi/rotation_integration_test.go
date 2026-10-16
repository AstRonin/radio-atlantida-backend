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

func TestTrackPlanFollowsGridAndClock(t *testing.T) {
	f := newClockFixture(t)

	var saved dto.ClockSaveResult
	f.ok(http.StatusCreated, http.MethodPost, "/api/clocks", f.musicClock("Day"), &saved)
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
	require.Empty(t, h.Inserts)
	// Four songs of 19 minutes, each once (Hourly): three whole, the fourth
	// cut at 60:00.
	require.Len(t, h.Items, 4)
	seen := map[uint64]bool{}
	for i, it := range h.Items {
		require.Equal(t, "fill", it.Source)
		require.Equal(t, "planned", it.Status)
		require.Equal(t, start.Add(time.Duration(i)*19*time.Minute), it.PlannedStartUTC)
		require.False(t, seen[it.TrackID])
		seen[it.TrackID] = true
	}
	require.True(t, h.Items[3].Truncated)
	require.Equal(t, 180000, h.Items[3].PlannedOutMS-h.Items[3].PlannedInMS)

	// A rebuild of a current hour is a no-op; a clock change rebuilds it.
	var again dto.ClockSaveResult
	f.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/clocks/%d", saved.Clock.ID), f.musicClock("Day 2"), &again)
	op = f.waitOperation(again.BuildOperationID)
	require.Contains(t, op.ResultSummary, "built 1 hours")
	require.True(t, f.plan().Items[0].BuiltAt.After(h.BuiltAt))

	// A changed placement rule of a playlist in the clock rebuilds it too.
	var pl dto.PlaylistSaved
	f.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/playlists/%d", f.jingles), map[string]any{
		"name": "Jingles", "type_id": f.typeID("jingle"), "clock_rule": "start",
	}, &pl)
	require.Nil(t, pl.BuildOperationID, "no clock uses the jingles")
	f.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/clocks/%d", saved.Clock.ID), map[string]any{
		"name": "Day 2", "playlists": []any{map[string]any{"playlist_id": f.music}, map[string]any{"playlist_id": f.jingles}},
	}, &again)
	f.waitOperation(again.BuildOperationID)
	require.Equal(t, "rule", f.plan().Items[0].Items[0].Source, "the jingle starts the hour")
	f.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/playlists/%d", f.jingles), map[string]any{
		"name": "Jingles", "type_id": f.typeID("jingle"), "clock_rule": "end",
	}, &pl)
	op = f.waitOperation(pl.BuildOperationID)
	require.Contains(t, op.ResultSummary, "built 1 hours")
	items := f.plan().Items[0].Items
	require.Equal(t, "fill", items[0].Source)
	require.Equal(t, "rule", items[len(items)-1].Source, "the jingle ends the hour")

	// Clearing the grid clears the plan, so the clock can be deleted.
	f.ok(http.StatusOK, http.MethodPost, "/api/grid/unassign", map[string]any{
		"weekdays": []int{weekday}, "hours": []int{12}, "valid_from": date, "valid_to": date,
	}, &assigned)
	op = f.waitOperation(assigned.BuildOperationID)
	require.Contains(t, op.ResultSummary, "cleared 1")
	require.Empty(t, f.plan().Items)
	f.ok(http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/api/clocks/%d", saved.Clock.ID), nil, nil)
}

// The operator adds today's news to one planned hour; the hour is rebuilt
// around it, a rebuild keeps it, and removing it rebuilds the hour again
// (§7.2, docs/phase-23-plan.md §3.4).
func TestHourInserts(t *testing.T) {
	f := newClockFixture(t)
	news := f.exec(`INSERT INTO tracks (title, file_path, type_id, duration_ms) VALUES ('News', 'news/n.mp3', $1, 120000) RETURNING id`,
		f.typeID("news"))
	var saved dto.ClockSaveResult
	f.ok(http.StatusCreated, http.MethodPost, "/api/clocks", f.musicClock("Day"), &saved)
	date, weekday, start := tomorrowNoon(t)
	var assigned dto.GridAssignResult
	f.ok(http.StatusOK, http.MethodPost, "/api/grid/assign", map[string]any{
		"clock_id": saved.Clock.ID, "weekdays": []int{weekday}, "hours": []int{12}, "valid_from": date, "valid_to": date,
	}, &assigned)
	f.waitOperation(assigned.BuildOperationID)

	body := func(extra ...any) map[string]any {
		b := map[string]any{"hour_start_utc": start.Format(time.RFC3339), "offset_ms": 1800000, "tolerance_ms": 0, "track_id": news}
		for i := 0; i+1 < len(extra); i += 2 {
			b[extra[i].(string)] = extra[i+1]
		}
		return b
	}
	const path = "/api/track-plan/inserts"
	f.fieldError("hour_start_utc", errcodes.Invalid, http.MethodPost, path, body("hour_start_utc", start.Add(time.Minute).Format(time.RFC3339)))
	f.fieldError("hour_start_utc", errcodes.Range, http.MethodPost, path, body("hour_start_utc", time.Now().UTC().Truncate(time.Hour).Format(time.RFC3339)))
	f.fieldError("hour_start_utc", errcodes.Invalid, http.MethodPost, path, body("hour_start_utc", start.Add(time.Hour).Format(time.RFC3339)))
	f.fieldError("offset_ms", errcodes.Range, http.MethodPost, path, body("offset_ms", 1800500))
	f.fieldError("offset_ms", errcodes.DoesNotFit, http.MethodPost, path, body("offset_ms", 3590000))
	f.fieldError("track_id", errcodes.UnknownID, http.MethodPost, path, body("track_id", 999999))

	// 12:30:00 sharp: the music before it is cut at 30:00 or ends there.
	var acc dto.OperationAccepted
	f.ok(http.StatusAccepted, http.MethodPost, path, body(), &acc)
	op := f.waitOperation(&acc.OperationID)
	require.Contains(t, op.ResultSummary, "placed at 12:30:00")
	h := f.plan().Items[0]
	require.Len(t, h.Inserts, 1)
	ins := h.Inserts[0]
	require.Equal(t, dto.TrackPlanInsert{ID: ins.ID, Placement: "fixed", OffsetMS: new(1800000), ToleranceMS: 0, TrackID: news}, ins)
	var placed *dto.TrackPlanItem
	for i, it := range h.Items {
		if it.Source == "hour_insert" {
			placed = &h.Items[i]
			require.Equal(t, start.Add(30*time.Minute), h.Items[i].PlannedStartUTC)
			require.Equal(t, start.Add(30*time.Minute), h.Items[i-1].PlannedStartUTC.Add(time.Duration(h.Items[i-1].PlannedOutMS-h.Items[i-1].PlannedInMS)*time.Millisecond))
		}
	}
	require.NotNil(t, placed, "%+v", h.Items)
	require.Equal(t, ins.ID, *placed.HourInsertID)

	// A rebuild and a clock change keep it.
	f.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/clocks/%d", saved.Clock.ID), f.musicClock("Day 2"), &saved)
	f.waitOperation(saved.BuildOperationID)
	h = f.plan().Items[0]
	require.Len(t, h.Inserts, 1)
	require.Contains(t, sourcesOf(h), "hour_insert")

	// A track that breaks a play rule there: the operation fails and
	// nothing changes.
	var early dto.PlayRule
	f.ok(http.StatusCreated, http.MethodPost, "/api/play-rules", map[string]any{"name": "Early", "window_from": "06:00", "window_to": "07:00"}, &early)
	f.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/tracks/%d/play-rules", f.jingle), map[string]any{"rule_ids": []uint64{early.ID}}, nil)
	f.ok(http.StatusAccepted, http.MethodPost, path, body("offset_ms", 600000, "track_id", f.jingle), &acc)
	failed := f.waitFailed(acc.OperationID)
	require.Contains(t, failed.ErrorMessage, "breaks a play rule")
	require.Len(t, f.plan().Items[0].Inserts, 1)

	// Removing it rebuilds the hour without it.
	f.problem(http.StatusNotFound, errcodes.HourInsertNotFound, http.MethodDelete, path+"/999999", nil)
	f.ok(http.StatusAccepted, http.MethodDelete, fmt.Sprintf("%s/%d", path, ins.ID), nil, &acc)
	op = f.waitOperation(&acc.OperationID)
	require.Contains(t, op.ResultSummary, "removed")
	h = f.plan().Items[0]
	require.Empty(t, h.Inserts)
	require.NotContains(t, sourcesOf(h), "hour_insert")

	// A frozen hour is refused.
	f.ok(http.StatusAccepted, http.MethodPost, path, body(), &acc)
	f.waitOperation(&acc.OperationID)
	id := f.plan().Items[0].Inserts[0].ID
	f.clock.set(start.Add(-2 * time.Minute))
	f.problem(http.StatusConflict, errcodes.HourStarted, http.MethodDelete, fmt.Sprintf("%s/%d", path, id), nil)
}

func sourcesOf(h dto.TrackPlanHour) []string {
	var out []string
	for _, it := range h.Items {
		out = append(out, it.Source)
	}
	return out
}
