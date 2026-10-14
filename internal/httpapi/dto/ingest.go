package dto

import "time"

// InboxFile is a file waiting in the inbox folder.
type InboxFile struct {
	Path       string    `json:"path" doc:"Relative to the media root, with forward slashes."`
	Size       int64     `json:"size" doc:"Bytes."`
	ModifiedAt time.Time `json:"modified_at"`
}

// Inbox is the content of the inbox folder.
type Inbox struct {
	Dir   string      `json:"dir" doc:"The inbox folder relative to the media root (setting import.inbox_dir)."`
	Items []InboxFile `json:"items" nullable:"false" doc:"In path order. Hidden files are not listed; files that are not audio are listed and skipped by an import."`
	More  bool        `json:"more" doc:"The folder holds more files than listed."`
}

// ImportBody is the body of POST /api/import.
type ImportBody struct {
	TypeID      uint64   `json:"type_id" minimum:"1" doc:"Playlist type of every imported track."`
	PlaylistIDs []uint64 `json:"playlist_ids,omitempty" uniqueItems:"true" maxItems:"1000" doc:"Playlists every imported track joins."`
}

// AnalyseBody is the body of POST /api/tracks/analyse.
type AnalyseBody struct {
	TrackIDs []uint64 `json:"track_ids,omitempty" uniqueItems:"true" maxItems:"1000" doc:"Tracks to analyse; absent or empty: every track never analysed."`
	Force    bool     `json:"force,omitempty" doc:"Decode files that have not changed since their last analysis. Without it, their issues are only checked again against the current settings."`
}

// TranscodeBody is the body of POST /api/tracks/transcode.
type TranscodeBody struct {
	TrackIDs []uint64 `json:"track_ids" nullable:"false" minItems:"1" maxItems:"200" uniqueItems:"true"`
	Fix      string   `json:"fix" enum:"loudness,optimize" doc:"loudness: bring the loudness to stream.loudness_target_lufs, keeping the format. optimize: convert to the broadcast standard (settings import.target_*) and fix the loudness."`
	Output   string   `json:"output" enum:"replace,copy" doc:"replace: the new file replaces the original. copy: the new file is written as <name>.optimal.<ext> next to the original, which stays on disk; the track then plays the copy."`
}
