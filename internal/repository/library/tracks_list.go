package library

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

type trackRow struct {
	ID           uint64
	Title        string
	ArtistID     *uint64
	ArtistName   *string
	DurationMS   *int `gorm:"column:duration_ms"`
	FilePath     string
	Format       *string
	Bitrate      *int
	SampleRate   *int
	QualityOK    *bool `gorm:"column:quality_ok"`
	QualityNotes string
	TypeID       uint64
	TypeCode     string
	TypeName     string
	FileChecksum *string
	CreatedAt    time.Time

	Codec           *string
	Channels        *int
	LoudnessLUFS    *float64 `gorm:"column:loudness_lufs"`
	TruePeakDBTP    *float64 `gorm:"column:true_peak_dbtp"`
	LoudnessRangeLU *float64 `gorm:"column:loudness_range_lu"`
	SourceBitrate   *int
	// QualityIssues is the text[] joined with commas: database/sql does not
	// scan arrays. The codes contain no comma (ck_tracks_quality_issues).
	QualityIssues string
	AnalyzedAt    *time.Time
	ReviewedAt    *time.Time

	CueInMS   int  `gorm:"column:cue_in_ms"`
	CueOutMS  *int `gorm:"column:cue_out_ms"`
	FadeInMS  int  `gorm:"column:fade_in_ms"`
	FadeOutMS int  `gorm:"column:fade_out_ms"`

	NextPlannedAt *time.Time
	Queued        bool
	// Total is count(*) OVER () in List; zero elsewhere.
	Total int
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
// conditions are fixed strings; values go in args.
func trackWhere(f library.TrackFilter) (where []string, args []any) {
	if f.TypeID != 0 {
		where = append(where, "tr.type_id = ?")
		args = append(args, f.TypeID)
	}
	if f.ArtistID != 0 {
		where = append(where, "tr.artist_id = ?")
		args = append(args, f.ArtistID)
	}
	if f.PlaylistID != 0 {
		where = append(where, "EXISTS (SELECT 1 FROM track_playlists tp WHERE tp.track_id = tr.id AND tp.playlist_id = ?)")
		args = append(args, f.PlaylistID)
	}
	if f.Query != "" {
		p := containsPattern(f.Query)
		where = append(where, "(tr.title ILIKE ? OR a.name ILIKE ?)")
		args = append(args, p, p)
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
func (s *Tracks) List(ctx context.Context, f library.TrackFilter) ([]library.Track, int, error) {
	order, err := trackOrder(f)
	if err != nil {
		return nil, 0, err
	}
	where, args := trackWhere(f)
	if f.After != nil {
		where = append(where, "(tr.title, tr.id) > (?, ?)")
		args = append(args, f.After.Title, f.After.ID)
	}
	sql := "SELECT " + trackSelect + `, p.total
FROM (SELECT tr.id, count(*) OVER () AS total` + fromTracks + whereClause(where) + `
      ORDER BY ` + order + ` LIMIT ? OFFSET ?) p
JOIN tracks tr ON tr.id = p.id
JOIN playlist_types t ON t.id = tr.type_id
LEFT JOIN artists a ON a.id = tr.artist_id` + planUsage + `
ORDER BY ` + order
	args = append(args, f.Limit, f.Offset)

	var rows []trackRow
	if err = s.g.WithContext(ctx).Raw(sql, args...).Scan(&rows).Error; err != nil {
		return nil, 0, fmt.Errorf("select tracks: %w", err)
	}
	total := 0
	if len(rows) > 0 {
		total = rows[0].Total
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
func (s *Tracks) count(ctx context.Context, f library.TrackFilter) (int, error) {
	where, args := trackWhere(f)
	var n int
	if err := s.g.WithContext(ctx).Raw("SELECT count(*)"+fromTracks+whereClause(where), args...).Scan(&n).Error; err != nil {
		return 0, fmt.Errorf("count tracks: %w", err)
	}
	return n, nil
}

// trackStatsRow is the one row of Stats.
type trackStatsRow struct {
	Total         int
	Reviewed      int
	WithIssues    int
	Unanalysed    int
	AirDurationMS int64 `gorm:"column:air_duration_ms"`
}

// Stats returns the figures of the tracks matching the filters of f; the sort
// and the page of f take no part. It scans the filtered tracks, as count does;
// fromTracks is needed for the joins of q and in_plan. No index
// (docs/track-stats-plan.md decision 8).
func (s *Tracks) Stats(ctx context.Context, f library.TrackFilter) (library.TrackStats, error) {
	where, args := trackWhere(f)
	sql := `SELECT count(*) AS total,
       count(*) FILTER (WHERE ` + condTrackReviewed + `) AS reviewed,
       count(*) FILTER (WHERE ` + condTrackHasIssues + `) AS with_issues,
       count(*) FILTER (WHERE ` + condTrackUnanalysed + `) AS unanalysed,
       COALESCE(sum(tr.air_duration_ms), 0) AS air_duration_ms` + fromTracks + whereClause(where)
	var r trackStatsRow
	if err := s.g.WithContext(ctx).Raw(sql, args...).Scan(&r).Error; err != nil {
		return library.TrackStats{}, fmt.Errorf("select track stats: %w", err)
	}
	return library.TrackStats(r), nil
}

// Get returns one track, or library.ErrTrackNotFound.
func (s *Tracks) Get(ctx context.Context, id uint64) (library.Track, error) {
	var rows []trackRow
	if err := s.g.WithContext(ctx).Raw(selectTracks+` WHERE tr.id = ?`, id).Scan(&rows).Error; err != nil {
		return library.Track{}, fmt.Errorf("select track %d: %w", id, err)
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
func (s *Tracks) withLinks(ctx context.Context, rows []trackRow) ([]library.Track, error) {
	if len(rows) == 0 {
		return []library.Track{}, nil
	}
	ids := make([]uint64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	g := s.g.WithContext(ctx)
	var playlists, rules []linkRow
	err := g.Raw(`
SELECT track_id AS owner_id, playlist_id AS other_id FROM track_playlists
WHERE track_id IN ? ORDER BY playlist_id`, ids).Scan(&playlists).Error
	if err != nil {
		return nil, fmt.Errorf("select track playlists: %w", err)
	}
	err = g.Raw(`
SELECT track_id AS owner_id, rule_id AS other_id FROM track_play_rules
WHERE track_id IN ? ORDER BY rule_id`, ids).Scan(&rules).Error
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
			QualityIssues: issues(r.QualityIssues),
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

// issues splits the joined quality_issues column.
func issues(joined string) []library.QualityIssue {
	out := []library.QualityIssue{}
	if joined == "" {
		return out
	}
	for c := range strings.SplitSeq(joined, ",") {
		out = append(out, library.QualityIssue(c))
	}
	return out
}
