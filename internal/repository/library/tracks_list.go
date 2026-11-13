package library

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

// trackRow is a row of selectTracks (trackSelect).
type trackRow struct {
	ID           uint64    `db:"id"`
	Title        string    `db:"title"`
	ArtistID     *uint64   `db:"artist_id"`
	ArtistName   *string   `db:"artist_name"`
	DurationMS   *int      `db:"duration_ms"`
	FilePath     string    `db:"file_path"`
	Format       *string   `db:"format"`
	Bitrate      *int      `db:"bitrate"`
	SampleRate   *int      `db:"sample_rate"`
	QualityOK    *bool     `db:"quality_ok"`
	QualityNotes string    `db:"quality_notes"`
	TypeID       uint64    `db:"type_id"`
	TypeCode     string    `db:"type_code"`
	TypeName     string    `db:"type_name"`
	FileChecksum *string   `db:"file_checksum"`
	CreatedAt    time.Time `db:"created_at"`

	Codec           *string  `db:"codec"`
	Channels        *int     `db:"channels"`
	LoudnessLUFS    *float64 `db:"loudness_lufs"`
	TruePeakDBTP    *float64 `db:"true_peak_dbtp"`
	LoudnessRangeLU *float64 `db:"loudness_range_lu"`
	SourceBitrate   *int     `db:"source_bitrate"`
	// QualityIssues is the text[] column as it is (pgx scans arrays).
	QualityIssues []string   `db:"quality_issues"`
	AnalyzedAt    *time.Time `db:"analyzed_at"`
	ReviewedAt    *time.Time `db:"reviewed_at"`

	CueInMS   int  `db:"cue_in_ms"`
	CueOutMS  *int `db:"cue_out_ms"`
	FadeInMS  int  `db:"fade_in_ms"`
	FadeOutMS int  `db:"fade_out_ms"`

	NextPlannedAt *time.Time `db:"next_planned_at"`
	Queued        bool       `db:"queued"`
}

// listRow is a row of List: a track and count(*) OVER ().
type listRow struct {
	trackRow
	Total int64 `db:"total"`
}

// queryArgs numbers the parameters of a query as they are added.
type queryArgs []any

// add appends v and returns its placeholder ($n).
func (a *queryArgs) add(v any) string {
	*a = append(*a, v)
	return "$" + strconv.Itoa(len(*a))
}

// trackSortKeys are the ORDER BY keys of each sort, fixed SQL: the sort
// parameter never reaches the SQL text. Missing values are NULL (an empty
// format too), so NULLS LAST puts them last in both directions. Quality:
// no issues 0, with issues 1 and more issues first, never analysed NULL.
var trackSortKeys = map[library.TrackSort][]string{
	library.SortTitle:    {"tr.title"},
	library.SortArtist:   {"a.name"},
	library.SortType:     {"t.name"},
	library.SortDuration: {"tr.air_duration_ms"},
	library.SortFormat:   {"NULLIF(tr.format, '')"},
	library.SortQuality: {
		"CASE WHEN tr.analyzed_at IS NULL THEN NULL WHEN cardinality(tr.quality_issues) = 0 THEN 0 ELSE 1 END",
		"-cardinality(tr.quality_issues)",
	},
	library.SortReviewed:    {"tr.reviewed_at"},
	library.SortNextPlanned: {"np.next_planned_at"},
}

// trackOrder is the ORDER BY of f: the sort keys in f's direction, then
// title and ID ascending, so equal keys keep their place between pages.
func trackOrder(f library.TrackFilter) (string, error) {
	sort := f.Sort
	if sort == "" {
		sort = library.SortTitle
	}
	keys, ok := trackSortKeys[sort]
	if !ok {
		return "", fmt.Errorf("unknown track sort %q", sort)
	}
	dir := " ASC NULLS LAST"
	if f.Desc {
		dir = " DESC NULLS LAST"
	}
	order := make([]string, 0, len(keys)+2)
	for _, k := range keys {
		order = append(order, k+dir)
	}
	if sort != library.SortTitle {
		order = append(order, "tr.title")
	}
	return strings.Join(append(order, "tr.id"), ", "), nil
}

