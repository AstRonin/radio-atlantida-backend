//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/dbtest"
	"github.com/AstRonin/radio-atlantida-backend/internal/grid"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/errcodes"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/media"
	"github.com/AstRonin/radio-atlantida-backend/internal/playrules"
	clocksrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/clocks"
	gridrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/grid"
	libraryrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/library"
	playrulesrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/playrules"
	separationrepo "github.com/AstRonin/radio-atlantida-backend/internal/repository/separation"
	"github.com/AstRonin/radio-atlantida-backend/internal/separation"
)

// libraryAPI is the API over a migrated database and a media root, wired as
// serve wires it.
type libraryAPI struct {
	t         *testing.T
	url       string
	db        *db.DB
	mediaRoot string
}

func newLibraryAPI(t *testing.T) *libraryAPI {
	t.Helper()
	d := dbtest.Migrated(t)
	log := dbtest.Logger(t)
	root := t.TempDir()
	lib := media.New(root)
	svc := library.NewService(d,
		libraryrepo.NewTypes(d), libraryrepo.NewPlaylists(d), libraryrepo.NewArtists(d), libraryrepo.NewTracks(d),
		lib, media.NewProber(lib), log)
	gridSvc := grid.NewService(d, gridrepo.New(d), func(context.Context) (grid.Settings, error) {
		return grid.Settings{Location: stationTZ(t), HorizonDays: 30}, nil // setting defaults
	}, time.Now)
	clocksSvc := clocks.NewService(d, clocksrepo.New(d), clocksrepo.NewCheck(d.Pool), gridSvc,
		func(context.Context) (clocks.Settings, error) {
			return clocks.Settings{DurationToleranceMS: 30000, FixedToleranceMS: 60000, GridHorizonDays: 30}, nil
		}, time.Now)
	srv := httptest.NewServer(httpapi.NewRouter(httpapi.Deps{
		Log: log, DB: d.Pool, Logs: logging.NewFileReader(t.TempDir()),
		Library:   svc,
		PlayRules: playrules.NewService(d, playrulesrepo.New(d.Pool)),
		Separation: separation.NewService(separationrepo.New(d), func(context.Context) (int, error) {
			return 10 * 24, nil // retention.play_log_days default
		}),
		Clocks: clocksSvc,
		Grid:   gridSvc,
		Now:    time.Now,
	}))
	t.Cleanup(srv.Close)
	return &libraryAPI{t: t, url: srv.URL, db: d, mediaRoot: root}
}

// reply is a read response.
type reply struct {
	status int
	header http.Header
	body   []byte
}

