//go:build integration

package httpapi_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/errcodes"
)

func TestLibraryReviewTracks(t *testing.T) {
	a := newLibraryAPI(t)
	music := a.typeID("music")
	// 1 000 tracks, marked in one request.
	a.exec(`WITH n AS (
    INSERT INTO tracks (title, file_path, type_id, duration_ms)
    SELECT 'Bulk ' || lpad(i::text, 4, '0'), 'bulk/' || i || '.mp3', $1, 200000 FROM generate_series(1, 1000) i
    RETURNING id
) SELECT count(*) FROM n`, music)
	var page dto.TrackPage
	a.ok(http.StatusOK, http.MethodGet, "/api/tracks?limit=500", nil, &page)
	ids := make([]uint64, 0, 1000)
	for _, tr := range page.Items {
		ids = append(ids, tr.ID)
	}
	a.ok(http.StatusOK, http.MethodGet, "/api/tracks?limit=500&offset=500", nil, &page)
	for _, tr := range page.Items {
		ids = append(ids, tr.ID)
	}
	require.Len(t, ids, 1000)

	var other dto.Track
	a.ok(http.StatusCreated, http.MethodPost, "/api/tracks", map[string]any{
		"title": "Other", "file_path": "music/other.mp3", "type_id": music, "duration_ms": 200000, "playlist_ids": []uint64{},
	}, &other)
	require.Nil(t, other.ReviewedAt)

	var res dto.ReviewTracksResult
	a.ok(http.StatusOK, http.MethodPost, "/api/tracks/review", map[string]any{"track_ids": ids, "reviewed": true}, &res)
	require.Equal(t, ids, res.Updated)
	require.Empty(t, res.NotFound)

	a.ok(http.StatusOK, http.MethodGet, "/api/tracks?reviewed=false", nil, &page)
	require.Equal(t, 1, page.Total)
	require.Equal(t, other.ID, page.Items[0].ID)
	a.ok(http.StatusOK, http.MethodGet, "/api/tracks?reviewed=true&limit=1", nil, &page)
	require.Equal(t, 1000, page.Total)
	require.NotNil(t, page.Items[0].ReviewedAt)

	a.ok(http.StatusOK, http.MethodGet, "/api/tracks?sort=reviewed&order=desc&limit=1&offset=1000", nil, &page)
	require.Equal(t, other.ID, page.Items[0].ID, "not reviewed last in both orders")

	raw := a.call(http.MethodPost, "/api/tracks/review", map[string]any{"track_ids": []uint64{ids[0], 999999}, "reviewed": false}, &res)
	require.Equal(t, http.StatusOK, raw.status, string(raw.body))
	require.JSONEq(t, fmt.Sprintf(`{"updated":[%d],"not_found":[999999]}`, ids[0]), string(raw.body))

	tooMany := make([]uint64, 1001)
	for i := range tooMany {
		tooMany[i] = uint64(i + 1)
	}
	a.fieldError("track_ids", errcodes.Length, http.MethodPost, "/api/tracks/review", map[string]any{"track_ids": tooMany, "reviewed": true})
	a.fieldError("track_ids", errcodes.Unique, http.MethodPost, "/api/tracks/review", map[string]any{"track_ids": []uint64{1, 1}, "reviewed": true})
	a.fieldError("reviewed", errcodes.Required, http.MethodPost, "/api/tracks/review", map[string]any{"track_ids": []uint64{1}})
	a.fieldError("query.reviewed", errcodes.Enum, http.MethodGet, "/api/tracks?reviewed=yes", nil)
}

func TestLibraryTrackEditClearsReviewedMark(t *testing.T) {
	a := newLibraryAPI(t)
	music := a.typeID("music")
	var pl dto.Playlist
	a.ok(http.StatusCreated, http.MethodPost, "/api/playlists", map[string]any{"name": "Rock", "type_id": music}, &pl)
	body := map[string]any{
		"title": "  Song   [Live] ", "file_path": "music/song.mp3", "type_id": music, "duration_ms": 200000, "playlist_ids": []uint64{},
	}
	var tr dto.Track
	a.ok(http.StatusCreated, http.MethodPost, "/api/tracks", body, &tr)
	require.Equal(t, "Song [Live]", tr.Title, "trimmed and collapsed")
	path := fmt.Sprintf("/api/tracks/%d", tr.ID)
	mark := func() {
		var res dto.ReviewTracksResult
		a.ok(http.StatusOK, http.MethodPost, "/api/tracks/review", map[string]any{"track_ids": []uint64{tr.ID}, "reviewed": true}, &res)
	}

	mark()
	body["playlist_ids"], body["quality_notes"] = []uint64{pl.ID}, "checked"
	a.ok(http.StatusOK, http.MethodPut, path, body, &tr)
	require.NotNil(t, tr.ReviewedAt, "playlists and notes are not content")

	body["title"] = "Song [Live] (Edit)"
	var edited dto.Track // fresh: an absent field must not keep an old value
	a.ok(http.StatusOK, http.MethodPut, path, body, &edited)
	require.Nil(t, edited.ReviewedAt, "a new title clears the mark")

	body["title"] = "Song\nB-side"
	a.fieldError("title", errcodes.InvalidCharacters, http.MethodPut, path, body)
	body["title"] = "   "
	a.fieldError("title", errcodes.Required, http.MethodPut, path, body)
}

