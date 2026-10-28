//go:build integration

package library_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

// item plans a track at start with status.
func (f *fixture) item(clock, track uint64, start time.Time, status string) {
	f.t.Helper()
	f.exec(`INSERT INTO scheduled_items (clock_id, track_id, planned_start_utc, planned_out_ms, source, status)
	        VALUES ($1, $2, $3, 200000, 'fill', $4)`, clock, track, start, status)
}

// hour records a built plan hour.
func (f *fixture) hour(clock uint64, start time.Time) {
	f.t.Helper()
	f.exec(`INSERT INTO plan_hours (hour_start_utc, clock_id, clock_updated_at, seed, built_at)
	        VALUES ($1, $2, now(), 1, now())`, start, clock)
}

func TestCueStateSetCueAndPlannedHours(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	clock := f.id(`INSERT INTO clocks (name) VALUES ('Morning') RETURNING id`)
	a, b := f.track("A", nil), f.track("B", nil)
	f.exec(`UPDATE tracks SET reviewed_at = now() WHERE id = $1`, a)
	h1 := time.Date(2026, 10, 17, 6, 0, 0, 0, time.UTC)
	for i := range 4 {
		f.hour(clock, h1.Add(time.Duration(i)*time.Hour))
	}
	f.item(clock, a, h1.Add(10*time.Minute), "planned")
	f.item(clock, a, h1.Add(2*time.Hour+59*time.Minute), "planned")
	f.item(clock, b, h1.Add(3*time.Hour), "queued")

	var st library.CueState
	cut := library.Cue{InMS: 5000, OutMS: new(150000), FadeInMS: 1000, FadeOutMS: 2000}
	err := f.d.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		ts := f.tracks.WithTx(tx)
		var err error
		if st, err = ts.CueState(ctx, a); err != nil {
			return err
		}
		return ts.SetCue(ctx, a, cut)
	})
	require.NoError(t, err)
	require.Equal(t, library.CueState{DurationMS: new(200000)}, st)

	got, err := f.tracks.Get(ctx, a)
	require.NoError(t, err)
	require.Equal(t, cut, got.Cue)
	require.Equal(t, 145000, *got.AirDurationMS())
	require.NotNil(t, got.ReviewedAt, "editing the cue is reviewing")
	require.Equal(t, h1.Add(10*time.Minute), *got.NextPlannedAt)
	require.Empty(t, got.PlayoutState)

	hours, err := f.tracks.PlannedHours(ctx, a)
	require.NoError(t, err)
	require.Equal(t, &library.HourRange{From: h1, To: h1.Add(3 * time.Hour)}, hours)

	st, err = f.tracks.CueState(ctx, b)
	require.NoError(t, err)
	require.True(t, st.Queued)
	hours, err = f.tracks.PlannedHours(ctx, b)
	require.NoError(t, err)
	require.Nil(t, hours, "a queued item is not rebuilt")

	_, err = f.tracks.CueState(ctx, 999999)
	require.ErrorIs(t, err, library.ErrTrackNotFound)
	require.ErrorIs(t, f.tracks.SetCue(ctx, 999999, cut), library.ErrTrackNotFound)

	// The database refuses what needs no duration.
	require.Error(t, f.tracks.SetCue(ctx, a, library.Cue{InMS: 10, OutMS: new(10)}))
	require.Error(t, f.tracks.SetCue(ctx, a, library.Cue{FadeInMS: 5001}))
}

// A new duration on update keeps a cue that fits and resets one that does
// not (docs/phase-22-plan.md decision 7).
// An update keeps the cue and the analysis of the file: the duration is not
// edited there (the replace of the file analyses a new one).
func TestUpdateKeepsCueAndAnalysis(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	in := library.TrackInput{Title: "Live", FilePath: "music/live.mp3", TypeID: f.music, DurationMS: new(200000), Format: new("mp3"), Bitrate: new(320000), SampleRate: new(44100)}
	id, err := f.tracks.Create(ctx, in)
	require.NoError(t, err)
	cut := library.Cue{InMS: 5000, OutMS: new(150000), FadeInMS: 1000, FadeOutMS: 2000}
	require.NoError(t, f.tracks.SetCue(ctx, id, cut))

	// A PUT without the analysis fields changes the title only.
	require.NoError(t, f.tracks.Update(ctx, id, library.TrackEdit{Title: "Live (edit)", TypeID: f.music}))
	got, err := f.tracks.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, cut, got.Cue)
	require.Equal(t, "music/live.mp3", got.FilePath)
	require.Equal(t, 200000, *got.DurationMS)
	require.Equal(t, "mp3", *got.Format)
	require.Equal(t, 320000, *got.Bitrate)
	require.Equal(t, 44100, *got.SampleRate)
}

func TestUsageMarksFiltersAndSort(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	clock := f.id(`INSERT INTO clocks (name) VALUES ('Morning') RETURNING id`)
	pl := f.playlist("Rock")
	soon, later, queued := f.track("Soon", nil), f.track("Later", nil), f.track("Queued", nil)
	f.track("Idle", nil)
	require.NoError(t, f.tracks.SetPlaylists(ctx, soon, []uint64{pl}))
	h := time.Date(2026, 10, 17, 6, 0, 0, 0, time.UTC)
	f.item(clock, soon, h.Add(time.Hour), "planned")
	f.item(clock, later, h.Add(5*time.Hour), "planned")
	f.item(clock, later, h.Add(7*time.Hour), "planned")
	f.item(clock, queued, h, "queued")

	list := func(filter library.TrackFilter) ([]string, int) {
		t.Helper()
		filter.Limit = 50
		ts, total, err := f.tracks.List(ctx, filter)
		require.NoError(t, err)
		var titles []string
		for _, tr := range ts {
			titles = append(titles, tr.Title)
		}
		return titles, total
	}
	titles, total := list(library.TrackFilter{InPlan: new(true)})
	require.Equal(t, []string{"Later", "Queued", "Soon"}, titles)
	require.Equal(t, 3, total)
	titles, total = list(library.TrackFilter{InPlan: new(false)})
	require.Equal(t, []string{"Idle"}, titles)
	require.Equal(t, 1, total)
	titles, _ = list(library.TrackFilter{InPlaylist: new(false)})
	require.Equal(t, []string{"Idle", "Later", "Queued"}, titles)
	titles, total = list(library.TrackFilter{InPlaylist: new(true)})
	require.Equal(t, []string{"Soon"}, titles)
	require.Equal(t, 1, total)

	titles, _ = list(library.TrackFilter{Sort: library.SortNextPlanned})
	require.Equal(t, []string{"Queued", "Soon", "Later", "Idle"}, titles, "not planned last")
	titles, _ = list(library.TrackFilter{Sort: library.SortNextPlanned, Desc: true})
	require.Equal(t, []string{"Later", "Soon", "Queued", "Idle"}, titles, "not planned last in both orders")

	ts, _, err := f.tracks.List(ctx, library.TrackFilter{Limit: 50})
	require.NoError(t, err)
	byTitle := map[string]library.Track{}
	for _, tr := range ts {
		byTitle[tr.Title] = tr
	}
	require.Equal(t, h.Add(5*time.Hour), *byTitle["Later"].NextPlannedAt, "the earliest item")
	require.Equal(t, library.PlayoutQueued, byTitle["Queued"].PlayoutState)
	require.Nil(t, byTitle["Idle"].NextPlannedAt)
}
