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

func stationTZ(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Yerevan")
	require.NoError(t, err)
	return loc
}

// clockFixture is a library with a music playlist of 19-minute tracks and a
// jingle playlist of 30-second jingles.
type clockFixture struct {
	*libraryAPI
	music, jingles int64
	tracks         []int64
}

func newClockFixture(t *testing.T) *clockFixture {
	t.Helper()
	a := newLibraryAPI(t)
	f := &clockFixture{libraryAPI: a}
	var pl dto.Playlist
	a.ok(http.StatusCreated, http.MethodPost, "/api/playlists", map[string]any{"name": "Music", "type_id": a.typeID("music")}, &pl)
	f.music = pl.ID
	a.ok(http.StatusCreated, http.MethodPost, "/api/playlists", map[string]any{"name": "Jingles", "type_id": a.typeID("jingle")}, &pl)
	f.jingles = pl.ID
	for i := range 4 {
		id := a.exec(`INSERT INTO tracks (title, file_path, type_id, duration_ms) VALUES ($1, $2, $3, 1140000) RETURNING id`,
			fmt.Sprintf("Song %d", i), fmt.Sprintf("music/%d.mp3", i), a.typeID("music"))
		a.exec(`INSERT INTO track_playlists (track_id, playlist_id) VALUES ($1, $2) RETURNING track_id`, id, f.music)
		f.tracks = append(f.tracks, id)
	}
	id := a.exec(`INSERT INTO tracks (title, file_path, type_id, duration_ms) VALUES ('Jingle', 'jingles/j.mp3', $1, 30000) RETURNING id`,
		a.typeID("jingle"))
	a.exec(`INSERT INTO track_playlists (track_id, playlist_id) VALUES ($1, $2) RETURNING track_id`, id, f.jingles)
	return f
}

func element(playlist int64, minutes int, extra ...any) map[string]any {
	e := map[string]any{"nominal_duration_ms": minutes * 60000, "playlist_ids": []int64{playlist}}
	for i := 0; i+1 < len(extra); i += 2 {
		e[extra[i].(string)] = extra[i+1]
	}
	return e
}

func warningCodes(c dto.ClockCheck) []string {
	var out []string
	for _, w := range c.Warnings {
		out = append(out, w.Code)
	}
	return out
}

