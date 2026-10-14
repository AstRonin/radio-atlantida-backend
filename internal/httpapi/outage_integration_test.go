//go:build integration

package httpapi_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/errcodes"
)

// waitFailed waits for an operation to finish and expects it failed.
func (a *libraryAPI) waitFailed(id uint64) dto.Operation {
	a.t.Helper()
	var op dto.Operation
	require.Eventually(a.t, func() bool {
		a.ok(http.StatusOK, http.MethodGet, fmt.Sprintf("/api/operations/%d", id), nil, &op)
		return op.Status != "running"
	}, 10*time.Second, 20*time.Millisecond)
	require.Equal(a.t, "failed", op.Status, "%+v", op)
	return op
}

// missedItem logs a missed item of the track in an outage of yesterday.
func (f *clockFixture) missedItem(track uint64) uint64 {
	yesterday := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Hour)
	o := f.exec(`INSERT INTO broadcast_outages (started_at, ended_at, reconciled_at, detected_by, component, reason)
VALUES ($1, $2, $2, 'icecast', 'icecast', 'mount gone') RETURNING id`, yesterday, yesterday.Add(10*time.Minute))
	return f.exec(`INSERT INTO play_log (track_id, status, planned_start_utc, planned_out_ms, outage_id, resolution)
VALUES ($1, 'missed', $2, 120000, $3, 'open') RETURNING id`, track, yesterday.Add(time.Minute), o)
}