func TestLibraryRenameTracks(t *testing.T) {
	a := newLibraryAPI(t)
	music := a.typeID("music")
	newTrack := func(title, file string) uint64 {
		return a.exec(`INSERT INTO tracks (title, file_path, type_id) VALUES ($1, $2, $3) RETURNING id`, title, file, music)
	}
	site := newTrack("Song [www.site.com]", "a.mp3")
	upper := newTrack("Hit [WWW.SITE.COM] Mix", "b.mp3")
	plain := newTrack("Plain", "c.mp3")
	only := newTrack("[www.site.com]", "d.mp3")
	long := newTrack(strings.Repeat("x", 450)+" [www.site.com]", "e.mp3")
	var reviewed dto.ReviewTracksResult
	a.ok(http.StatusOK, http.MethodPost, "/api/tracks/review", map[string]any{"track_ids": []uint64{site, plain}, "reviewed": true}, &reviewed)

	body := map[string]any{"track_ids": []uint64{site, upper, plain, only, 999999}, "find": " [www.site.com]", "replace": "", "dry_run": true}
	var preview dto.RenameTracksResult
	a.ok(http.StatusOK, http.MethodPost, "/api/tracks/rename", body, &preview)
	want := dto.RenameTracksResult{
		Changed: []dto.TitleChange{
			{TrackID: site, OldTitle: "Song [www.site.com]", NewTitle: "Song"},
			{TrackID: upper, OldTitle: "Hit [WWW.SITE.COM] Mix", NewTitle: "Hit Mix"},
		},
		Unchanged: []uint64{plain, only},
		Refused:   []dto.RenameRefusal{{TrackID: 999999, Code: string(errcodes.TrackNotFound)}},
	}
	require.Equal(t, want, preview)
	get := func(id uint64) dto.Track {
		var tr dto.Track // fresh: an absent field must not keep an old value
		a.ok(http.StatusOK, http.MethodGet, fmt.Sprintf("/api/tracks/%d", id), nil, &tr)
		return tr
	}
	require.Equal(t, "Song [www.site.com]", get(site).Title, "a dry run changes nothing")

	body["dry_run"] = false
	var applied dto.RenameTracksResult
	a.ok(http.StatusOK, http.MethodPost, "/api/tracks/rename", body, &applied)
	require.Equal(t, want, applied)
	require.Equal(t, "Song", get(site).Title)
	require.Nil(t, get(site).ReviewedAt, "a renamed track loses its mark")
	require.NotNil(t, get(plain).ReviewedAt, "an unchanged track keeps it")

	// Refused, not cut: empty and too long.
	raw := a.call(http.MethodPost, "/api/tracks/rename", map[string]any{
		"track_ids": []uint64{only, long}, "find": "[www.site.com]", "replace": strings.Repeat("y", 200), "match_case": true, "dry_run": true,
	}, &applied)
	require.Equal(t, http.StatusOK, raw.status, string(raw.body))
	require.Equal(t, []dto.RenameRefusal{{TrackID: long, Code: string(errcodes.TitleTooLong)}}, applied.Refused)
	a.ok(http.StatusOK, http.MethodPost, "/api/tracks/rename", map[string]any{
		"track_ids": []uint64{only}, "find": "[www.site.com]", "dry_run": true,
	}, &applied)
	require.Equal(t, []dto.RenameRefusal{{TrackID: only, Code: string(errcodes.TitleEmpty)}}, applied.Refused)

	a.fieldError("find", errcodes.Length, http.MethodPost, "/api/tracks/rename", map[string]any{"track_ids": []uint64{site}, "find": "", "dry_run": true})
	a.fieldError("find", errcodes.Required, http.MethodPost, "/api/tracks/rename", map[string]any{"track_ids": []uint64{site}, "find": "  ", "dry_run": true})
	a.fieldError("replace", errcodes.InvalidCharacters, http.MethodPost, "/api/tracks/rename",
		map[string]any{"track_ids": []uint64{site}, "find": "a", "replace": "b\tc", "dry_run": true})
	a.fieldError("dry_run", errcodes.Required, http.MethodPost, "/api/tracks/rename", map[string]any{"track_ids": []uint64{site}, "find": "a"})
	a.fieldError("track_ids", errcodes.Length, http.MethodPost, "/api/tracks/rename", map[string]any{"track_ids": []uint64{}, "find": "a", "dry_run": true})
}
