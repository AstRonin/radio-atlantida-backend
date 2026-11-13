package library

const selectTracks = `
SELECT ` + trackSelect + fromTracks

const trackSelect = `tr.id, tr.title, tr.artist_id, a.name AS artist_name, tr.duration_ms, tr.file_path,
       tr.format, tr.bitrate, tr.sample_rate, tr.quality_ok, tr.quality_notes,
       t.id AS type_id, t.code AS type_code, t.name AS type_name, tr.file_checksum, tr.created_at,
       tr.codec, tr.channels, tr.loudness_lufs, tr.true_peak_dbtp, tr.loudness_range_lu, tr.source_bitrate,
       tr.quality_issues, tr.analyzed_at, tr.reviewed_at,
       tr.cue_in_ms, tr.cue_out_ms, tr.fade_in_ms, tr.fade_out_ms,
       np.next_planned_at, COALESCE(np.queued, false) AS queued`

// planUsage joins the plan use of each track: the earliest start of a
// planned or queued item, and whether one is queued. scheduled_items holds
// only the plan horizon (hundreds of rows), so it is aggregated once and
// hash-joined; a subquery per track would probe the index for every track
// of the library.
const planUsage = `
LEFT JOIN (SELECT si.track_id, min(si.planned_start_utc) AS next_planned_at, bool_or(si.status = 'queued') AS queued
           FROM scheduled_items si WHERE si.status IN ('planned', 'queued')
           GROUP BY si.track_id) np ON np.track_id = tr.id`

const fromTracks = `
FROM tracks tr
JOIN playlist_types t ON t.id = tr.type_id
LEFT JOIN artists a ON a.id = tr.artist_id` + planUsage

// usersOfTracks lists the clocks with an insert of each track and the
// one-time inserts of each track, named by their hour (UTC), at most $2 per
// track (ix_clock_inserts_track_id, ix_hour_inserts_track_id).
const usersOfTracks = `
SELECT track_id, kind, id, name
FROM (
    SELECT u.*, row_number() OVER (PARTITION BY u.track_id ORDER BY u.kind, u.name, u.id) AS n
    FROM (
        SELECT DISTINCT i.track_id, 'clock' AS kind, c.id, c.name
        FROM clock_inserts i JOIN clocks c ON c.id = i.clock_id
        WHERE i.track_id = ANY($1)
        UNION ALL
        SELECT h.track_id, 'hour_insert' AS kind, h.id, to_char(h.hour_start_utc AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI"Z"') AS name
        FROM hour_inserts h
        WHERE h.track_id = ANY($1)
    ) u
) n
WHERE n <= $2
ORDER BY track_id, n`