func TestMissedItemsRescheduleAndDismiss(t *testing.T) {
	f := newClockFixture(t)
	f.filler()
	news := f.exec(`INSERT INTO tracks (title, file_path, type_id, duration_ms) VALUES ('News', 'news/n.mp3', $1, 120000) RETURNING id`,
		f.typeID("news"))
	var saved dto.ClockSaveResult
	f.ok(http.StatusCreated, http.MethodPost, "/api/clocks", map[string]any{
		"name":     "Day",
		"elements": []any{element(f.music, 19), element(f.music, 19), element(f.music, 19, "filler_mode", "random")},
	}, &saved)
	date, weekday, start := tomorrowNoon(t)
	var assigned dto.GridAssignResult
	f.ok(http.StatusOK, http.MethodPost, "/api/grid/assign", map[string]any{
		"clock_id": saved.Clock.ID, "weekdays": []int{weekday}, "hours": []int{12}, "valid_from": date, "valid_to": date,
	}, &assigned)
	f.waitOperation(assigned.BuildOperationID)
	missed := f.missedItem(news)

	// The lists show it.
	var list dto.MissedItemList
	f.ok(http.StatusOK, http.MethodGet, "/api/missed-items", nil, &list)
	require.Len(t, list.Items, 1)
	m := list.Items[0]
	require.Equal(t, missed, m.ID)
	require.Equal(t, "missed", m.Status)
	require.Equal(t, "open", *m.Resolution)
	require.Equal(t, "icecast", m.Outage.Component)
	require.Equal(t, "mount gone", *m.Outage.Reason)
	require.Nil(t, m.StartedAt)

	from := time.Now().UTC().Add(-48 * time.Hour)
	to := time.Now().UTC()
	q := url.Values{"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}, "status": {"missed"}}
	var hist dto.PlayHistory
	f.ok(http.StatusOK, http.MethodGet, "/api/play-history?"+q.Encode(), nil, &hist)
	require.Len(t, hist.Items, 1)
	require.Nil(t, hist.NextCursor)
	var outages dto.OutageList
	f.ok(http.StatusOK, http.MethodGet, "/api/outages?"+url.Values{"from": q["from"], "to": q["to"]}.Encode(), nil, &outages)
	require.Len(t, outages.Items, 1)
	require.Equal(t, 1, outages.Items[0].MissedCount)

	// Refusals checked before the operation.
	path := fmt.Sprintf("/api/missed-items/%d/reschedule", missed)
	body := func(target time.Time, extra ...any) map[string]any {
		b := map[string]any{"target_hour_start_utc": target.Format(time.RFC3339), "placement": "start"}
		for i := 0; i+1 < len(extra); i += 2 {
			b[extra[i].(string)] = extra[i+1]
		}
		return b
	}
	f.fieldError("target_hour_start_utc", errcodes.Invalid, http.MethodPost, path, body(start.Add(time.Minute)))
	f.fieldError("target_hour_start_utc", errcodes.Range, http.MethodPost, path, body(time.Now().UTC().Truncate(time.Hour)))
	f.fieldError("target_hour_start_utc", errcodes.Invalid, http.MethodPost, path, body(start.Add(time.Hour)))
	f.fieldError("fixed_minute", errcodes.Required, http.MethodPost, path, body(start, "placement", "fixed"))
	f.problem(http.StatusNotFound, errcodes.MissedItemNotFound, http.MethodPost, "/api/missed-items/999999/reschedule", body(start))

	// Placed at the start of the hour.
	var acc dto.OperationAccepted
	f.ok(http.StatusAccepted, http.MethodPost, path, body(start), &acc)
	op := f.waitOperation(&acc.OperationID)
	require.Contains(t, op.ResultSummary, fmt.Sprintf("missed item %d placed at 12:00:00", missed))
	h := f.plan().Items[0]
	require.Equal(t, news, h.Items[0].TrackID)
	require.Equal(t, "reschedule", h.Items[0].Source)
	require.Equal(t, start, h.Items[0].PlannedStartUTC)
	require.Equal(t, start.Add(2*time.Minute), h.Items[1].PlannedStartUTC, "the clock follows it")
	f.ok(http.StatusOK, http.MethodGet, "/api/missed-items?resolution=rescheduled", nil, &list)
	require.Len(t, list.Items, 1)
	require.Equal(t, h.Items[0].ID, *list.Items[0].ResolvedItemID)

	// A rebuild keeps it.
	f.ok(http.StatusAccepted, http.MethodPost, "/api/track-plan/rebuild", map[string]any{
		"from": start.Format(time.RFC3339), "to": start.Add(time.Hour).Format(time.RFC3339),
	}, &acc)
	f.waitOperation(&acc.OperationID)
	h = f.plan().Items[0]
	require.Equal(t, "reschedule", h.Items[0].Source)
	f.ok(http.StatusOK, http.MethodGet, "/api/missed-items?resolution=rescheduled", nil, &list)
	require.Equal(t, h.Items[0].ID, *list.Items[0].ResolvedItemID, "linked to the rebuilt row")

	f.problem(http.StatusConflict, errcodes.MissedItemResolved, http.MethodPost, path, body(start))
	f.problem(http.StatusConflict, errcodes.MissedItemResolved, http.MethodPost, fmt.Sprintf("/api/missed-items/%d/dismiss", missed), nil)

	// A track that breaks separation in that hour: the operation fails and
	// the item stays open.
	other := f.missedItem(f.tracks[0])
	f.ok(http.StatusAccepted, http.MethodPost, fmt.Sprintf("/api/missed-items/%d/reschedule", other), body(start, "placement", "end"), &acc)
	failed := f.waitFailed(acc.OperationID)
	require.Contains(t, failed.ErrorMessage, "cannot be placed")
	f.ok(http.StatusOK, http.MethodGet, "/api/missed-items", nil, &list)
	require.Len(t, list.Items, 1)
	require.Equal(t, other, list.Items[0].ID)

	// Dismiss.
	var e dto.PlayHistoryEntry
	f.ok(http.StatusOK, http.MethodPost, fmt.Sprintf("/api/missed-items/%d/dismiss", other), nil, &e)
	require.Equal(t, "dismissed", *e.Resolution)
	require.NotNil(t, e.ResolvedAt)
	f.problem(http.StatusNotFound, errcodes.MissedItemNotFound, http.MethodPost, "/api/missed-items/999999/dismiss", nil)
	f.ok(http.StatusOK, http.MethodGet, "/api/missed-items", nil, &list)
	require.Empty(t, list.Items)

	// Unassigning the hour reopens the rescheduled item.
	f.ok(http.StatusOK, http.MethodPost, "/api/grid/unassign", map[string]any{
		"weekdays": []int{weekday}, "hours": []int{12}, "valid_from": date, "valid_to": date,
	}, &assigned)
	f.waitOperation(assigned.BuildOperationID)
	f.ok(http.StatusOK, http.MethodGet, "/api/missed-items", nil, &list)
	require.Len(t, list.Items, 1)
	require.Equal(t, missed, list.Items[0].ID)
}

func TestPlayHistoryPages(t *testing.T) {
	f := newClockFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	for i := range 5 {
		at := now.Add(-time.Duration(i+1) * time.Minute)
		f.exec(`INSERT INTO play_log (track_id, status, planned_start_utc, started_at, planned_out_ms, actual_duration_ms)
VALUES ($1, 'aired', $2, $2, 60000, 60000) RETURNING id`, f.tracks[0], at)
	}
	base := url.Values{"from": {now.Add(-time.Hour).Format(time.RFC3339)}, "to": {now.Format(time.RFC3339)}, "limit": {"2"}}
	var seen []uint64
	cursor := ""
	for range 3 {
		q := url.Values{}
		for k, v := range base {
			q[k] = v
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page dto.PlayHistory
		f.ok(http.StatusOK, http.MethodGet, "/api/play-history?"+q.Encode(), nil, &page)
		for _, e := range page.Items {
			seen = append(seen, e.ID)
		}
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	require.Len(t, seen, 5)
	for i := 1; i < len(seen); i++ {
		// The rows were inserted newest first.
		require.Less(t, seen[i-1], seen[i], "newest first")
	}

	q := url.Values{"from": base["from"], "to": base["to"], "cursor": {"nonsense"}}
	f.fieldError("query.cursor", errcodes.Format, http.MethodGet, "/api/play-history?"+q.Encode(), nil)
	q = url.Values{"from": base["from"], "to": base["from"]}
	f.fieldError("query.to", errcodes.Range, http.MethodGet, "/api/play-history?"+q.Encode(), nil)
}