// The states of a track, each defined once: the filters of trackWhere and the
// figures of Stats use these, so a change of a definition changes both
// (docs/track-stats-plan.md decision 3). quality_ok takes no part: a track
// with a manual "ok" override and open issues is with issues.
const (
	condTrackNoIssues   = "tr.analyzed_at IS NOT NULL AND cardinality(tr.quality_issues) = 0"
	condTrackHasIssues  = "cardinality(tr.quality_issues) > 0"
	condTrackUnanalysed = "tr.analyzed_at IS NULL"
	condTrackReviewed   = "tr.reviewed_at IS NOT NULL"
)

// trackWhere is the WHERE of the filters of f (not the cursor), shared by
// the page, the count, and the statistics so that they cannot drift. The
// conditions are fixed strings; values go in args, numbered from $1.
func trackWhere(f library.TrackFilter) (where []string, args queryArgs) {
	if f.TypeID != 0 {
		where = append(where, "tr.type_id = "+args.add(f.TypeID))
	}
	if f.ArtistID != 0 {
		where = append(where, "tr.artist_id = "+args.add(f.ArtistID))
	}
	if f.PlaylistID != 0 {
		where = append(where, "EXISTS (SELECT 1 FROM track_playlists tp WHERE tp.track_id = tr.id AND tp.playlist_id = "+
			args.add(f.PlaylistID)+")")
	}
	if f.Query != "" {
		p := args.add(containsPattern(f.Query))
		where = append(where, "(tr.title ILIKE "+p+" OR a.name ILIKE "+p+")")
	}
	switch f.Quality {
	case library.QualityOK:
		where = append(where, condTrackNoIssues)
	case library.QualityIssuesFound:
		where = append(where, condTrackHasIssues)
	case library.QualityUnanalysed:
		where = append(where, condTrackUnanalysed)
	}
	if f.Reviewed != nil {
		where = append(where, notIf(!*f.Reviewed)+"("+condTrackReviewed+")")
	}
	switch {
	case f.InPlan == nil:
	case *f.InPlan:
		where = append(where, "np.track_id IS NOT NULL")
	default:
		where = append(where, "np.track_id IS NULL")
	}
	if f.InPlaylist != nil {
		where = append(where, notIf(!*f.InPlaylist)+"EXISTS (SELECT 1 FROM track_playlists tp WHERE tp.track_id = tr.id)")
	}
	return where, args
}

func notIf(not bool) string {
	if not {
		return "NOT "
	}
	return ""
}

func whereClause(where []string) string {
	if len(where) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(where, " AND ")
}

