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

// clockFixture is a library with a music playlist of four 19-minute tracks
// that may repeat once an hour (play rule "Hourly"), and a jingle playlist
// of one 30-second jingle. A clock of the music playlist plans the four
// songs in an hour: three whole, the fourth cut at 60:00.
type clockFixture struct {
	*libraryAPI
	music, jingles uint64
	tracks         []uint64
	jingle         uint64
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
	var rule dto.PlayRule
	a.ok(http.StatusCreated, http.MethodPost, "/api/play-rules", map[string]any{"name": "Hourly", "min_interval_minutes": 60}, &rule)
	a.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/playlists/%d/play-rules", f.music), map[string]any{"rule_ids": []uint64{rule.ID}}, nil)
	f.jingle = a.exec(`INSERT INTO tracks (title, file_path, type_id, duration_ms) VALUES ('Jingle', 'jingles/j.mp3', $1, 30000) RETURNING id`,
		a.typeID("jingle"))
	a.exec(`INSERT INTO track_playlists (track_id, playlist_id) VALUES ($1, $2) RETURNING track_id`, f.jingle, f.jingles)
	return f
}

// musicClock is the body of a clock that fills the hour from the music
// playlist.
func (f *clockFixture) musicClock(name string) map[string]any {
	return map[string]any{"name": name, "playlists": []any{map[string]any{"playlist_id": f.music}}}
}

func warningCodes(ws []dto.ClockWarning) []string {
	var out []string
	for _, w := range ws {
		out = append(out, w.Code)
	}
	return out
}

func TestClockSaveFlow(t *testing.T) {
	f := newClockFixture(t)

	// Music fills the hour, two jingles are spread over it, and a jingle
	// plays at 29:40 every hour.
	var saved dto.ClockSaveResult
	f.ok(http.StatusCreated, http.MethodPost, "/api/clocks", map[string]any{
		"name": "Morning",
		"playlists": []any{
			map[string]any{"playlist_id": f.music},
			map[string]any{"playlist_id": f.jingles, "clock_rule": "spread", "clock_count": 2},
		},
		"inserts": []any{map[string]any{"offset_ms": 1780000, "tolerance_ms": 20000, "track_id": f.jingle}},
	}, &saved)
	c := saved.Clock
	require.Equal(t, "Morning", c.Name)
	require.Equal(t, []dto.ClockPlaylist{
		{PlaylistID: f.music, Position: 0, EffectiveRule: "fill"},
		{PlaylistID: f.jingles, Position: 1, ClockRule: new("spread"), ClockCount: new(2), EffectiveRule: "spread", EffectiveCount: new(2)},
	}, c.Playlists)
	require.Len(t, c.Inserts, 1)
	require.Equal(t, 20000, c.Inserts[0].ToleranceMS)
	require.NotNil(t, saved.Sample, "the sample hour")
	require.NotEmpty(t, saved.Sample.Items)
	last := saved.Sample.Items[len(saved.Sample.Items)-1]
	require.Equal(t, 3600000, last.OffsetMS+last.LengthMS, "the sample fills the hour")
	sources := map[string]int{}
	for _, it := range saved.Sample.Items {
		sources[it.Source]++
	}
	require.Equal(t, 1, sources["insert"], "%+v", saved.Sample.Items)
	require.Positive(t, sources["fill"])
	f.waitOperation(saved.BuildOperationID)

	var got dto.Clock
	f.ok(http.StatusOK, http.MethodGet, fmt.Sprintf("/api/clocks/%d", c.ID), nil, &got)
	require.Equal(t, c, got)
	var list dto.ClockList
	f.ok(http.StatusOK, http.MethodGet, "/api/clocks", nil, &list)
	require.Equal(t, []dto.ClockSummary{{ID: c.ID, Name: "Morning", PlaylistCount: 2, InsertCount: 1, UpdatedAt: list.Items[0].UpdatedAt}}, list.Items)

	// An update keeps the insert at the same time, with its ID.
	f.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/clocks/%d", c.ID), map[string]any{
		"name": "Morning",
		"playlists": []any{
			map[string]any{"playlist_id": f.jingles, "clock_rule": "start"},
			map[string]any{"playlist_id": f.music},
		},
		"inserts": []any{map[string]any{"offset_ms": 1780000, "track_id": f.jingle}},
	}, &saved)
	require.Equal(t, c.Inserts[0].ID, saved.Clock.Inserts[0].ID)
	require.Equal(t, 60000, saved.Clock.Inserts[0].ToleranceMS, "default rotation.default_fixed_tolerance_ms")
	require.Equal(t, "start", saved.Clock.Playlists[0].EffectiveRule)
	require.Equal(t, "rule", saved.Sample.Items[0].Source, "the jingle starts the hour")
	f.waitOperation(saved.BuildOperationID)
}