func TestClockSaveFlow(t *testing.T) {
	f := newClockFixture(t)

	// 3 × 19 minutes: short by 3 minutes, saved incomplete.
	var saved dto.ClockSaveResult
	f.ok(http.StatusCreated, http.MethodPost, "/api/clocks", map[string]any{
		"name": "Morning",
		"elements": []any{
			element(f.music, 19), element(f.music, 19), element(f.music, 19),
		},
	}, &saved)
	c := saved.Clock
	require.Equal(t, "incomplete", c.Status)
	require.Equal(t, 180000, saved.Check.ShortfallMS)
	require.True(t, saved.Check.RequiresFillerChoice)
	require.Equal(t, []int{0}, saved.Check.ShortSegments)
	require.Len(t, c.Elements, 3)
	require.Equal(t, "flow", c.Elements[0].Placement)
	require.Equal(t, 30000, c.Elements[0].DurationToleranceMS, "default from the settings")
	require.Equal(t, 2, c.Layout.Segments[0].HolderIndex)
	// Four 19-minute tracks for 3 plays per hour: no grid hours yet, so no
	// separation warning.
	require.Empty(t, saved.Check.Warnings)

	path := fmt.Sprintf("/api/clocks/%d", c.ID)

	// The dialog: pool mode needs tracks in the pool.
	f.fieldError("pool_track_ids", errcodes.Required, http.MethodPost, path+"/save-with-filler", map[string]any{
		"segments": []any{map[string]any{"segment": 0, "filler_mode": "pool"}},
	})
	f.fieldError("segments[0].segment", errcodes.Range, http.MethodPost, path+"/save-with-filler", map[string]any{
		"segments": []any{map[string]any{"segment": 3, "filler_mode": "random"}},
	})
	f.ok(http.StatusOK, http.MethodPost, path+"/save-with-filler", map[string]any{
		"segments":       []any{map[string]any{"segment": 0, "filler_mode": "pool"}},
		"pool_track_ids": []int64{f.tracks[3], f.tracks[2]},
	}, &saved)
	require.Equal(t, "complete", saved.Clock.Status)
	require.False(t, saved.Check.RequiresFillerChoice)
	require.Equal(t, "pool", saved.Clock.Elements[2].FillerMode)

	var pool dto.FillerPool
	f.ok(http.StatusOK, http.MethodGet, path+"/filler-pool", nil, &pool)
	require.Len(t, pool.Items, 2)
	require.Equal(t, f.tracks[3], pool.Items[0].TrackID)

	// Re-saving keeps the choice: no further questions.
	elements := make([]any, len(saved.Clock.Elements))
	for i, e := range saved.Clock.Elements {
		elements[i] = element(f.music, 19, "id", e.ID, "filler_mode", e.FillerMode)
	}
	f.ok(http.StatusOK, http.MethodPut, path, map[string]any{"name": "Morning", "elements": elements}, &saved)
	require.Equal(t, "complete", saved.Clock.Status)

	// A fixed jingle at :30 splits the hour; then a new fixed element at
	// :31 replaces it.
	f.ok(http.StatusOK, http.MethodPut, path, map[string]any{"name": "Morning", "elements": []any{
		element(f.music, 19, "id", saved.Clock.Elements[0].ID),
		element(f.jingles, 1, "placement", "fixed", "fixed_minute", 30, "duration_tolerance_ms", 0),
		element(f.music, 19, "id", saved.Clock.Elements[1].ID),
		element(f.music, 9, "id", saved.Clock.Elements[2].ID, "filler_mode", "random"),
	}}, &saved)
	require.Len(t, saved.Clock.Layout.Segments, 2)
	require.Equal(t, 10*60000, saved.Clock.Layout.Segments[0].ShortfallMS, "19 minutes before the earliest start at :29")
	require.Equal(t, []int{0}, saved.Check.ShortSegments)
	require.Equal(t, "incomplete", saved.Clock.Status)
	jingleID := saved.Clock.Elements[1].ID
	require.Contains(t, warningCodes(saved.Check), "element_no_fitting_track", "the music tracks do not last 9 minutes")

	f.ok(http.StatusOK, http.MethodPut, path, map[string]any{"name": "Morning", "elements": []any{
		element(f.music, 19, "id", saved.Clock.Elements[0].ID),
		element(f.jingles, 1, "id", jingleID, "placement", "fixed", "fixed_minute", 30, "duration_tolerance_ms", 0),
		element(f.jingles, 1, "placement", "fixed", "fixed_minute", 31, "duration_tolerance_ms", 0, "filler_mode", "ask"),
		element(f.music, 19, "id", saved.Clock.Elements[2].ID),
	}}, &saved)
	require.Len(t, saved.Check.ReplacedElements, 1)
	require.Equal(t, jingleID, saved.Check.ReplacedElements[0].ID)
	require.Len(t, saved.Clock.Elements, 3)
	require.Equal(t, 31, *saved.Clock.Elements[1].FixedMinute)

	// save-incomplete keeps the clock as it is.
	var kept dto.ClockSaveResult
	f.ok(http.StatusOK, http.MethodPost, path+"/save-incomplete", nil, &kept)
	require.Equal(t, saved.Clock.Status, kept.Clock.Status)
}

func TestClockRefusals(t *testing.T) {
	f := newClockFixture(t)
	post := func(field string, code errcodes.Field, elements ...any) {
		t.Helper()
		f.fieldError(field, code, http.MethodPost, "/api/clocks", map[string]any{"name": "X", "elements": elements})
	}
	post("elements[0].fixed_minute", errcodes.DoesNotFit, element(f.jingles, 1, "placement", "fixed", "fixed_minute", 0))
	post("elements[1].fixed_minute", errcodes.Overlap,
		element(f.jingles, 1, "placement", "fixed", "fixed_minute", 30),
		element(f.jingles, 1, "placement", "fixed", "fixed_minute", 31))
	post("elements[1].fixed_minute", errcodes.Order,
		element(f.jingles, 1, "placement", "fixed", "fixed_minute", 40),
		element(f.jingles, 1, "placement", "fixed", "fixed_minute", 20))
	post("elements[2].fixed_minute", errcodes.DoesNotFit,
		element(f.music, 20), element(f.music, 12), element(f.jingles, 1, "placement", "fixed", "fixed_minute", 30))
	post("elements[0].playlist_ids", errcodes.UnknownID, element(9999, 10))
	post("elements[0].filler_source_id", errcodes.Required, element(f.music, 10, "filler_mode", "playlist"))
	post("elements[0].id", errcodes.UnknownID, element(f.music, 10, "id", 12345))
	post("elements[0].playlist_ids", errcodes.Length, map[string]any{"nominal_duration_ms": 60000, "playlist_ids": []int64{}})
	f.fieldError("default_filler_mode", errcodes.Enum, http.MethodPost, "/api/clocks", map[string]any{
		"name": "X", "default_filler_mode": "pool", "elements": []any{element(f.music, 60)},
	})

	var saved dto.ClockSaveResult
	f.ok(http.StatusCreated, http.MethodPost, "/api/clocks", map[string]any{"name": "X", "elements": []any{element(f.music, 60)}}, &saved)
	f.fieldError("name", errcodes.Taken, http.MethodPost, "/api/clocks", map[string]any{"name": "X", "elements": []any{element(f.music, 60)}})
	f.problem(http.StatusNotFound, errcodes.ClockNotFound, http.MethodPut, "/api/clocks/9999",
		map[string]any{"name": "Y", "elements": []any{element(f.music, 60)}})

	// A playlist used by a clock cannot be deleted.
	f.problem(http.StatusConflict, errcodes.PlaylistInUse, http.MethodDelete, fmt.Sprintf("/api/playlists/%d", f.music), nil)
}