// List returns at most f.Limit tracks in the order of f, after f.After or
// skipping f.Offset, and the number of tracks matching the filters.
//
// The inner query sorts only the IDs with their sort keys and counts the
// matches (count(*) OVER (), computed before LIMIT and OFFSET); the outer
// one reads the columns of the page rows alone. Sorting the full rows
// instead doubles the time: every row is carried through the window and
// the sort. A cursor narrows the WHERE and an offset past the end returns
// no row, so then a second query counts (not in the same snapshot: a list
// view accepts a total that is off by a concurrent import).
//
// The library holds hundreds to tens of thousands of tracks; the filters
// run on a sequential scan of tracks with a sort (top-N heapsort for the
// first pages). No index on a sort key is added: an index on (title, id)
// was not chosen for the last page, and the artist and type keys come from
// a join. The cost of the last page is the collation (en_US.utf8) of the
// text keys.
// EXPLAIN ANALYZE, PostgreSQL 17 in Docker Desktop on an Apple M1 Pro,
// 30 000 tracks, 2 000 artists, no filter; first page (limit 101) / last
// page (offset 29 950, limit 50): title 15 / 29 ms (desc the same), artist
// 20 / 51 ms, type 17 / 43 ms, duration 15 / 20 ms, format 18 / 42 ms,
// quality 19 / 40 ms. With playlist_id and q, by duration desc, offset 200:
// 28 ms. The count alone: 7 ms. Phase 21, the same table with a third of the
// tracks not reviewed: reviewed=false, first page 7 ms; sort reviewed desc,
// last page 31 ms. No index on reviewed_at (the 50 ms rule of phase 20).
// Phase 22, 30 000 tracks with 32-character titles and a 48-hour plan (720
// items), wall time of List: first page 21 ms, in_plan=true 11 ms,
// in_playlist=false 14 ms; sort next_planned_at first page 18 ms, desc last
// page 80 ms, of which the plan join (HashAggregate of the plan, Hash Left
// Join) is 8 ms and the rest the title tie-break, as in the title order's
// last page on the same table (72 ms). No index: the key is an aggregate of
// the plan.
func (s *TrackQueries) List(ctx context.Context, f library.TrackFilter) ([]library.Track, int, error) {
	order, err := trackOrder(f)
	if err != nil {
		return nil, 0, err
	}
	where, args := trackWhere(f)
	if f.After != nil {
		where = append(where, "(tr.title, tr.id) > ("+args.add(f.After.Title)+", "+args.add(f.After.ID)+")")
	}
	page := " LIMIT " + args.add(f.Limit) + " OFFSET " + args.add(f.Offset)
	sql := "SELECT " + trackSelect + `, p.total
FROM (SELECT tr.id, count(*) OVER () AS total` + fromTracks + whereClause(where) + `
      ORDER BY ` + order + page + `) p
JOIN tracks tr ON tr.id = p.id
JOIN playlist_types t ON t.id = tr.type_id
LEFT JOIN artists a ON a.id = tr.artist_id` + planUsage + `
ORDER BY ` + order

	pgRows, err := s.q.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("select tracks: %w", err)
	}
	listed, err := pgx.CollectRows(pgRows, pgx.RowToStructByName[listRow])
	if err != nil {
		return nil, 0, fmt.Errorf("scan tracks: %w", err)
	}
	total := 0
	rows := make([]trackRow, len(listed))
	for i, r := range listed {
		rows[i] = r.trackRow
	}
	if len(listed) > 0 {
		total = int(listed[0].Total)
	}
	if f.After != nil || (len(rows) == 0 && f.Offset > 0) {
		if total, err = s.count(ctx, f); err != nil {
			return nil, 0, err
		}
	}
	ts, err := s.withLinks(ctx, rows)
	if err != nil {
		return nil, 0, err
	}
	return ts, total, nil
}

// count returns the number of tracks matching the filters of f.
func (s *TrackQueries) count(ctx context.Context, f library.TrackFilter) (int, error) {
	where, args := trackWhere(f)
	var n int
	if err := s.q.QueryRow(ctx, "SELECT count(*)"+fromTracks+whereClause(where), args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count tracks: %w", err)
	}
	return n, nil
}

// trackStatsRow is the one row of Stats.
type trackStatsRow struct {
	Total         int   `db:"total"`
	Reviewed      int   `db:"reviewed"`
	WithIssues    int   `db:"with_issues"`
	Unanalysed    int   `db:"unanalysed"`
	AirDurationMS int64 `db:"air_duration_ms"`
}

// Stats returns the figures of the tracks matching the filters of f; the sort
// and the page of f take no part. It scans the filtered tracks, as count does;
// fromTracks is needed for the joins of q and in_plan. No index
// (docs/track-stats-plan.md decision 8).
func (s *TrackQueries) Stats(ctx context.Context, f library.TrackFilter) (library.TrackStats, error) {
	where, args := trackWhere(f)
	sql := `SELECT count(*) AS total,
       count(*) FILTER (WHERE ` + condTrackReviewed + `) AS reviewed,
       count(*) FILTER (WHERE ` + condTrackHasIssues + `) AS with_issues,
       count(*) FILTER (WHERE ` + condTrackUnanalysed + `) AS unanalysed,
       COALESCE(sum(tr.air_duration_ms), 0) AS air_duration_ms` + fromTracks + whereClause(where)
	rows, err := s.q.Query(ctx, sql, args...)
	if err != nil {
		return library.TrackStats{}, fmt.Errorf("select track stats: %w", err)
	}
	r, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByName[trackStatsRow])
	if err != nil {
		return library.TrackStats{}, fmt.Errorf("scan track stats: %w", err)
	}
	return library.TrackStats(r), nil
}