// call sends body as JSON and decodes a JSON response into out (if not nil).
func (a *libraryAPI) call(method, path string, body, out any, header ...string) reply {
	a.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(a.t, err)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(a.t.Context(), method, a.url+path, r)
	require.NoError(a.t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(a.t, err)
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(res.Body)
	require.NoError(a.t, err)
	if out != nil && len(raw) > 0 {
		require.NoError(a.t, json.Unmarshal(raw, out), string(raw))
	}
	return reply{status: res.StatusCode, header: res.Header, body: raw}
}

// ok expects status and decodes the body into out.
func (a *libraryAPI) ok(status int, method, path string, body, out any) {
	a.t.Helper()
	res := a.call(method, path, body, out)
	if res.status != status {
		a.t.Fatalf("%s %s: status %d, want %d: %s", method, path, res.status, status, res.body)
	}
}

// problem expects a problem response with status and code.
func (a *libraryAPI) problem(status int, code errcodes.Problem, method, path string, body any) dto.Problem {
	a.t.Helper()
	var p dto.Problem
	res := a.call(method, path, body, &p)
	require.Equal(a.t, status, res.status, "%s %s: %+v", method, path, p)
	require.Equal(a.t, dto.ContentTypeProblem, res.header.Get("Content-Type"))
	require.Equal(a.t, code, p.Code, "%+v", p)
	return p
}

// fieldError expects 422 with exactly one field error.
func (a *libraryAPI) fieldError(field string, code errcodes.Field, method, path string, body any) {
	a.t.Helper()
	p := a.problem(http.StatusUnprocessableEntity, errcodes.ValidationFailed, method, path, body)
	require.Len(a.t, p.Errors, 1, "%+v", p.Errors)
	require.Equal(a.t, field, p.Errors[0].Field, "%+v", p.Errors)
	require.Equal(a.t, code, p.Errors[0].Code, "%+v", p.Errors)
}

func (a *libraryAPI) exec(sql string, args ...any) uint64 {
	a.t.Helper()
	var id uint64
	require.NoError(a.t, a.db.Pool.QueryRow(a.t.Context(), sql, args...).Scan(&id))
	return id
}

func (a *libraryAPI) typeID(code string) uint64 {
	a.t.Helper()
	var list dto.PlaylistTypeList
	a.ok(http.StatusOK, http.MethodGet, "/api/playlist-types", nil, &list)
	for _, t := range list.Items {
		if t.Code == code {
			return t.ID
		}
	}
	a.t.Fatalf("no playlist type %s", code)
	return 0
}

func refsOf(p dto.Problem) []string {
	var out []string
	for _, r := range p.UsedBy {
		out = append(out, fmt.Sprintf("%s:%d:%s", r.Kind, r.ID, r.Name))
	}
	return out
}

func TestLibraryPlaylistTypes(t *testing.T) {
	a := newLibraryAPI(t)

	var list dto.PlaylistTypeList
	a.ok(http.StatusOK, http.MethodGet, "/api/playlist-types", nil, &list)
	require.Len(t, list.Items, 4)
	for _, pt := range list.Items {
		require.True(t, pt.IsSystem, pt.Code)
	}

	var promo dto.PlaylistType
	a.ok(http.StatusCreated, http.MethodPost, "/api/playlist-types", map[string]any{"code": "promo", "name": " Promo "}, &promo)
	require.Equal(t, "Promo", promo.Name, "trimmed")
	require.False(t, promo.IsSystem)

	a.fieldError("code", errcodes.Taken, http.MethodPost, "/api/playlist-types", map[string]any{"code": "promo", "name": "Other"})
	a.fieldError("name", errcodes.Taken, http.MethodPost, "/api/playlist-types", map[string]any{"code": "other", "name": "Music"})
	a.fieldError("code", errcodes.Pattern, http.MethodPost, "/api/playlist-types", map[string]any{"code": "Bad Code", "name": "Bad"})
	a.fieldError("name", errcodes.Required, http.MethodPost, "/api/playlist-types", map[string]any{"code": "blank", "name": "   "})

	// A system type is renamed, never deleted; its code stays.
	music := a.typeID("music")
	var renamed dto.PlaylistType
	a.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/playlist-types/%d", music), map[string]any{"name": "Songs"}, &renamed)
	require.Equal(t, "music", renamed.Code)
	require.Equal(t, "Songs", renamed.Name)
	a.problem(http.StatusConflict, errcodes.SystemTypeNotDeletable, http.MethodDelete, fmt.Sprintf("/api/playlist-types/%d", music), nil)

	// A type in use names the playlists and tracks that have it.
	var pl dto.Playlist
	a.ok(http.StatusCreated, http.MethodPost, "/api/playlists", map[string]any{"name": "Promos", "type_id": promo.ID}, &pl)
	var tr dto.Track
	a.ok(http.StatusCreated, http.MethodPost, "/api/tracks", map[string]any{
		"title": "Spot", "file_path": "promo/spot.mp3", "type_id": promo.ID, "playlist_ids": []uint64{},
	}, &tr)
	p := a.problem(http.StatusConflict, errcodes.PlaylistTypeInUse, http.MethodDelete, fmt.Sprintf("/api/playlist-types/%d", promo.ID), nil)
	require.Equal(t, []string{fmt.Sprintf("playlist:%d:Promos", pl.ID), fmt.Sprintf("track:%d:Spot", tr.ID)}, refsOf(p))

	var got dto.PlaylistType
	a.ok(http.StatusOK, http.MethodGet, fmt.Sprintf("/api/playlist-types/%d", promo.ID), nil, &got)
	require.Equal(t, 1, got.PlaylistCount)
	require.Equal(t, 1, got.TrackCount)

	a.ok(http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/api/playlists/%d", pl.ID), nil, nil)
	a.ok(http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/api/tracks/%d", tr.ID), nil, nil)
	a.ok(http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/api/playlist-types/%d", promo.ID), nil, nil)
	a.problem(http.StatusNotFound, errcodes.PlaylistTypeNotFound, http.MethodDelete, fmt.Sprintf("/api/playlist-types/%d", promo.ID), nil)
}

func TestLibraryPlaylistsArtistsTracks(t *testing.T) {
	a := newLibraryAPI(t)
	music, jingle := a.typeID("music"), a.typeID("jingle")

	var pop, rock dto.Playlist
	a.ok(http.StatusCreated, http.MethodPost, "/api/playlists", map[string]any{"name": "Pop", "type_id": music}, &pop)
	a.ok(http.StatusCreated, http.MethodPost, "/api/playlists", map[string]any{"name": "Rock", "type_id": music}, &rock)
	require.Equal(t, "music", pop.Type.Code)
	require.Equal(t, []uint64{}, pop.PlayRuleIDs)
	a.fieldError("name", errcodes.Taken, http.MethodPost, "/api/playlists", map[string]any{"name": "Pop", "type_id": music})
	a.fieldError("type_id", errcodes.UnknownID, http.MethodPost, "/api/playlists", map[string]any{"name": "X", "type_id": 9999})

	var abba dto.Artist
	a.ok(http.StatusCreated, http.MethodPost, "/api/artists", map[string]any{"name": "ABBA"}, &abba)
	a.fieldError("name", errcodes.Taken, http.MethodPost, "/api/artists", map[string]any{"name": "ABBA"})

	// A track is registered by path; the file need not exist yet.
	var song dto.Track
	a.ok(http.StatusCreated, http.MethodPost, "/api/tracks", map[string]any{
		"title": "Song", "artist_id": abba.ID, "file_path": "music/song.mp3", "type_id": music,
		"duration_ms": 180000, "format": "mp3", "playlist_ids": []uint64{pop.ID, rock.ID},
	}, &song)
	require.Equal(t, []uint64{pop.ID, rock.ID}, song.PlaylistIDs)
	require.Equal(t, "ABBA", song.Artist.Name)
	require.Equal(t, 180000, *song.DurationMS)

	for _, bad := range []string{"../song.mp3", "/abs/song.mp3", `music\song.mp3`, "C:/song.mp3", "music//song.mp3", "./song.mp3"} {
		a.fieldError("file_path", errcodes.Format, http.MethodPost, "/api/tracks", map[string]any{
			"title": "Bad", "file_path": bad, "type_id": music, "playlist_ids": []uint64{},
		})
	}
	a.fieldError("file_path", errcodes.Taken, http.MethodPost, "/api/tracks", map[string]any{
		"title": "Copy", "file_path": "music/song.mp3", "type_id": music, "playlist_ids": []uint64{},
	})
	a.fieldError("artist_id", errcodes.UnknownID, http.MethodPost, "/api/tracks", map[string]any{
		"title": "X", "artist_id": 9999, "file_path": "x.mp3", "type_id": music, "playlist_ids": []uint64{},
	})
	a.fieldError("playlist_ids", errcodes.UnknownID, http.MethodPost, "/api/tracks", map[string]any{
		"title": "X", "file_path": "x.mp3", "type_id": music, "playlist_ids": []uint64{9999},
	})
	var none dto.TrackPage
	a.ok(http.StatusOK, http.MethodGet, "/api/tracks?q=X", nil, &none)
	require.Empty(t, none.Items, "a refused track leaves nothing behind (one transaction)")

	// PUT replaces the track: the artist is cleared, the playlists change.
	var updated dto.Track
	a.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/tracks/%d", song.ID), map[string]any{
		"title": "Song", "artist_id": nil, "file_path": "music/song.mp3", "type_id": music,
		"duration_ms": 180000, "playlist_ids": []uint64{rock.ID},
	}, &updated)
	require.Nil(t, updated.Artist)
	require.Nil(t, updated.Format, "cleared")
	require.Equal(t, []uint64{rock.ID}, updated.PlaylistIDs)

	// Listing: filters and pages in title order.
	for _, title := range []string{"b-side", "A_ha 50%", "Jingle"} {
		typeID := music
		if title == "Jingle" {
			typeID = jingle
		}
		a.ok(http.StatusCreated, http.MethodPost, "/api/tracks", map[string]any{
			"title": title, "file_path": "music/" + title + ".mp3", "type_id": typeID, "playlist_ids": []uint64{pop.ID},
		}, nil)
	}
	var page1, page2 dto.TrackPage
	a.ok(http.StatusOK, http.MethodGet, "/api/tracks?limit=2", nil, &page1)
	require.Equal(t, []string{"A_ha 50%", "b-side"}, titles(page1), "case-insensitive collation order")
	require.NotEmpty(t, page1.NextCursor)
	a.ok(http.StatusOK, http.MethodGet, "/api/tracks?limit=2&cursor="+page1.NextCursor, nil, &page2)
	require.Equal(t, []string{"Jingle", "Song"}, titles(page2))
	require.Empty(t, page2.NextCursor)

	var filtered dto.TrackPage
	a.ok(http.StatusOK, http.MethodGet, fmt.Sprintf("/api/tracks?playlist_id=%d&type_id=%d", pop.ID, music), nil, &filtered)
	require.Equal(t, []string{"A_ha 50%", "b-side"}, titles(filtered))
	a.ok(http.StatusOK, http.MethodGet, "/api/tracks?q=50%25", nil, &filtered)
	require.Equal(t, []string{"A_ha 50%"}, titles(filtered), "% is literal")
	a.ok(http.StatusOK, http.MethodGet, "/api/tracks?q=a_h", nil, &filtered)
	require.Equal(t, []string{"A_ha 50%"}, titles(filtered), "_ is literal")
	a.fieldError("query.cursor", errcodes.Format, http.MethodGet, "/api/tracks?cursor=nonsense", nil)

	// Membership is set from the playlist side too.
	var set dto.Playlist
	a.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/playlists/%d/tracks", pop.ID), map[string]any{"track_ids": []uint64{song.ID}}, &set)
	require.Equal(t, 1, set.TrackCount)
	a.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/playlists/%d/tracks", pop.ID), map[string]any{"track_ids": []uint64{}}, &set)
	require.Equal(t, 0, set.TrackCount)
	a.problem(http.StatusNotFound, errcodes.PlaylistNotFound, http.MethodPut, "/api/playlists/9999/tracks", map[string]any{"track_ids": []uint64{}})
	a.fieldError("track_ids", errcodes.UnknownID, http.MethodPut, fmt.Sprintf("/api/playlists/%d/tracks", pop.ID), map[string]any{"track_ids": []uint64{9999}})

	var artists dto.ArtistList
	a.ok(http.StatusOK, http.MethodGet, "/api/artists?q=ab", nil, &artists)
	require.Len(t, artists.Items, 1)
	a.problem(http.StatusNotFound, errcodes.TrackNotFound, http.MethodGet, "/api/tracks/9999", nil)
	a.problem(http.StatusNotFound, errcodes.ArtistNotFound, http.MethodPut, "/api/artists/9999", map[string]any{"name": "Nobody"})
}

func titles(p dto.TrackPage) []string {
	out := []string{}
	for _, t := range p.Items {
		out = append(out, t.Title)
	}
	return out
}

// Every refusal rule of Phase 6 (init plan, task 8): what blocks a delete,
// and that the history of air blocks it for good.
func TestLibraryDeleteRefusals(t *testing.T) {
	a := newLibraryAPI(t)
	music := a.typeID("music")

	var pop dto.Playlist
	a.ok(http.StatusCreated, http.MethodPost, "/api/playlists", map[string]any{"name": "Pop", "type_id": music}, &pop)
	var abba, gone dto.Artist
	a.ok(http.StatusCreated, http.MethodPost, "/api/artists", map[string]any{"name": "ABBA"}, &abba)
	a.ok(http.StatusCreated, http.MethodPost, "/api/artists", map[string]any{"name": "Gone"}, &gone)
	newTrack := func(title string, artistID any) dto.Track {
		var tr dto.Track
		a.ok(http.StatusCreated, http.MethodPost, "/api/tracks", map[string]any{
			"title": title, "artist_id": artistID, "file_path": "music/" + title + ".mp3", "type_id": music,
			"duration_ms": 200000, "playlist_ids": []uint64{pop.ID},
		}, &tr)
		return tr
	}
	pooled, aired, plain := newTrack("Pooled", abba.ID), newTrack("Aired", gone.ID), newTrack("Plain", nil)

	// Fixtures for what later phases create: a clock that uses the playlist
	// and has a filler pool, and a play log row.
	clock := a.exec(`INSERT INTO clocks (name) VALUES ('Morning') RETURNING id`)
	element := a.exec(`INSERT INTO clock_elements (clock_id, position, nominal_duration_ms) VALUES ($1, 0, 200000) RETURNING id`, clock)
	a.exec(`INSERT INTO clock_element_playlists (clock_element_id, playlist_id) VALUES ($1, $2) RETURNING clock_element_id`, element, pop.ID)
	a.exec(`INSERT INTO clock_filler_pool (clock_id, track_id, position) VALUES ($1, $2, 0) RETURNING id`, clock, pooled.ID)
	now := time.Now().UTC()
	a.exec(`INSERT INTO play_log (track_id, artist_id, status, planned_start_utc, started_at, actual_duration_ms)
	        VALUES ($1, $2, 'aired', $3, $3, 200000) RETURNING id`, aired.ID, gone.ID, now)

	p := a.problem(http.StatusConflict, errcodes.PlaylistInUse, http.MethodDelete, fmt.Sprintf("/api/playlists/%d", pop.ID), nil)
	require.Equal(t, []string{fmt.Sprintf("clock:%d:Morning", clock)}, refsOf(p))

	p = a.problem(http.StatusConflict, errcodes.TrackInUse, http.MethodDelete, fmt.Sprintf("/api/tracks/%d", pooled.ID), nil)
	require.Equal(t, []string{fmt.Sprintf("clock:%d:Morning", clock)}, refsOf(p))

	a.problem(http.StatusConflict, errcodes.TrackHasHistory, http.MethodDelete, fmt.Sprintf("/api/tracks/%d", aired.ID), nil)

	p = a.problem(http.StatusConflict, errcodes.ArtistInUse, http.MethodDelete, fmt.Sprintf("/api/artists/%d", abba.ID), nil)
	require.Equal(t, []string{fmt.Sprintf("track:%d:Pooled", pooled.ID)}, refsOf(p))

	// The artist's track moves to no artist; the play log still names it.
	a.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/tracks/%d", aired.ID), map[string]any{
		"title": "Aired", "file_path": "music/Aired.mp3", "type_id": music, "playlist_ids": []uint64{},
	}, nil)
	a.problem(http.StatusConflict, errcodes.ArtistHasHistory, http.MethodDelete, fmt.Sprintf("/api/artists/%d", gone.ID), nil)

	// A clock's default filler refers to an artist.
	a.exec(`UPDATE clocks SET default_filler_mode = 'artist', default_filler_artist_id = $1 WHERE id = $2 RETURNING id`, abba.ID, clock)
	p = a.problem(http.StatusConflict, errcodes.ArtistInUse, http.MethodDelete, fmt.Sprintf("/api/artists/%d", abba.ID), nil)
	require.Equal(t, []string{fmt.Sprintf("track:%d:Pooled", pooled.ID), fmt.Sprintf("clock:%d:Morning", clock)}, refsOf(p))

	// Play rules: in use while applied to a track or a playlist.
	var rule dto.PlayRule
	a.ok(http.StatusCreated, http.MethodPost, "/api/play-rules", map[string]any{"name": "Once an hour", "max_per_hour": 1}, &rule)
	var ids dto.PlayRuleIDs
	a.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/tracks/%d/play-rules", plain.ID), map[string]any{"rule_ids": []uint64{rule.ID}}, &ids)
	require.Equal(t, []uint64{rule.ID}, ids.RuleIDs)
	a.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/playlists/%d/play-rules", pop.ID), map[string]any{"rule_ids": []uint64{rule.ID}}, &ids)
	p = a.problem(http.StatusConflict, errcodes.PlayRuleInUse, http.MethodDelete, fmt.Sprintf("/api/play-rules/%d", rule.ID), nil)
	require.Equal(t, []string{fmt.Sprintf("track:%d:Plain", plain.ID), fmt.Sprintf("playlist:%d:Pop", pop.ID)}, refsOf(p))

	// Deleting the track removes its rule links, and the plain track goes.
	a.ok(http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/api/tracks/%d", plain.ID), nil, nil)
	var targets dto.PlayRule
	a.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/play-rules/%d/targets", rule.ID), map[string]any{"track_ids": []uint64{}, "playlist_ids": []uint64{}}, &targets)
	require.Zero(t, targets.TrackCount+targets.PlaylistCount)
	a.ok(http.StatusNoContent, http.MethodDelete, fmt.Sprintf("/api/play-rules/%d", rule.ID), nil, nil)
}

func TestLibraryPlayRules(t *testing.T) {
	a := newLibraryAPI(t)
	music := a.typeID("music")

	var night dto.PlayRule
	a.ok(http.StatusCreated, http.MethodPost, "/api/play-rules", map[string]any{
		"name": "Night", "window_from": "22:00", "window_to": "02:00", "weekdays": []int{5, 1}, "notes": " crosses midnight ",
	}, &night)
	require.Equal(t, "22:00", *night.WindowFrom)
	require.Equal(t, "02:00", *night.WindowTo)
	require.Equal(t, []int{1, 5}, night.Weekdays)
	require.Equal(t, "crosses midnight", night.Notes)
	require.Nil(t, night.MaxPerHour)

	a.fieldError("", errcodes.Required, http.MethodPost, "/api/play-rules", map[string]any{"name": "Nothing"})
	a.fieldError("window_to", errcodes.Required, http.MethodPost, "/api/play-rules", map[string]any{"name": "Half", "window_from": "06:00"})
	a.fieldError("window_to", errcodes.Invalid, http.MethodPost, "/api/play-rules", map[string]any{"name": "Empty", "window_from": "06:00", "window_to": "06:00"})
	a.fieldError("window_from", errcodes.Pattern, http.MethodPost, "/api/play-rules", map[string]any{"name": "Bad", "window_from": "24:00", "window_to": "01:00"})
	a.fieldError("weekdays[0]", errcodes.Range, http.MethodPost, "/api/play-rules", map[string]any{"name": "Bad", "weekdays": []int{8}})
	a.fieldError("name", errcodes.Taken, http.MethodPost, "/api/play-rules", map[string]any{"name": "Night", "max_per_hour": 1})

	// PUT replaces the limits: the window is removed.
	var updated dto.PlayRule
	a.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/play-rules/%d", night.ID), map[string]any{
		"name": "Night", "min_interval_minutes": 10,
	}, &updated)
	require.Nil(t, updated.WindowFrom)
	require.Nil(t, updated.Weekdays)
	require.Equal(t, 10, *updated.MinIntervalMinutes)

	var pop dto.Playlist
	a.ok(http.StatusCreated, http.MethodPost, "/api/playlists", map[string]any{"name": "Pop", "type_id": music}, &pop)
	var targets dto.PlayRule
	a.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/play-rules/%d/targets", night.ID), map[string]any{
		"track_ids": []uint64{}, "playlist_ids": []uint64{pop.ID},
	}, &targets)
	require.Equal(t, []uint64{pop.ID}, targets.PlaylistIDs)
	var got dto.Playlist
	a.ok(http.StatusOK, http.MethodGet, fmt.Sprintf("/api/playlists/%d", pop.ID), nil, &got)
	require.Equal(t, []uint64{night.ID}, got.PlayRuleIDs)

	a.fieldError("track_ids", errcodes.UnknownID, http.MethodPut, fmt.Sprintf("/api/play-rules/%d/targets", night.ID),
		map[string]any{"track_ids": []uint64{9999}, "playlist_ids": []uint64{}})
	a.fieldError("rule_ids", errcodes.UnknownID, http.MethodPut, fmt.Sprintf("/api/playlists/%d/play-rules", pop.ID),
		map[string]any{"rule_ids": []uint64{9999}})
	a.problem(http.StatusNotFound, errcodes.TrackNotFound, http.MethodPut, "/api/tracks/9999/play-rules", map[string]any{"rule_ids": []uint64{}})
	a.problem(http.StatusNotFound, errcodes.PlayRuleNotFound, http.MethodPut, "/api/play-rules/9999/targets",
		map[string]any{"track_ids": []uint64{}, "playlist_ids": []uint64{}})

	var list dto.PlayRuleList
	a.ok(http.StatusOK, http.MethodGet, "/api/play-rules", nil, &list)
	require.Len(t, list.Items, 1)
	require.Equal(t, 1, list.Items[0].PlaylistCount)
}