func TestGridAssignAndSchedule(t *testing.T) {
	f := newClockFixture(t)
	var morning, news dto.ClockSaveResult
	f.ok(http.StatusCreated, http.MethodPost, "/api/clocks", map[string]any{"name": "Morning", "elements": []any{
		element(f.music, 19), element(f.music, 19), element(f.music, 19, "filler_mode", "random"),
	}}, &morning)
	f.ok(http.StatusCreated, http.MethodPost, "/api/clocks", map[string]any{"name": "News", "elements": []any{
		element(f.jingles, 60),
	}}, &news)

	today := grid.Date(time.Now().In(stationTZ(t)))
	tomorrow := today.AddDate(0, 0, 1)
	date := func(t time.Time) string { return t.Format(time.DateOnly) }

	var res dto.GridAssignResult
	f.ok(http.StatusOK, http.MethodPost, "/api/grid/assign", map[string]any{
		"clock_id": morning.Clock.ID, "weekdays": []int{1, 2, 3, 4, 5, 6, 7}, "hours": []int{8, 9}, "valid_from": date(today),
	}, &res)
	require.Len(t, res.Created, 14)
	require.Empty(t, res.Replaced)

	// The grid by hour: 08:00 Yerevan is 04:00 UTC.
	var sched dto.GridSchedule
	f.ok(http.StatusOK, http.MethodGet, fmt.Sprintf("/api/grid?from=%s&to=%s", date(today), date(tomorrow)), nil, &sched)
	require.Len(t, sched.Items, 4)
	require.Equal(t, 8, sched.Items[0].Hour)
	require.Equal(t, time.Date(today.Year(), today.Month(), today.Day(), 4, 0, 0, 0, time.UTC), sched.Items[0].StartUTC)
	require.Equal(t, "Morning", sched.Items[0].Clock.Name)

	// One day of news at 08:00 splits the open-ended slot of that weekday.
	wd := grid.ISOWeekday(tomorrow)
	f.ok(http.StatusOK, http.MethodPost, "/api/grid/assign", map[string]any{
		"clock_id": news.Clock.ID, "weekdays": []int{wd}, "hours": []int{8}, "valid_from": date(tomorrow), "valid_to": date(tomorrow),
	}, &res)
	require.Len(t, res.Created, 1)
	require.Len(t, res.Replaced, 1)
	require.Equal(t, "split", res.Replaced[0].Change)
	require.Len(t, res.Replaced[0].Remaining, 2)
	require.Equal(t, date(today), *res.Replaced[0].Remaining[0].ValidTo)
	require.Equal(t, date(tomorrow.AddDate(0, 0, 1)), res.Replaced[0].Remaining[1].ValidFrom)

	f.ok(http.StatusOK, http.MethodGet, fmt.Sprintf("/api/grid?from=%s&to=%s", date(tomorrow), date(tomorrow)), nil, &sched)
	require.Equal(t, "News", sched.Items[0].Clock.Name)
	require.Equal(t, "Morning", sched.Items[1].Clock.Name)

	var slots dto.GridSlotList
	f.ok(http.StatusOK, http.MethodGet, fmt.Sprintf("/api/grid/slots?clock_id=%d", news.Clock.ID), nil, &slots)
	require.Len(t, slots.Items, 1)

	// Refusals: the past, the horizon, an unknown clock.
	f.fieldError("valid_from", errcodes.Range, http.MethodPost, "/api/grid/assign", map[string]any{
		"clock_id": news.Clock.ID, "weekdays": []int{1}, "hours": []int{8}, "valid_from": date(today.AddDate(0, 0, -1)),
	})
	f.fieldError("valid_from", errcodes.Range, http.MethodPost, "/api/grid/assign", map[string]any{
		"clock_id": news.Clock.ID, "weekdays": []int{1}, "hours": []int{8}, "valid_from": date(today.AddDate(0, 0, 31)),
	})
	f.fieldError("clock_id", errcodes.UnknownID, http.MethodPost, "/api/grid/assign", map[string]any{
		"clock_id": 9999, "weekdays": []int{1}, "hours": []int{8}, "valid_from": date(today),
	})
	f.fieldError("query.to", errcodes.Range, http.MethodGet,
		fmt.Sprintf("/api/grid?from=%s&to=%s", date(today), date(today.AddDate(0, 0, 70))), nil)

	// A clock in the grid cannot be deleted; the refusal names the slots.
	p := f.problem(http.StatusConflict, errcodes.ClockInUse, http.MethodDelete, fmt.Sprintf("/api/clocks/%d", news.Clock.ID), nil)
	require.Len(t, p.UsedBy, 1)
	require.Equal(t, "grid_slot", p.UsedBy[0].Kind)

	// Clearing the hours from today removes every slot that starts today
	// or later, then the clock can go.
	f.ok(http.StatusOK, http.MethodPost, "/api/grid/unassign", map[string]any{
		"weekdays": []int{1, 2, 3, 4, 5, 6, 7}, "hours": []int{8, 9}, "valid_from": date(today),
	}, &res)
	require.Empty(t, res.Created)
	f.ok(http.StatusOK, http.MethodGet, "/api/grid/slots", nil, &slots)
	require.Empty(t, slots.Items)
	f.ok(http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/api/clocks/%d", news.Clock.ID), nil, nil)
	f.problem(http.StatusNotFound, errcodes.GridSlotNotFound, http.MethodDelete, "/api/grid/slots/9999", nil)
}

func TestClockDryCheckUsesGridAndPlayRules(t *testing.T) {
	f := newClockFixture(t)
	var saved dto.ClockSaveResult
	f.ok(http.StatusCreated, http.MethodPost, "/api/clocks", map[string]any{"name": "Jingle hour", "elements": []any{
		element(f.jingles, 1, "placement", "fixed", "fixed_minute", 15, "duration_tolerance_ms", 30000),
		element(f.music, 19), element(f.music, 19), element(f.music, 19, "filler_mode", "random"),
	}}, &saved)

	// The only jingle may play from 06:00 to 07:00; the clock airs at 08:00.
	var rule dto.PlayRule
	f.ok(http.StatusCreated, http.MethodPost, "/api/play-rules", map[string]any{
		"name": "Early", "window_from": "06:00", "window_to": "07:00",
	}, &rule)
	f.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/playlists/%d/play-rules", f.jingles), map[string]any{
		"rule_ids": []int64{rule.ID},
	}, nil)
	today := grid.Date(time.Now().In(stationTZ(t)))
	f.ok(http.StatusOK, http.MethodPost, "/api/grid/assign", map[string]any{
		"clock_id": saved.Clock.ID, "weekdays": []int{1, 2, 3, 4, 5, 6, 7}, "hours": []int{8},
		"valid_from": today.Format(time.DateOnly),
	}, nil)

	elements := make([]any, len(saved.Clock.Elements))
	for i, e := range saved.Clock.Elements {
		el := element(e.PlaylistIDs[0], e.NominalDurationMS/60000, "id", e.ID, "filler_mode", e.FillerMode,
			"placement", e.Placement, "duration_tolerance_ms", e.DurationToleranceMS)
		if e.FixedMinute != nil {
			el["fixed_minute"] = *e.FixedMinute
		}
		elements[i] = el
	}
	f.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/clocks/%d", saved.Clock.ID), map[string]any{
		"name": "Jingle hour", "elements": elements,
	}, &saved)

	var blocked, few *dto.ClockWarning
	for i, w := range saved.Check.Warnings {
		switch w.Code {
		case "element_blocked_by_play_rules":
			blocked = &saved.Check.Warnings[i]
		case "element_few_tracks":
			few = &saved.Check.Warnings[i]
		}
	}
	require.NotNil(t, blocked, "%+v", saved.Check.Warnings)
	require.Equal(t, 0, *blocked.ElementIndex)
	require.Len(t, blocked.Hours, 7)
	require.Equal(t, 8, blocked.Hours[0].Hour)
	// 4 music tracks, 3 plays an hour, 08:00 every day within the 72 h track
	// separation: about 9 plays.
	require.NotNil(t, few, "%+v", saved.Check.Warnings)
}