func TestClockRefusals(t *testing.T) {
	f := newClockFixture(t)
	post := func(field string, code errcodes.Field, body map[string]any) {
		t.Helper()
		body["name"] = "X"
		f.fieldError(field, code, http.MethodPost, "/api/clocks", body)
	}
	music := map[string]any{"playlist_id": f.music}
	post("playlists", errcodes.FillPlaylistRequired, map[string]any{"playlists": []any{map[string]any{"playlist_id": f.jingles, "clock_rule": "end"}}})
	post("playlists[1].playlist_id", errcodes.UnknownID, map[string]any{"playlists": []any{music, map[string]any{"playlist_id": 9999}}})
	post("playlists[1].playlist_id", errcodes.Invalid, map[string]any{"playlists": []any{music, music}})
	post("playlists[1].fill_weight", errcodes.FillWeightsPartial, map[string]any{"playlists": []any{
		map[string]any{"playlist_id": f.music, "fill_weight": 30}, map[string]any{"playlist_id": f.jingles},
	}})
	post("playlists[1].fill_weight", errcodes.FillWeightNotAllowed, map[string]any{"playlists": []any{
		music, map[string]any{"playlist_id": f.jingles, "clock_rule": "end", "fill_weight": 5},
	}})
	post("playlists[1].clock_count", errcodes.ClockCountRequired, map[string]any{"playlists": []any{
		music, map[string]any{"playlist_id": f.jingles, "clock_rule": "spread"},
	}})
	post("playlists[1].clock_count", errcodes.ClockCountNotAllowed, map[string]any{"playlists": []any{
		music, map[string]any{"playlist_id": f.jingles, "clock_count": 2},
	}})
	post("playlists[0].clock_rule", errcodes.Enum, map[string]any{"playlists": []any{map[string]any{"playlist_id": f.music, "clock_rule": "often"}}})
	post("playlists", errcodes.Length, map[string]any{"playlists": []any{}})
	post("inserts[0].track_id", errcodes.InsertSourceRequired, map[string]any{"playlists": []any{music}, "inserts": []any{map[string]any{"offset_ms": 0}}})
	post("inserts[1].offset_ms", errcodes.InsertOffsetTaken, map[string]any{"playlists": []any{music}, "inserts": []any{
		map[string]any{"offset_ms": 60000, "track_id": f.jingle}, map[string]any{"offset_ms": 60000, "playlist_id": f.jingles},
	}})
	post("inserts[0].offset_ms", errcodes.Range, map[string]any{"playlists": []any{music}, "inserts": []any{map[string]any{"offset_ms": 1500, "track_id": f.jingle}}})
	post("inserts[0].track_id", errcodes.UnknownID, map[string]any{"playlists": []any{music}, "inserts": []any{map[string]any{"offset_ms": 0, "track_id": 9999}}})

	var saved dto.ClockSaveResult
	f.ok(http.StatusCreated, http.MethodPost, "/api/clocks", f.musicClock("X"), &saved)
	f.fieldError("name", errcodes.Taken, http.MethodPost, "/api/clocks", f.musicClock("X"))
	f.problem(http.StatusNotFound, errcodes.ClockNotFound, http.MethodPut, "/api/clocks/9999", f.musicClock("Y"))

	// A playlist used by a clock cannot be deleted.
	p := f.problem(http.StatusConflict, errcodes.PlaylistInUse, http.MethodDelete, fmt.Sprintf("/api/playlists/%d", f.music), nil)
	require.Equal(t, []string{fmt.Sprintf("clock:%d:X", saved.Clock.ID)}, refsOf(p))
}