// Get returns one track, or library.ErrTrackNotFound.
func (s *TrackQueries) Get(ctx context.Context, id uint64) (library.Track, error) {
	pgRows, err := s.q.Query(ctx, selectTracks+` WHERE tr.id = $1`, id)
	if err != nil {
		return library.Track{}, fmt.Errorf("select track %d: %w", id, err)
	}
	rows, err := pgx.CollectRows(pgRows, pgx.RowToStructByName[trackRow])
	if err != nil {
		return library.Track{}, fmt.Errorf("scan track %d: %w", id, err)
	}
	if len(rows) == 0 {
		return library.Track{}, library.ErrTrackNotFound
	}
	ts, err := s.withLinks(ctx, rows)
	if err != nil {
		return library.Track{}, err
	}
	return ts[0], nil
}

// withLinks adds the playlist and play rule IDs of the tracks, one query
// each (the primary keys of the link tables lead with track_id).
func (s *TrackQueries) withLinks(ctx context.Context, rows []trackRow) ([]library.Track, error) {
	if len(rows) == 0 {
		return []library.Track{}, nil
	}
	ids := make([]uint64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	playlists, err := s.links(ctx, `
SELECT track_id AS owner_id, playlist_id AS other_id FROM track_playlists
WHERE track_id = ANY($1) ORDER BY playlist_id`, ids)
	if err != nil {
		return nil, fmt.Errorf("select track playlists: %w", err)
	}
	rules, err := s.links(ctx, `
SELECT track_id AS owner_id, rule_id AS other_id FROM track_play_rules
WHERE track_id = ANY($1) ORDER BY rule_id`, ids)
	if err != nil {
		return nil, fmt.Errorf("select track play rules: %w", err)
	}
	byPlaylist, byRule := group(playlists), group(rules)

	out := make([]library.Track, len(rows))
	for i, r := range rows {
		t := library.Track{
			ID: r.ID, Title: r.Title, DurationMS: r.DurationMS, FilePath: r.FilePath,
			Format: r.Format, Bitrate: r.Bitrate, SampleRate: r.SampleRate,
			QualityOK: r.QualityOK, QualityNotes: r.QualityNotes,
			Type:         library.TypeRef{ID: r.TypeID, Code: r.TypeCode, Name: r.TypeName},
			FileChecksum: r.FileChecksum,
			CreatedAt:    r.CreatedAt.UTC(),
			PlaylistIDs:  byPlaylist[r.ID],
			PlayRuleIDs:  byRule[r.ID],

			Codec: r.Codec, Channels: r.Channels,
			LoudnessLUFS: r.LoudnessLUFS, TruePeakDBTP: r.TruePeakDBTP, LoudnessRangeLU: r.LoudnessRangeLU, SourceBitrate: r.SourceBitrate,
			QualityIssues: make([]library.QualityIssue, len(r.QualityIssues)),
		}
		for j, c := range r.QualityIssues {
			t.QualityIssues[j] = library.QualityIssue(c)
		}
		if r.AnalyzedAt != nil {
			t.AnalyzedAt = new(r.AnalyzedAt.UTC())
		}
		if r.ReviewedAt != nil {
			t.ReviewedAt = new(r.ReviewedAt.UTC())
		}
		t.Cue = library.Cue{InMS: r.CueInMS, OutMS: r.CueOutMS, FadeInMS: r.FadeInMS, FadeOutMS: r.FadeOutMS}
		if r.NextPlannedAt != nil {
			t.NextPlannedAt = new(r.NextPlannedAt.UTC())
		}
		if r.Queued {
			t.PlayoutState = library.PlayoutQueued
		}
		if r.ArtistID != nil && r.ArtistName != nil {
			t.Artist = &library.ArtistRef{ID: *r.ArtistID, Name: *r.ArtistName}
		}
		out[i] = t
	}
	return out, nil
}

// links reads the link rows of a query whose one parameter is the track IDs.
func (s *TrackQueries) links(ctx context.Context, sql string, ids []uint64) ([]linkRow, error) {
	rows, err := s.q.Query(ctx, sql, ids)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByName[linkRow])
}
