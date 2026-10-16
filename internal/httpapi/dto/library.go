package dto

import "time"

// PlaylistType labels playlists and tracks (§4). It is only a label: it
// does not limit what a clock element can combine.
type PlaylistType struct {
	ID            uint64    `json:"id"`
	Code          string    `json:"code" doc:"Unique slug; never changes after creation."`
	Name          string    `json:"name" doc:"Display name."`
	IsSystem      bool      `json:"is_system" doc:"True for music, jingle, news, and segment: renamable, never deletable."`
	PlaylistCount int       `json:"playlist_count" doc:"Playlists of this type."`
	TrackCount    int       `json:"track_count" doc:"Tracks of this type."`
	CreatedAt     time.Time `json:"created_at"`
}

// PlaylistTypeList is every playlist type in name order.
type PlaylistTypeList struct {
	Items []PlaylistType `json:"items" nullable:"false"`
}

// CreatePlaylistType is the body of POST /api/playlist-types.
type CreatePlaylistType struct {
	Code string `json:"code" pattern:"^[a-z][a-z0-9_-]{0,62}$" doc:"Unique slug, for example promo. Cannot be changed later."`
	Name string `json:"name" minLength:"1" maxLength:"100" doc:"Display name."`
}

// RenamePlaylistType is the body of PUT /api/playlist-types/{id}.
type RenamePlaylistType struct {
	Name string `json:"name" minLength:"1" maxLength:"100" doc:"New display name. The code does not change."`
}

// TypeRef is the type of a playlist or a track.
type TypeRef struct {
	ID   uint64 `json:"id"`
	Code string `json:"code"`
	Name string `json:"name"`
}

// Playlist is a pool of tracks: the order of its tracks is not stored, and
// rotation chooses the track (§4).
type Playlist struct {
	ID          uint64    `json:"id"`
	Name        string    `json:"name"`
	Type        TypeRef   `json:"type"`
	TrackCount  int       `json:"track_count"`
	PlayRuleIDs []uint64  `json:"play_rule_ids" nullable:"false" doc:"Play rules applied to every track of the playlist, in ID order."`
	CreatedAt   time.Time `json:"created_at"`
}

// PlaylistList is the playlists in name order.
type PlaylistList struct {
	Items []Playlist `json:"items" nullable:"false"`
}

// PlaylistBody is the body of POST /api/playlists and PUT /api/playlists/{id}.
type PlaylistBody struct {
	Name   string `json:"name" minLength:"1" maxLength:"200"`
	TypeID uint64 `json:"type_id" minimum:"1" doc:"Playlist type (GET /api/playlist-types)."`
}

// SetPlaylistTracks is the body of PUT /api/playlists/{id}/tracks.
type SetPlaylistTracks struct {
	TrackIDs []uint64 `json:"track_ids" nullable:"false" uniqueItems:"true" maxItems:"100000" doc:"Every track of the playlist; tracks not listed are removed from it. The order is not stored."`
}

// SetPlayRules is the body of PUT /api/tracks/{id}/play-rules and
// PUT /api/playlists/{id}/play-rules.
type SetPlayRules struct {
	RuleIDs []uint64 `json:"rule_ids" nullable:"false" uniqueItems:"true" maxItems:"1000" doc:"Every play rule of the target; rules not listed are removed from it."`
}

// PlayRuleIDs is the rules of a track or a playlist after a change.
type PlayRuleIDs struct {
	RuleIDs []uint64 `json:"rule_ids" nullable:"false" doc:"In ID order."`
}

// Artist is used for artist separation.
type Artist struct {
	ID         uint64 `json:"id"`
	Name       string `json:"name"`
	TrackCount int    `json:"track_count"`
}

// ArtistList is the artists in name order.
type ArtistList struct {
	Items []Artist `json:"items" nullable:"false"`
}

// ArtistBody is the body of POST /api/artists and PUT /api/artists/{id}.
type ArtistBody struct {
	Name string `json:"name" minLength:"1" maxLength:"200" doc:"Unique: artist separation counts plays per artist."`
}

// ArtistRef is the artist of a track.
type ArtistRef struct {
	ID   uint64 `json:"id"`
	Name string `json:"name"`
}