func TestGridAssignAndSchedule(t *testing.T) {
	f := newClockFixture(t)
	var morning, news dto.ClockSaveResult
	f.ok(http.StatusCreated, http.MethodPost, "/api/clocks", f.musicClock("Morning"), &morning)
	f.ok(http.StatusCreated, http.MethodPost, "/api/clocks", map[string]any{"name": "News", "playlists": []any{
		map[string]any{"playlist_id": f.jingles},
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
	f.waitOperation(res.BuildOperationID) // the plan of the news hour is cleared
	f.ok(http.StatusOK, http.MethodGet, "/api/grid/slots", nil, &slots)
	require.Empty(t, slots.Items)
	f.ok(http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/api/clocks/%d", news.Clock.ID), nil, nil)
	f.problem(http.StatusNotFound, errcodes.GridSlotNotFound, http.MethodDelete, "/api/grid/slots/9999", nil)
}

func TestClockDryCheckUsesGridAndPlayRules(t *testing.T) {
	f := newClockFixture(t)
	var saved dto.ClockSaveResult
	f.ok(http.StatusCreated, http.MethodPost, "/api/clocks", f.musicClock("Jingle hour"), &saved)

	// The only jingle may play from 06:00 to 07:00; the clock airs at 08:00.
	var rule dto.PlayRule
	f.ok(http.StatusCreated, http.MethodPost, "/api/play-rules", map[string]any{
		"name": "Early", "window_from": "06:00", "window_to": "07:00",
	}, &rule)
	f.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/playlists/%d/play-rules", f.jingles), map[string]any{
		"rule_ids": []uint64{rule.ID},
	}, nil)
	today := grid.Date(time.Now().In(stationTZ(t)))
	f.ok(http.StatusOK, http.MethodPost, "/api/grid/assign", map[string]any{
		"clock_id": saved.Clock.ID, "weekdays": []int{1, 2, 3, 4, 5, 6, 7}, "hours": []int{8},
		"valid_from": today.Format(time.DateOnly),
	}, nil)

	// A second fill playlist without an interval rule, and the jingles
	// spread over the hour.
	var extra dto.Playlist
	f.ok(http.StatusCreated, http.MethodPost, "/api/playlists", map[string]any{"name": "Extra", "type_id": f.typeID("music")}, &extra)
	song := f.exec(`INSERT INTO tracks (title, file_path, type_id, duration_ms) VALUES ('Extra', 'music/extra.mp3', $1, 200000) RETURNING id`,
		f.typeID("music"))
	f.exec(`INSERT INTO track_playlists (track_id, playlist_id) VALUES ($1, $2) RETURNING track_id`, song, extra.ID)
	f.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/clocks/%d", saved.Clock.ID), map[string]any{
		"name": "Jingle hour",
		"playlists": []any{
			map[string]any{"playlist_id": f.music},
			map[string]any{"playlist_id": f.jingles, "clock_rule": "spread", "clock_count": 2},
			map[string]any{"playlist_id": extra.ID},
		},
	}, &saved)

	byCode := map[string]dto.ClockWarning{}
	for _, w := range saved.Warnings {
		if _, ok := byCode[w.Code]; !ok {
			byCode[w.Code] = w
		}
	}
	blocked, ok := byCode["rule_blocked_by_play_rules"]
	require.True(t, ok, "%+v", saved.Warnings)
	require.Equal(t, 1, *blocked.PlaylistIndex)
	require.Len(t, blocked.Hours, 7)
	require.Equal(t, 8, blocked.Hours[0].Hour)
	noInterval, ok := byCode["fill_no_interval_rule"]
	require.True(t, ok, "%+v", saved.Warnings)
	require.Equal(t, 2, *noInterval.PlaylistIndex, "only the extra playlist: music has Hourly")
	require.NotContains(t, warningCodes(saved.Warnings), "playlist_no_tracks")
}