func TestLibrarySeparationRules(t *testing.T) {
	a := newLibraryAPI(t)

	var list dto.SeparationRuleList
	a.ok(http.StatusOK, http.MethodGet, "/api/separation-rules", nil, &list)
	require.Len(t, list.Items, 2)
	require.Equal(t, "track", list.Items[0].Scope)
	require.Equal(t, 72, list.Items[0].MinIntervalHours)

	var r dto.SeparationRule
	a.ok(http.StatusOK, http.MethodPut, fmt.Sprintf("/api/separation-rules/%d", list.Items[0].ID),
		map[string]any{"min_interval_hours": 96, "notes": "from the reference software"}, &r)
	require.Equal(t, 96, r.MinIntervalHours)
	require.Equal(t, "from the reference software", r.Notes)

	a.fieldError("min_interval_hours", errcodes.Range, http.MethodPut, fmt.Sprintf("/api/separation-rules/%d", r.ID),
		map[string]any{"min_interval_hours": 241})
	a.fieldError("min_interval_hours", errcodes.Range, http.MethodPut, fmt.Sprintf("/api/separation-rules/%d", r.ID),
		map[string]any{"min_interval_hours": 0})
	a.problem(http.StatusNotFound, errcodes.SeparationRuleNotFound, http.MethodPut, "/api/separation-rules/9999",
		map[string]any{"min_interval_hours": 72})
}