// Track is a file in the library and its metadata.
type Track struct {
	ID           uint64     `json:"id"`
	Title        string     `json:"title"`
	Artist       *ArtistRef `json:"artist,omitempty" doc:"Absent for material without an artist (jingles, station IDs)."`
	DurationMS   *int       `json:"duration_ms,omitempty" doc:"Absent until entered or read from the file; such a track cannot be planned."`
	FilePath     string     `json:"file_path" doc:"Relative to the media root, with forward slashes."`
	Format       *string    `json:"format,omitempty" enum:"mp3,aac,m4a,ogg,flac,wav"`
	Bitrate      *int       `json:"bitrate,omitempty" doc:"Bits per second."`
	SampleRate   *int       `json:"sample_rate,omitempty" doc:"Hz."`
	QualityOK    *bool      `json:"quality_ok,omitempty" doc:"Absent until the file is analysed."`
	QualityNotes string     `json:"quality_notes"`
	Type         TypeRef    `json:"type"`
	FileChecksum *string    `json:"file_checksum,omitempty"`
	PlaylistIDs  []uint64   `json:"playlist_ids" nullable:"false" doc:"In ID order."`
	PlayRuleIDs  []uint64   `json:"play_rule_ids" nullable:"false" doc:"Rules of the track itself, in ID order. The rules of its playlists apply too."`
	CreatedAt    time.Time  `json:"created_at"`

	Codec           *string    `json:"codec,omitempty" doc:"Codec of the audio stream as ffprobe names it (mp3, aac, vorbis, flac, pcm_s16le, ...). Absent until analysed."`
	Channels        *int       `json:"channels,omitempty"`
	LoudnessLUFS    *float64   `json:"loudness_lufs,omitempty" doc:"Integrated loudness (EBU R128). Absent until analysed, and for a silent file."`
	TruePeakDBTP    *float64   `json:"true_peak_dbtp,omitempty"`
	LoudnessRangeLU *float64   `json:"loudness_range_lu,omitempty"`
	SourceBitrate   *int       `json:"source_bitrate,omitempty" doc:"Bits per second of the lossy file this one was transcoded from; it keeps the low_bitrate issue."`
	QualityIssues   []string   `json:"quality_issues" nullable:"false" enum:"format,sample_rate,channels,loudness,true_peak,low_bitrate,file_missing" doc:"Problems found by the last analysis against the broadcast standard (settings import.*). low_bitrate and file_missing cannot be fixed by a transcode."`
	Fixes           []string   `json:"fixes" nullable:"false" enum:"loudness,optimize" doc:"Transcodes that remove at least one issue: the buttons Fix loudness and Optimize track (POST /api/tracks/transcode)."`
	AnalyzedAt      *time.Time `json:"analyzed_at,omitempty" doc:"Absent until the file is analysed."`
	ReviewedAt      *time.Time `json:"reviewed_at,omitempty" doc:"When the manager marked the track checked (POST /api/tracks/review); absent when it is not. A change of the title, the artist, the file path, or the file itself (a transcode, a replaced file) clears it."`

	CueInMS       int        `json:"cue_in_ms" minimum:"0" doc:"Start of the part that goes on air, from the start of the file (PUT /api/tracks/{id}/cue). 0: the start of the file."`
	CueOutMS      *int       `json:"cue_out_ms,omitempty" doc:"End of the part that goes on air, from the start of the file; absent: the end of the file."`
	FadeInMS      int        `json:"fade_in_ms" minimum:"0" maximum:"5000" doc:"Fade in at the cue in point; 0 is a hard cut."`
	FadeOutMS     int        `json:"fade_out_ms" minimum:"0" maximum:"5000" doc:"Fade out ending at the cue out point; 0 is a hard cut. A track cut at the end of its segment fades out by liquidsoap.fade_out_ms instead."`
	AirDurationMS *int       `json:"air_duration_ms,omitempty" doc:"Length on air: cue out (or the duration) minus cue in. The planner fits the track by it. Absent while the duration is unknown."`
	NextPlannedAt *time.Time `json:"next_planned_at,omitempty" doc:"Earliest planned start of the track in the track plan (planned or queued items); absent when it is not planned."`
	PlayoutState  string     `json:"playout_state,omitempty" enum:"on_air,queued" doc:"on_air: the track is on air; queued: a plan item of it waits in Liquidsoap's queue. Absent otherwise. The panel reloads on the playout and plan events."`
}

