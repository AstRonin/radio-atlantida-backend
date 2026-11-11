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
	Path       string           `json:"path" doc:"Relative to the media root, with forward slashes."`
	Size       int64            `json:"size" doc:"Bytes."`
	ModifiedAt time.Time        `json:"modified_at"`
	Status     string           `json:"status" enum:"new,in_library,original,not_audio" doc:"new: an import would take it. in_library: a track has this path. original: the original of a transcoded copy (<name>.optimal.<ext> next to it is a track). not_audio: an import skips it."`
	TrackID    *uint64          `json:"track_id,omitempty" doc:"in_library: the track of the file. original: the track of its copy."`
	Track      *ImportFileTrack `json:"track,omitempty" doc:"in_library and original: the track of track_id as the library has it, read from the database (no file is read). For original the facts describe the library copy (<name>.optimal.<ext>), not this file. Absent for new and not_audio."`
	NameTitle  *string          `json:"name_title,omitempty" doc:"new only: the title an import reads from the file name (the base name without the extension, or the title part of \"Artist - Title\" with exactly one hyphen, en dash, or em dash). The import prefers the title tag; this is its fallback. Absent for other statuses."`
	NameArtist *string          `json:"name_artist,omitempty" nullable:"true" doc:"new only: the artist an import reads from the file name (\"Artist - Title\" with exactly one hyphen); null when the name gives none. The import prefers artist_id of the request, then the artist tag; this is their fallback. Absent for other statuses."`
}

// ImportFileTrack is the track of a listed file, from the database.
type ImportFileTrack struct {
	ID            uint64     `json:"id"`
	Title         string     `json:"title"`
	Artist        *string    `json:"artist" nullable:"true" doc:"The artist's name; null for a track without an artist."`
	DurationMS    *int       `json:"duration_ms" nullable:"true" doc:"Null until read from the file."`
	AirDurationMS *int       `json:"air_duration_ms" nullable:"true" doc:"Length on air: cue out (or the duration) minus cue in. Null while the duration is unknown."`
	Format        *string    `json:"format" nullable:"true" enum:"mp3,aac,m4a,ogg,flac,wav"`
	Codec         *string    `json:"codec" nullable:"true" doc:"Codec of the audio stream as ffprobe names it (mp3, aac, vorbis, flac, pcm_s16le, ...)."`
	Bitrate       *int       `json:"bitrate" nullable:"true" doc:"Bits per second."`
	SampleRate    *int       `json:"sample_rate" nullable:"true" doc:"Hz."`
	Channels      *int       `json:"channels" nullable:"true"`
	QualityIssues []string   `json:"quality_issues" nullable:"false" enum:"format,sample_rate,channels,loudness,true_peak,low_bitrate,file_missing" doc:"Problems found by the last analysis, as on the track."`
	ReviewedAt    *time.Time `json:"reviewed_at" nullable:"true" doc:"When the manager marked the track checked; null when it is not."`
}

// ImportFileTags are the title and artist tags of a file.
type ImportFileTags struct {
	Title  *string `json:"title" nullable:"true" doc:"The title tag; null when the file has none."`
	Artist *string `json:"artist" nullable:"true" doc:"The artist tag; null when the file has none."`
}

// ImportFileInfo is the response of GET /api/import/file-info: one probe
// (header read) of a file, before it is imported.
type ImportFileInfo struct {
	Path         string         `json:"path" doc:"Relative to the media root, with forward slashes."`
	Size         int64          `json:"size" doc:"Bytes."`
	ModifiedAt   time.Time      `json:"modified_at"`
	DurationMS   int            `json:"duration_ms" minimum:"1" doc:"A file without a duration is refused with probe_failed: an import skips it as unreadable."`
	Format       *string        `json:"format" nullable:"true" enum:"mp3,aac,m4a,ogg,flac,wav" doc:"The container; null when it is none of these."`
	Codec        *string        `json:"codec" nullable:"true" doc:"Codec of the audio stream as ffprobe names it (mp3, aac, vorbis, flac, pcm_s16le, ...)."`
	Bitrate      *int           `json:"bitrate" nullable:"true" doc:"Bits per second; null when ffprobe does not report it."`
	SampleRate   *int           `json:"sample_rate" nullable:"true" doc:"Hz."`
	Channels     *int           `json:"channels" nullable:"true"`
	Tags         ImportFileTags `json:"tags"`
	Title        string         `json:"title" doc:"The title an import would give the track: the title tag (cleaned: no control characters, single spaces), else from the file name. Cut to 500 characters."`
	TitleSource  string         `json:"title_source" enum:"tag,file_name"`
	Artist       *string        `json:"artist" nullable:"true" doc:"The artist an import would give the track: the artist tag, else from the file name (\"Artist - Title\" with exactly one hyphen); null when neither gives one. artist_id of the import request overrides it."`
	ArtistSource string         `json:"artist_source" enum:"tag,file_name,none"`
	Issues       []string       `json:"issues" nullable:"false" enum:"format,sample_rate,channels,low_bitrate" doc:"Quality issues the probe shows against the broadcast standard (settings import.*), with the codes of a track's quality_issues. Loudness and true peak need a decode: the analysis after the import measures them."`
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
