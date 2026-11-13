//go:build integration

package library_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

// airColumn reads the stored on-air length of a track.
func (f *fixture) airColumn(id uint64) *int {
	f.t.Helper()
	var got *int
	require.NoError(f.t, f.d.Pool.QueryRow(f.t.Context(), `SELECT air_duration_ms FROM tracks WHERE id = $1`, id).Scan(&got))
	return got
}

// The generated column equals Track.AirDurationMS (docs/track-stats-plan.md
// decision 2), for a cue set in every way the schema allows.
func TestAirDurationColumnEqualsTheMethod(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	tests := []struct {
		name     string
		duration *int
		cueIn    int
		cueOut   *int
		want     *int
	}{
		{"no cue", new(200000), 0, nil, new(200000)},
		{"cue in only", new(200000), 5000, nil, new(195000)},
		{"cue out only", new(200000), 0, new(150000), new(150000)},
		{"both", new(200000), 5000, new(150000), new(145000)},
		{"no duration", nil, 0, nil, nil},
		{"no duration with a cue out", nil, 0, new(150000), nil},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := f.id(`INSERT INTO tracks (title, file_path, type_id, duration_ms, cue_in_ms, cue_out_ms)
VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, tt.name, fmt.Sprintf("music/air-%d.mp3", i), f.music, tt.duration, tt.cueIn, tt.cueOut)
			tr, err := f.queries.Get(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, tt.want, f.airColumn(id))
			require.Equal(t, tr.AirDurationMS(), f.airColumn(id))
		})
	}
}

func TestAirDurationColumnFollowsSetCueAndRefusesWrites(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	id := f.track("Song", nil)
	require.Equal(t, 200000, *f.airColumn(id))

	require.NoError(t, f.tracks.SetCue(ctx, id, library.Cue{InMS: 5000, OutMS: new(150000)}))
	require.Equal(t, 145000, *f.airColumn(id))
	require.NoError(t, f.tracks.SetCue(ctx, id, library.Cue{}))
	require.Equal(t, 200000, *f.airColumn(id))

	_, err := f.d.Pool.Exec(ctx, `UPDATE tracks SET air_duration_ms = 1 WHERE id = $1`, id)
	require.Error(t, err, "the column cannot be written")
}

func TestTrackStatsEmptyLibrary(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	got, err := f.queries.Stats(t.Context(), library.TrackFilter{})
	require.NoError(t, err)
	require.Equal(t, library.TrackStats{}, got)
}

func TestTrackStatsMixedSet(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	reviewed := f.track("Reviewed", nil) // not analysed, 200 s
	issues := f.track("Issues", nil)     // 200 s, cut to 100 s
	require.NoError(t, f.tracks.SetCue(ctx, issues, library.Cue{OutMS: new(100000)}))
	override := f.track("Override", nil) // 200 s
	clean := f.track("Clean", nil)       // 200 s
	noDuration, err := f.tracks.Create(ctx, library.TrackInput{Title: "Unknown", FilePath: "music/unknown.mp3", TypeID: f.music})
	require.NoError(t, err)

	f.exec(`UPDATE tracks SET reviewed_at = now() WHERE id = $1`, reviewed)
	f.exec(`UPDATE tracks SET analyzed_at = now(), quality_issues = '{loudness}' WHERE id = $1`, issues)
	f.exec(`UPDATE tracks SET analyzed_at = now(), quality_issues = '{loudness,true_peak}', quality_ok = true WHERE id = $1`, override)
	f.exec(`UPDATE tracks SET analyzed_at = now(), quality_ok = false, reviewed_at = now() WHERE id = $1`, clean)
	f.exec(`UPDATE tracks SET analyzed_at = now() WHERE id = $1`, noDuration)

	got, err := f.queries.Stats(ctx, library.TrackFilter{})
	require.NoError(t, err)
	require.Equal(t, library.TrackStats{
		Total: 5, Reviewed: 2, WithIssues: 2, Unanalysed: 1, AirDurationMS: 700000,
	}, got)
}

// The sum goes past the range of a 32-bit integer.
func TestTrackStatsSumIsBigint(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	for i := range 3 {
		f.exec(`INSERT INTO tracks (title, file_path, type_id, duration_ms) VALUES ('Long', $1, $2, 2000000000)`,
			fmt.Sprintf("music/long-%d.mp3", i), f.music)
	}
	got, err := f.queries.Stats(t.Context(), library.TrackFilter{})
	require.NoError(t, err)
	require.Equal(t, int64(6_000_000_000), got.AirDurationMS)
}

// Stats counts the rows the track list returns, filter by filter.
func TestTrackStatsMatchTheList(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rows := sortFixture(t, f)
	ctx := t.Context()
	pop := f.playlist("Pop")
	for i, r := range rows {
		if i%2 == 0 {
			f.exec(`INSERT INTO track_playlists (track_id, playlist_id) VALUES ($1, $2)`, r.id, pop)
		}
	}
	artist := f.id(`SELECT id FROM artists WHERE name = 'aaa'`)

	tests := []struct {
		name   string
		filter library.TrackFilter
	}{
		{"no filter", library.TrackFilter{}},
		{"type", library.TrackFilter{TypeID: f.music}},
		{"artist", library.TrackFilter{ArtistID: artist}},
		{"playlist", library.TrackFilter{PlaylistID: pop}},
		{"query", library.TrackFilter{Query: "AA"}},
		{"quality ok", library.TrackFilter{Quality: library.QualityOK}},
		{"quality issues", library.TrackFilter{Quality: library.QualityIssuesFound}},
		{"quality unanalysed", library.TrackFilter{Quality: library.QualityUnanalysed}},
		{"reviewed", library.TrackFilter{Reviewed: new(true)}},
		{"not reviewed", library.TrackFilter{Reviewed: new(false)}},
		{"in plan", library.TrackFilter{InPlan: new(true)}},
		{"not in plan", library.TrackFilter{InPlan: new(false)}},
		{"in a playlist", library.TrackFilter{InPlaylist: new(true)}},
		{"in no playlist", library.TrackFilter{InPlaylist: new(false)}},
		{"combined", library.TrackFilter{TypeID: f.music, Quality: library.QualityOK, Reviewed: new(true), InPlan: new(true), InPlaylist: new(true)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := f.queries.Stats(ctx, tt.filter)
			require.NoError(t, err)

			list := tt.filter
			list.Limit = 100
			tracks, total, err := f.queries.List(ctx, list)
			require.NoError(t, err)
			want := library.TrackStats{Total: total}
			for _, tr := range tracks {
				if tr.ReviewedAt != nil {
					want.Reviewed++
				}
				if len(tr.QualityIssues) > 0 {
					want.WithIssues++
				}
				if tr.AnalyzedAt == nil {
					want.Unanalysed++
				}
				if air := tr.AirDurationMS(); air != nil {
					want.AirDurationMS += int64(*air)
				}
			}
			require.Equal(t, want, got)
		})
	}
	all, err := f.queries.Stats(ctx, library.TrackFilter{})
	require.NoError(t, err)
	require.Equal(t, len(rows), all.Total)
}

// A long file with a short cue sorts by what goes on air; tracks without a
// duration stay last in both orders, a cue out notwithstanding.
func TestTrackSortDurationUsesTheOnAirLength(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	track := func(title string, duration *int) uint64 {
		t.Helper()
		id, err := f.tracks.Create(ctx, library.TrackInput{Title: title, FilePath: "music/" + title + ".mp3", TypeID: f.music, DurationMS: duration})
		require.NoError(t, err)
		return id
	}
	cut := track("cut", new(300000))
	require.NoError(t, f.tracks.SetCue(ctx, cut, library.Cue{OutMS: new(10000)}))
	track("medium", new(15000))
	track("long", new(20000))
	track("none", nil)
	cueOnly := track("none-cue", nil)
	f.exec(`UPDATE tracks SET cue_out_ms = 150000 WHERE id = $1`, cueOnly)

	titles := func(desc bool) []string {
		t.Helper()
		ts, _, err := f.queries.List(ctx, library.TrackFilter{Sort: library.SortDuration, Desc: desc, Limit: 10})
		require.NoError(t, err)
		out := make([]string, len(ts))
		for i, tr := range ts {
			out[i] = tr.Title
		}
		return out
	}
	require.Equal(t, []string{"cut", "medium", "long", "none", "none-cue"}, titles(false))
	require.Equal(t, []string{"long", "medium", "cut", "none", "none-cue"}, titles(true))
}
