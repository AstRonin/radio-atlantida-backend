//go:build integration

package library_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

// The q filter (one parameter used twice) with the other filters, a cursor,
// and an offset: the parameters are numbered in the order they are added.
func TestTrackListParameterNumbering(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	alpha, err := f.artists.Create(ctx, "Alpha")
	require.NoError(t, err)
	beta, err := f.artists.Create(ctx, "Beta")
	require.NoError(t, err)
	band, err := f.artists.Create(ctx, "Echo Band")
	require.NoError(t, err)
	jingle := f.id(`SELECT id FROM playlist_types WHERE code = 'jingle'`)
	pool := f.playlist("Pool")

	add := func(title string, artist uint64, typeID uint64, inPool, reviewed bool) uint64 {
		t.Helper()
		id, cerr := f.tracks.Create(ctx, library.TrackInput{
			Title: title, ArtistID: &artist, FilePath: "music/" + title + ".mp3", TypeID: typeID, DurationMS: new(200000),
		})
		require.NoError(t, cerr)
		if inPool {
			require.NoError(t, f.tracks.SetPlaylists(ctx, id, []uint64{pool}))
		}
		if reviewed {
			f.exec(`UPDATE tracks SET reviewed_at = now() WHERE id = $1`, id)
		}
		return id
	}
	echo := make([]uint64, 6)
	for i := range 5 {
		echo[i+1] = add("Echo "+string(rune('1'+i)), alpha, f.music, true, true)
	}
	add("Echo 6", alpha, f.music, true, false) // not reviewed
	add("Foxtrot", alpha, f.music, true, true) // q does not match
	add("Echo 7", alpha, f.music, false, true) // not in the playlist
	echo8 := add("Echo 8", beta, f.music, true, true)
	add("Echo 9", alpha, jingle, true, true)       // another type
	golf := add("Golf", band, f.music, true, true) // q matches the artist

	reviewed := true
	t.Run("every filter, cursor, and offset", func(t *testing.T) {
		t.Parallel()
		filter := library.TrackFilter{
			TypeID: f.music, ArtistID: alpha, PlaylistID: pool, Query: "echo", Reviewed: &reviewed,
			After: &library.TrackCursor{Title: "Echo 1", ID: echo[1]}, Offset: 1, Limit: 2,
		}
		page, total, lerr := f.queries.List(ctx, filter)
		require.NoError(t, lerr)
		require.Equal(t, []uint64{echo[3], echo[4]}, trackIDs(page))
		require.Equal(t, 5, total, "the count leaves the cursor and the page out")

		stats, serr := f.queries.Stats(ctx, filter)
		require.NoError(t, serr)
		require.Equal(t, 5, stats.Total)
		require.Equal(t, 5, stats.Reviewed)
	})
	t.Run("q on the title or the artist name", func(t *testing.T) {
		t.Parallel()
		filter := library.TrackFilter{
			TypeID: f.music, PlaylistID: pool, Query: "ECHO", Reviewed: &reviewed,
			After: &library.TrackCursor{Title: "Echo 2", ID: echo[2]}, Offset: 1, Limit: 10,
		}
		page, total, lerr := f.queries.List(ctx, filter)
		require.NoError(t, lerr)
		require.Equal(t, []uint64{echo[4], echo[5], echo8, golf}, trackIDs(page))
		require.Equal(t, 7, total)
	})
	t.Run("filters and offset without a cursor", func(t *testing.T) {
		t.Parallel()
		page, total, lerr := f.queries.List(ctx, library.TrackFilter{
			ArtistID: alpha, Query: "echo", Reviewed: &reviewed, Offset: 4, Limit: 10,
		})
		require.NoError(t, lerr)
		// Echo 1–5, Echo 7, Echo 9: the fifth onwards.
		require.Len(t, page, 3)
		require.Equal(t, echo[5], page[0].ID)
		require.Equal(t, 7, total)
	})
}

// quality_issues is read as text[]: several issues in their stored order,
// and none as an empty list, by Get and List.
func TestTrackQualityIssuesRoundTrip(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	several, none, fresh := f.track("several", nil), f.track("none", nil), f.track("fresh", nil)
	f.exec(`UPDATE tracks SET analyzed_at = now(), quality_issues = $2 WHERE id = $1`,
		several, []string{"true_peak", "format", "channels"})
	f.exec(`UPDATE tracks SET analyzed_at = now(), quality_issues = '{}' WHERE id = $1`, none)

	want := map[uint64][]library.QualityIssue{
		several: {library.IssueTruePeak, library.IssueFormat, library.IssueChannels},
		none:    {},
		fresh:   {},
	}
	for id, issues := range want {
		tr, err := f.queries.Get(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, tr.QualityIssues)
		require.Equal(t, issues, tr.QualityIssues)
	}
	page, _, err := f.queries.List(ctx, library.TrackFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, page, 3)
	for _, tr := range page {
		require.NotNil(t, tr.QualityIssues)
		require.Equal(t, want[tr.ID], tr.QualityIssues)
	}
}

// An empty ID list deletes nothing and finds no blockers; a delete that
// keeps every track returns an empty list, not nil.
func TestDeleteUnreferencedAndBlockersWithNoIDs(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := t.Context()
	kept := f.track("kept", nil)
	clock := f.id(`INSERT INTO clocks (name) VALUES ('Morning') RETURNING id`)
	f.id(`INSERT INTO clock_inserts (clock_id, offset_ms, track_id) VALUES ($1, 0, $2) RETURNING id`, clock, kept)

	for _, ids := range [][]uint64{nil, {}} {
		deleted, err := f.queries.DeleteUnreferenced(ctx, ids)
		require.NoError(t, err)
		require.NotNil(t, deleted)
		require.Empty(t, deleted)

		blockers, err := f.queries.Blockers(ctx, ids, library.MaxUsers)
		require.NoError(t, err)
		require.Empty(t, blockers)
	}

	deleted, err := f.queries.DeleteUnreferenced(ctx, []uint64{kept})
	require.NoError(t, err)
	require.NotNil(t, deleted)
	require.Empty(t, deleted)
	_, err = f.queries.Get(ctx, kept)
	require.NoError(t, err)
}

func trackIDs(ts []library.Track) []uint64 {
	out := make([]uint64, len(ts))
	for i, tr := range ts {
		out[i] = tr.ID
	}
	return out
}