// TrackCueBody is the body of PUT /api/tracks/{id}/cue: the whole cue,
// every field required.
type TrackCueBody struct {
	CueInMS   int  `json:"cue_in_ms" minimum:"0" maximum:"21600000" doc:"Start of the on-air part, from the start of the file; before the end of the file."`
	CueOutMS  *int `json:"cue_out_ms" nullable:"true" minimum:"1" maximum:"21600000" doc:"End of the on-air part, after cue_in_ms and not past the end of the file; null: the end of the file. A cut must leave at least 5 s on air."`
	FadeInMS  int  `json:"fade_in_ms" minimum:"0" maximum:"5000" doc:"Fade in at cue in; 0 is a hard cut. The panel proposes liquidsoap.cue_fade_in_ms."`
	FadeOutMS int  `json:"fade_out_ms" minimum:"0" maximum:"5000" doc:"Fade out ending at cue out; 0 is a hard cut. The fades together must not be longer than the on-air part. The panel proposes liquidsoap.cue_fade_out_ms."`
}

// TrackCueResult is the response of PUT /api/tracks/{id}/cue.
type TrackCueResult struct {
	Track Track `json:"track"`
	// BuildOperationID is absent when the cut did not change (fades only),
	// the track is not planned, or the rebuild could not be started.
	BuildOperationID *uint64 `json:"build_operation_id,omitempty" doc:"The clock_build operation that rebuilds the future hours holding a planned item of the track, so they use the new cut. Absent when only fades changed or the track is not planned."`
}

// TrackWaveform is the waveform of a track file.
type TrackWaveform struct {
	DurationMS int    `json:"duration_ms" minimum:"0" doc:"Length of the decoded audio."`
	BucketMS   int    `json:"bucket_ms" minimum:"1" doc:"Length of one bucket."`
	Peaks      []int8 `json:"peaks" nullable:"false" doc:"The min and max amplitude of every bucket of a mono mix, -127 to 127 (linear; 127 is full scale, 0 dBFS), flat: [min0, max0, min1, max1, ...]. The last bucket may be partial."`
	MaxPeak    int    `json:"max_peak" minimum:"0" maximum:"127" doc:"The largest absolute value in peaks, so the drawing can be scaled to it (0 for silence). In dBFS: 20 × log10(max_peak / 127)."`
}

// TrackPage is a page of tracks in the requested order.
type TrackPage struct {
	Items      []Track `json:"items" nullable:"false"`
	NextCursor string  `json:"next_cursor,omitempty" doc:"Pass as cursor to read the next page; set only in the default order (title, asc), absent on the last page."`
	Total      int     `json:"total" minimum:"0" doc:"Tracks matching the filters, on every page."`
}

// TrackBody is the body of POST /api/tracks and PUT /api/tracks/{id}. PUT
// replaces the whole track: an optional field left out is cleared. Metadata
// left out (duration, format, bitrate, sample rate) is read from the file
// when it exists.
type TrackBody struct {
	Title        string   `json:"title" minLength:"1" maxLength:"500"`
	ArtistID     *uint64  `json:"artist_id,omitempty" nullable:"true" minimum:"1"`
	FilePath     string   `json:"file_path" minLength:"1" maxLength:"1024" doc:"Relative to the media root, with forward slashes, for example music/song.mp3. No absolute paths, backslashes, or .. elements."`
	TypeID       uint64   `json:"type_id" minimum:"1" doc:"Playlist type of the track (GET /api/playlist-types)."`
	DurationMS   *int     `json:"duration_ms,omitempty" nullable:"true" minimum:"1" maximum:"21600000" doc:"At most 6 hours."`
	Format       *string  `json:"format,omitempty" nullable:"true" enum:"mp3,aac,m4a,ogg,flac,wav"`
	Bitrate      *int     `json:"bitrate,omitempty" nullable:"true" minimum:"1" maximum:"10000000" doc:"Bits per second."`
	SampleRate   *int     `json:"sample_rate,omitempty" nullable:"true" minimum:"1" maximum:"768000" doc:"Hz."`
	QualityOK    *bool    `json:"quality_ok,omitempty" nullable:"true" doc:"Manual override of the quality check."`
	QualityNotes string   `json:"quality_notes,omitempty" maxLength:"2000"`
	PlaylistIDs  []uint64 `json:"playlist_ids" nullable:"false" uniqueItems:"true" maxItems:"1000" doc:"Every playlist of the track; playlists not listed lose it."`
}