// The preview streams the file with Range support; the client never sends
// a path (§6.6).
func TestLibraryTrackPreview(t *testing.T) {
	a := newLibraryAPI(t)
	music := a.typeID("music")

	audio := []byte("ID3fake mp3 bytes for the preview test")
	require.NoError(t, os.MkdirAll(filepath.Join(a.mediaRoot, "music"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(a.mediaRoot, "music", "song.mp3"), audio, 0o600))

	var song, missing dto.Track
	a.ok(http.StatusCreated, http.MethodPost, "/api/tracks", map[string]any{
		"title": "Song", "file_path": "music/song.mp3", "type_id": music, "format": "mp3", "playlist_ids": []uint64{},
	}, &song)
	a.ok(http.StatusCreated, http.MethodPost, "/api/tracks", map[string]any{
		"title": "Missing", "file_path": "music/missing.ogg", "type_id": music, "playlist_ids": []uint64{},
	}, &missing)

	res := a.call(http.MethodGet, fmt.Sprintf("/api/tracks/%d/audio", song.ID), nil, nil)
	require.Equal(t, http.StatusOK, res.status)
	require.Equal(t, "audio/mpeg", res.header.Get("Content-Type"))
	require.Equal(t, "bytes", res.header.Get("Accept-Ranges"))
	require.Equal(t, audio, res.body)

	res = a.call(http.MethodGet, fmt.Sprintf("/api/tracks/%d/audio", song.ID), nil, nil, "Range", "bytes=3-6")
	require.Equal(t, http.StatusPartialContent, res.status)
	require.Equal(t, fmt.Sprintf("bytes 3-6/%d", len(audio)), res.header.Get("Content-Range"))
	require.Equal(t, audio[3:7], res.body)

	res = a.call(http.MethodGet, fmt.Sprintf("/api/tracks/%d/audio", song.ID), nil, nil, "Range", fmt.Sprintf("bytes=%d-", len(audio)+10))
	require.Equal(t, http.StatusRequestedRangeNotSatisfiable, res.status)

	a.problem(http.StatusNotFound, errcodes.TrackFileNotFound, http.MethodGet, fmt.Sprintf("/api/tracks/%d/audio", missing.ID), nil)
	a.problem(http.StatusNotFound, errcodes.TrackNotFound, http.MethodGet, "/api/tracks/9999/audio", nil)

	// A symbolic link that leaves the media root is not served.
	outside := filepath.Join(t.TempDir(), "secret.mp3")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(a.mediaRoot, "music", "link.mp3")))
	var link dto.Track
	a.ok(http.StatusCreated, http.MethodPost, "/api/tracks", map[string]any{
		"title": "Link", "file_path": "music/link.mp3", "type_id": music, "playlist_ids": []uint64{},
	}, &link)
	a.problem(http.StatusNotFound, errcodes.TrackFileNotFound, http.MethodGet, fmt.Sprintf("/api/tracks/%d/audio", link.ID), nil)
}
