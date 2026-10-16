package dto

import "time"

// ImportCounts are the audio files of a folder and its subfolders.
type ImportCounts struct {
	AudioFiles int `json:"audio_files" doc:"Files an import takes by name: an audio extension (mp3, m4a, aac, ogg, oga, flac, wav), not hidden, not a temporary file."`
	NewFiles   int `json:"new_files" doc:"Audio files that are not a track yet by path and not the original of a transcoded copy. A file with the content of an existing track is counted here and reported as a duplicate by the import."`
}

// ImportFolder is a folder under the media root.
type ImportFolder struct {
	Path string `json:"path" doc:"Relative to the media root, with forward slashes (music/rock), the same form as a track's file_path."`
	ImportCounts
}

// ImportFolders are the folders under the media root.
type ImportFolders struct {
	Root    ImportCounts   `json:"root" doc:"The counts of the whole media root."`
	Folders []ImportFolder `json:"folders" nullable:"false" doc:"Every folder, nested ones included, in path order: a folder before its subfolders. Hidden folders and symbolic links are not listed."`
	More    bool           `json:"more" doc:"The media root has more folders than listed (at most 500)."`
}

// ImportFile is a file of a folder.
type ImportFile struct {
	Path       string    `json:"path" doc:"Relative to the media root, with forward slashes."`
	Size       int64     `json:"size" doc:"Bytes."`
	ModifiedAt time.Time `json:"modified_at"`
	Status     string    `json:"status" enum:"new,in_library,original,not_audio" doc:"new: an import would take it. in_library: a track has this path. original: the original of a transcoded copy (<name>.optimal.<ext> next to it is a track). not_audio: an import skips it."`
	TrackID    *uint64   `json:"track_id,omitempty" doc:"in_library: the track of the file. original: the track of its copy."`
}

// ImportFiles are the files of a folder and its subfolders.
type ImportFiles struct {
	Folder string       `json:"folder" doc:"The folder relative to the media root; empty for the media root."`
	Items  []ImportFile `json:"items" nullable:"false" doc:"In path order, at most 2,000. Hidden files are not listed."`
	More   bool         `json:"more" doc:"The folder holds more files than listed."`
}

// ImportBody is the body of POST /api/import.
type ImportBody struct {
	Folder      string   `json:"folder,omitempty" maxLength:"1024" doc:"Folder of the media root to import, with its subfolders (music, music/rock); absent or empty: the whole media root."`
	TypeID      uint64   `json:"type_id" minimum:"1" doc:"Playlist type of every new track."`
	PlaylistIDs []uint64 `json:"playlist_ids,omitempty" uniqueItems:"true" maxItems:"1000" doc:"Playlists every new track joins."`
	ArtistID    *uint64  `json:"artist_id,omitempty" minimum:"1" doc:"An existing artist for every new track; it wins over the artist tag and the file name."`
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