// DeleteTracksBody is the body of POST /api/tracks/delete.
type DeleteTracksBody struct {
	TrackIDs []uint64 `json:"track_ids" nullable:"false" minItems:"1" maxItems:"1000" uniqueItems:"true" doc:"Tracks to delete, in the order they are deleted."`
}

// DeleteTracksResult lists what a bulk delete removed and what it refused.
type DeleteTracksResult struct {
	Deleted []uint64       `json:"deleted" nullable:"false" doc:"Deleted tracks, in request order."`
	Refused []TrackRefusal `json:"refused" nullable:"false" doc:"Tracks not deleted, in request order."`
}

// TrackRefusal is a track a bulk delete did not remove.
type TrackRefusal struct {
	TrackID uint64      `json:"track_id"`
	Code    string      `json:"code" enum:"track_in_use,track_has_history,track_not_found" doc:"Why the track was kept; the codes of DELETE /api/tracks/{id}."`
	Title   string      `json:"title,omitempty" doc:"Title of the track; absent for track_not_found."`
	UsedBy  []EntityRef `json:"used_by,omitempty" nullable:"false" doc:"Clocks whose filler pool holds the track (track_in_use)."`
}

// ReviewTracksBody is the body of POST /api/tracks/review.
type ReviewTracksBody struct {
	TrackIDs []uint64 `json:"track_ids" nullable:"false" minItems:"1" maxItems:"1000" uniqueItems:"true"`
	Reviewed bool     `json:"reviewed" doc:"true marks the tracks reviewed (a track already reviewed keeps the time of its first mark); false clears the mark."`
}

// ReviewTracksResult lists the tracks whose mark was set or cleared.
type ReviewTracksResult struct {
	Updated  []uint64 `json:"updated" nullable:"false" doc:"Tracks marked or cleared, in request order."`
	NotFound []uint64 `json:"not_found" nullable:"false" doc:"Tracks that do not exist, in request order."`
}

// RenameTracksBody is the body of POST /api/tracks/rename.
type RenameTracksBody struct {
	TrackIDs  []uint64 `json:"track_ids" nullable:"false" minItems:"1" maxItems:"1000" uniqueItems:"true"`
	Find      string   `json:"find" minLength:"1" maxLength:"200" doc:"Text to find in the titles, literally (no pattern characters); not only white space."`
	Replace   string   `json:"replace,omitempty" maxLength:"200" doc:"Text that replaces every occurrence; empty or absent removes it."`
	MatchCase bool     `json:"match_case,omitempty" doc:"Match the case of find; by default the match ignores case."`
	DryRun    bool     `json:"dry_run" doc:"true returns the preview and writes nothing; false writes the titles."`
}

// RenameTracksResult is the outcome of a rename, each list in request
// order.
type RenameTracksResult struct {
	Changed   []TitleChange   `json:"changed" nullable:"false" doc:"Tracks whose title changes (dry run) or changed."`
	Unchanged []uint64        `json:"unchanged" nullable:"false" doc:"Tracks whose title has no occurrence of find, or stays the same."`
	Refused   []RenameRefusal `json:"refused" nullable:"false" doc:"Tracks left alone, with the reason."`
}

// TitleChange is a title a rename changes.
type TitleChange struct {
	TrackID  uint64 `json:"track_id"`
	OldTitle string `json:"old_title"`
	NewTitle string `json:"new_title"`
}

// RenameRefusal is a track a rename leaves alone.
type RenameRefusal struct {
	TrackID uint64 `json:"track_id"`
	Code    string `json:"code" enum:"track_not_found,title_empty,title_too_long" doc:"title_empty: nothing would be left of the title; title_too_long: the title would be over 500 characters (it is not cut)."`
}
