package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Apply tells when a saved value takes effect (§6.3).
type Apply string

// Apply modes.
const (
	// ApplyImmediate: the next use reads the new value.
	ApplyImmediate Apply = "immediate"
	// ApplyReconnect: the value takes effect when the connection it
	// configures is opened again; changing the stream server briefly
	// interrupts the stream.
	ApplyReconnect Apply = "reconnect"
)

// Type is the JSON type of a value.
type Type string

// Value types.
const (
	TypeInteger Type = "integer"
	TypeNumber  Type = "number"
	TypeString  Type = "string"
	TypeBoolean Type = "boolean"
	TypeObject  Type = "object"
)

// Group is the settings page a key belongs to. It is also the first part
// of the key.
type Group string

// Groups, in the order of §6.3.
const (
	GroupStation    Group = "station"
	GroupStream     Group = "stream"
	GroupLiquidsoap Group = "liquidsoap"
	GroupRotation   Group = "rotation"
	GroupFiller     Group = "filler"
	GroupOutage     Group = "outage"
	GroupRetention  Group = "retention"
	GroupPlayout    Group = "playout"
	GroupImport     Group = "import"
)

// Definition describes one setting. Definitions are built by the Registry
// and never changed afterwards.
type Definition struct {
	Key         string
	Group       Group
	Description string
	Type        Type
	// Unit of a number, for example ms, seconds, kbps (empty if none).
	Unit string
	// Default is the value used until one is saved. Its Go type is the
	// type of the Key that defines it.
	Default  any
	Nullable bool
	// Minimum and Maximum bound a number, or the length of a string.
	Minimum *float64
	Maximum *float64
	// Enum lists the allowed values, if limited.
	Enum       []any
	Apply      Apply
	AffectsAir bool

	// Secret marks a value that lives only in the environment variable
	// EnvName. It is never stored, returned, or logged.
	Secret  bool
	EnvName string

	// parse decodes and validates a JSON value into the Go type of Default.
	parse func(raw json.RawMessage) (any, error)
}

// Key is the typed handle of a setting; Value and OnChange use it to read
// and watch the value as T.
type Key[T any] struct{ name string }

// Name returns the key.
func (k Key[T]) Name() string { return k.name }

// Keys of the settings, grouped as in §6.3. Their definitions are in
// NewRegistry.
var (
	StationTimezone = Key[string]{"station.timezone"}
	StationName     = Key[string]{"station.name"}

	StreamIcecastHost  = Key[string]{"stream.icecast_host"}
	StreamIcecastPort  = Key[int]{"stream.icecast_port"}
	StreamMount        = Key[string]{"stream.mount"}
	StreamCodec        = Key[string]{"stream.codec"}
	StreamBitrateKbps  = Key[int]{"stream.bitrate_kbps"}
	StreamSampleRateHz = Key[int]{"stream.sample_rate_hz"}
	StreamChannels     = Key[int]{"stream.channels"}
	StreamLoudnessLUFS = Key[float64]{"stream.loudness_target_lufs"}
	StreamName         = Key[string]{"stream.name"}
	StreamDescription  = Key[string]{"stream.description"}
	StreamGenre        = Key[string]{"stream.genre"}
	StreamHLSEnabled   = Key[bool]{"stream.hls_enabled"}

	LiquidsoapMusicGain   = Key[float64]{"liquidsoap.music_gain"}
	LiquidsoapDuckGain    = Key[float64]{"liquidsoap.duck_gain"}
	LiquidsoapFadeOutMS   = Key[int]{"liquidsoap.fade_out_ms"}
	LiquidsoapHarborPort  = Key[int]{"liquidsoap.harbor_port"}
	LiquidsoapControlHost = Key[string]{"liquidsoap.control_host"}
	LiquidsoapControlPort = Key[int]{"liquidsoap.control_port"}
	LiquidsoapMediaRoot   = Key[string]{"liquidsoap.media_root"}

	RotationGridHorizonDays       = Key[int]{"rotation.grid_horizon_days"}
	RotationPlanHorizonHours      = Key[int]{"rotation.plan_horizon_hours"}
	RotationLookaheadItems        = Key[int]{"rotation.lookahead_items"}
	RotationDurationToleranceMS   = Key[int]{"rotation.default_duration_tolerance_ms"}
	RotationFixedToleranceMS      = Key[int]{"rotation.default_fixed_tolerance_ms"}
	RotationMinFillerGapMS        = Key[int]{"rotation.min_filler_gap_ms"}
	RotationMaxRelaxedToleranceMS = Key[int]{"rotation.max_relaxed_tolerance_ms"}

	FillerFallbackKey             = Key[FillerFallback]{"filler.fallback"}
	FillerReminderDeadlineMinutes = Key[int]{"filler.reminder_deadline_minutes"}

	OutageHeartbeatTimeoutSeconds = Key[int]{"outage.heartbeat_timeout_seconds"}
	OutageIcecastCheckSeconds     = Key[int]{"outage.icecast_check_interval_seconds"}
	OutageFailedChecks            = Key[int]{"outage.failed_checks_to_open"}

	RetentionPlayLogDays = Key[int]{"retention.play_log_days"}
	RetentionLogDays     = Key[int]{"retention.log_days"}

	PlayoutEmergencyPlaylistID = Key[*uint64]{"playout.emergency_playlist_id"}
	PlayoutPrepushMinutes      = Key[int]{"playout.prepush_minutes"}

	ImportInboxDir            = Key[string]{"import.inbox_dir"}
	ImportLibraryDir          = Key[string]{"import.library_dir"}
	ImportMinBitrateKbps      = Key[int]{"import.min_bitrate_kbps"}
	ImportTargetCodec         = Key[string]{"import.target_codec"}
	ImportTargetBitrateKbps   = Key[int]{"import.target_bitrate_kbps"}
	ImportTargetSampleRateHz  = Key[int]{"import.target_sample_rate_hz"}
	ImportTargetChannels      = Key[int]{"import.target_channels"}
	ImportLoudnessToleranceLU = Key[float64]{"import.loudness_tolerance_lu"}
	ImportMaxTruePeakDBTP     = Key[float64]{"import.max_true_peak_dbtp"}
)

// Secret keys. They have no typed Key: their values are never read here.
//
//nolint:gosec // G101: these are setting keys, not credentials.
const (
	SecretIcecastSourcePassword     = "stream.icecast_source_password"
	SecretLiquidsoapControlUser     = "liquidsoap.control_user"
	SecretLiquidsoapControlPassword = "liquidsoap.control_password"
)

// FillerFallback is the global fallback filler mode: used when a clock has
// no default filler mode of its own, for example when its pool is empty or
// a filler reminder expires (§4, §7). ask and pool are not allowed, because
// they would fall back to themselves.
//
// The source is not checked against the library here: a playlist or artist
// deleted later leaves the fallback pointing at nothing, and the filler
// treats it as random.
type FillerFallback struct {
	// Mode is random, playlist, or artist.
	Mode string `json:"mode"`
	// SourceID is the playlist (mode playlist) or artist (mode artist).
	SourceID *uint64 `json:"source_id,omitempty"`
}

// Filler fallback modes.
const (
	FillerRandom   = "random"
	FillerPlaylist = "playlist"
	FillerArtist   = "artist"
)

// Registry holds the definitions, in a fixed order.
type Registry struct {
	defs  []*Definition
	byKey map[string]*Definition
}

// NewRegistry returns every setting of the backend with its code default.
//
// The connection defaults (Icecast host, port, mount and stream format;
// Liquidsoap control host and port, harbor port) equal the services of
// docker-compose.dev.yml, which use the same names as the production stack
// in radio-atlantida-deploy. Keep them in step: TestDefaultsMatchDevStack.
func NewRegistry() *Registry {
	r := &Registry{byKey: map[string]*Definition{}}

	// Station.
	r.add(stringDef(StationTimezone, "Station timezone (IANA name). Grid hours and play rule windows are station local time.",
		"Asia/Yerevan", 1, 64, validTimezone), ApplyImmediate, true)
	r.add(stringDef(StationName, "Station name shown in the panel.", "Radio Atlantida", 1, 100, nil), ApplyImmediate, false)

	// Stream output (Icecast, §8.8).
	r.add(stringDef(StreamIcecastHost, "Icecast server host name or IP address.", "icecast", 1, 253, validHost), ApplyReconnect, true)
	r.add(intDef(StreamIcecastPort, "Icecast server port.", "", 8000, 1, 65535), ApplyReconnect, true)
	r.add(stringDef(StreamMount, "Icecast mount point, starting with /.", "/radio", 2, 100, validMount), ApplyReconnect, true)
	r.add(enumStringDef(StreamCodec, "Stream codec.", "mp3", "mp3", "aac", "vorbis"), ApplyReconnect, true)
	r.add(enumIntDef(StreamBitrateKbps, "Stream bitrate.", "kbps", 128, 64, 96, 128, 160, 192, 256, 320), ApplyReconnect, true)
	r.add(enumIntDef(StreamSampleRateHz, "Stream sample rate.", "Hz", 44100, 22050, 32000, 44100, 48000), ApplyReconnect, true)
	r.add(enumIntDef(StreamChannels, "Stream channels: 1 mono, 2 stereo.", "", 2, 1, 2), ApplyReconnect, true)
	r.add(numberDef(StreamLoudnessLUFS, "Loudness target of the output.", "LUFS", -16, -30, -5), ApplyReconnect, true)
	r.add(stringDef(StreamName, "Stream name sent to Icecast.", "Radio Atlantida", 0, 200, nil), ApplyReconnect, true)
	r.add(stringDef(StreamDescription, "Stream description sent to Icecast.", "", 0, 500, nil), ApplyReconnect, true)
	r.add(stringDef(StreamGenre, "Stream genre sent to Icecast.", "", 0, 100, nil), ApplyReconnect, true)
	r.add(boolDef(StreamHLSEnabled, "HLS output on or off.", false), ApplyReconnect, true)
	r.addSecret(SecretIcecastSourcePassword, GroupStream, "ICECAST_SOURCE_PASSWORD", "Icecast source password.")

	// Liquidsoap (§8).
	r.add(numberDef(LiquidsoapMusicGain, "Default music gain: 1 normal, 0.2 under voice, 0 off.", "", 1, 0, 1), ApplyImmediate, true)
	r.add(numberDef(LiquidsoapDuckGain, "Music level under live voice: while the microphone is live, the music fades to this part of its level (§8.5).",
		"", 0.2, 0, 1), ApplyImmediate, true)
	r.add(intDef(LiquidsoapFadeOutMS, "Fade-out of a truncated track, ending at the segment end.", "ms", 2500, 2000, 3000), ApplyImmediate, true)
	r.add(intDef(LiquidsoapHarborPort, "Harbor input port for live voice.", "", 8005, 1, 65535), ApplyReconnect, true)
	r.add(stringDef(LiquidsoapControlHost, "Liquidsoap control host name or IP address.", "liquidsoap", 1, 253, validHost), ApplyReconnect, true)
	r.add(intDef(LiquidsoapControlPort, "Liquidsoap control (telnet) port.", "", 1234, 1, 65535), ApplyReconnect, true)
	r.add(stringDef(LiquidsoapMediaRoot, "The media directory as Liquidsoap sees it (its mount of MEDIA_ROOT); track paths are pushed under it.",
		"/media", 1, 255, validMediaRoot), ApplyImmediate, true)
	r.addSecret(SecretLiquidsoapControlUser, GroupLiquidsoap, "LIQUIDSOAP_CONTROL_USER", "Liquidsoap control user.")
	r.addSecret(SecretLiquidsoapControlPassword, GroupLiquidsoap, "LIQUIDSOAP_CONTROL_PASSWORD", "Liquidsoap control password.")

	// Rotation (§7).
	r.add(intDef(RotationGridHorizonDays, "How far ahead the grid can be planned.", "days", 30, 1, 30), ApplyImmediate, true)
	r.add(intDef(RotationPlanHorizonHours, "How far ahead the track plan is kept.", "hours", 48, 24, 48), ApplyImmediate, true)
	r.add(intDef(RotationLookaheadItems, "Items pushed to Liquidsoap ahead of the current one.", "", 3, 2, 3), ApplyImmediate, true)
	r.add(intDef(RotationDurationToleranceMS, "Default duration tolerance of a new clock element.", "ms", 30000, 0, 600000), ApplyImmediate, false)
	r.add(intDef(RotationFixedToleranceMS, "Default start tolerance of a new fixed clock element.", "ms", 60000, 0, 600000), ApplyImmediate, false)
	r.add(intDef(RotationMinFillerGapMS, "Shortest gap in a segment that gets filler; a smaller gap is left.", "ms", 10000, 0, 120000), ApplyImmediate, true)
	r.add(intDef(RotationMaxRelaxedToleranceMS, "Emergency: how far the duration tolerance of an element may be relaxed when no track fits.", "ms", 120000, 0, 600000), ApplyImmediate, true)

	// Filler (§7).
	r.add(fillerFallbackDef(), ApplyImmediate, true)
	r.add(intDef(FillerReminderDeadlineMinutes, "Deadline of a filler reminder, before air time.", "minutes", 120, 5, 1440), ApplyImmediate, true)

	// Outage detection (§7.1).
	r.add(intDef(OutageHeartbeatTimeoutSeconds, "Liquidsoap callback and control link heartbeat timeout.", "seconds", 10, 2, 300), ApplyImmediate, false)
	r.add(intDef(OutageIcecastCheckSeconds, "Icecast mount check interval.", "seconds", 5, 1, 300), ApplyImmediate, false)
	r.add(intDef(OutageFailedChecks, "Consecutive failed checks before an outage is opened.", "", 2, 1, 10), ApplyImmediate, false)

	// Retention (§4.1). The play log must cover the separation window
	// (72 hours by default).
	r.add(intDef(RetentionPlayLogDays, "Days aired history is kept. Open missed items are kept until resolved.", "days", 10, 3, 365), ApplyImmediate, false)
	r.add(intDef(RetentionLogDays, "Days application log files and finished operations are kept.", "days", 10, 1, 365), ApplyImmediate, false)

	// Playout (§8.6).
	r.add(optionalIDDef(PlayoutEmergencyPlaylistID, "Playlist played when the pool and the plan are both empty. Null: none."), ApplyImmediate, true)
	r.add(intDef(PlayoutPrepushMinutes, "How long before its start an hour's first items are pushed to Liquidsoap; from then on the hour is no longer rebuilt.",
		"minutes", 5, 1, 15), ApplyImmediate, true)

	// Import and the broadcast standard of track files (docs/phase-11-plan.md
	// §2). The loudness target is stream.loudness_target_lufs.
	r.add(stringDef(ImportInboxDir, "Folder under the media root that an import scans for new files.", "incoming", 1, 255, validFolder), ApplyImmediate, false)
	r.add(stringDef(ImportLibraryDir, "Folder under the media root that imported files are moved to.", "library", 1, 255, validFolder), ApplyImmediate, false)
	r.add(intDef(ImportMinBitrateKbps, "Lossy files below this bitrate get the low_bitrate issue.", "kbps", 128, 32, 320), ApplyImmediate, false)
	r.add(enumStringDef(ImportTargetCodec, "Codec of the broadcast standard (Optimize track).", "mp3", "mp3", "aac", "vorbis"), ApplyImmediate, false)
	r.add(enumIntDef(ImportTargetBitrateKbps, "Bitrate of the broadcast standard.", "kbps", 320, 128, 160, 192, 256, 320), ApplyImmediate, false)
	r.add(enumIntDef(ImportTargetSampleRateHz, "Sample rate of the broadcast standard.", "Hz", 44100, 44100, 48000), ApplyImmediate, false)
	r.add(enumIntDef(ImportTargetChannels, "Channels of the broadcast standard: 1 mono, 2 stereo.", "", 2, 1, 2), ApplyImmediate, false)
	r.add(numberDef(ImportLoudnessToleranceLU, "How far a track's loudness may be from stream.loudness_target_lufs before it gets the loudness issue.",
		"LU", 2, 0.5, 6), ApplyImmediate, false)
	r.add(numberDef(ImportMaxTruePeakDBTP, "Highest true peak of a track; above it, the true_peak issue. Also the peak limit of Fix loudness.",
		"dBTP", -1, -6, 0), ApplyImmediate, false)

	return r
}

// Lookup returns the definition of key.
func (r *Registry) Lookup(key string) (*Definition, bool) {
	d, ok := r.byKey[key]
	return d, ok
}

// Settings returns the definitions that are not secrets.
func (r *Registry) Settings() []*Definition {
	return slices.DeleteFunc(slices.Clone(r.defs), func(d *Definition) bool { return d.Secret })
}

// Secrets returns the secret definitions.
func (r *Registry) Secrets() []*Definition {
	return slices.DeleteFunc(slices.Clone(r.defs), func(d *Definition) bool { return !d.Secret })
}

func (r *Registry) add(d *Definition, apply Apply, affectsAir bool) {
	d.Apply = apply
	d.AffectsAir = affectsAir
	r.insert(d)
}

func (r *Registry) addSecret(key string, g Group, envName, desc string) {
	r.insert(&Definition{Key: key, Group: g, Description: desc, Type: TypeString, Secret: true, EnvName: envName})
}

// insert panics on a programmer error: a duplicate key, or a key outside
// its group.
func (r *Registry) insert(d *Definition) {
	if _, dup := r.byKey[d.Key]; dup {
		panic("settings: duplicate key " + d.Key)
	}
	group, _, _ := strings.Cut(d.Key, ".")
	d.Group = Group(group)
	r.defs = append(r.defs, d)
	r.byKey[d.Key] = d
}

// Definition builders. Each sets parse to decode the JSON value into the
// Go type of its Key and to check it.

func intDef(k Key[int], desc, unit string, def, minimum, maximum int) *Definition {
	return &Definition{
		Key: k.name, Description: desc, Type: TypeInteger, Unit: unit, Default: def,
		Minimum: new(float64(minimum)), Maximum: new(float64(maximum)),
		parse: func(raw json.RawMessage) (any, error) {
			n, err := decodeInt(k.name, raw)
			if err != nil {
				return nil, err
			}
			if n < int64(minimum) || n > int64(maximum) {
				return nil, invalid(k.name, RuleRange, fmt.Sprintf("must be between %d and %d", minimum, maximum))
			}
			return int(n), nil
		},
	}
}

func enumIntDef(k Key[int], desc, unit string, def int, values ...int) *Definition {
	enum := make([]any, len(values))
	for i, v := range values {
		enum[i] = v
	}
	return &Definition{
		Key: k.name, Description: desc, Type: TypeInteger, Unit: unit, Default: def, Enum: enum,
		parse: func(raw json.RawMessage) (any, error) {
			n, err := decodeInt(k.name, raw)
			if err != nil {
				return nil, err
			}
			if !slices.Contains(values, int(n)) {
				return nil, invalid(k.name, RuleEnum, fmt.Sprintf("must be one of %v", values))
			}
			return int(n), nil
		},
	}
}

func numberDef(k Key[float64], desc, unit string, def, minimum, maximum float64) *Definition {
	return &Definition{
		Key: k.name, Description: desc, Type: TypeNumber, Unit: unit, Default: def,
		Minimum: new(minimum), Maximum: new(maximum),
		parse: func(raw json.RawMessage) (any, error) {
			num, err := decodeNumber(k.name, raw)
			if err != nil {
				return nil, err
			}
			f, err := num.Float64()
			if err != nil || math.IsInf(f, 0) {
				return nil, invalid(k.name, RuleRange, fmt.Sprintf("must be between %g and %g", minimum, maximum))
			}
			if f < minimum || f > maximum {
				return nil, invalid(k.name, RuleRange, fmt.Sprintf("must be between %g and %g", minimum, maximum))
			}
			return f, nil
		},
	}
}

// stringDef limits the length in characters to minLen..maxLen; check, if
// set, validates the format.
func stringDef(k Key[string], desc, def string, minLen, maxLen int, check func(string) error) *Definition {
	return &Definition{
		Key: k.name, Description: desc, Type: TypeString, Default: def,
		Minimum: new(float64(minLen)), Maximum: new(float64(maxLen)),
		parse: func(raw json.RawMessage) (any, error) {
			var s string
			if err := decodeStrict(raw, &s); err != nil || isNull(raw) {
				return nil, invalid(k.name, RuleType, "must be a string")
			}
			if s != strings.TrimSpace(s) {
				return nil, invalid(k.name, RuleFormat, "must not start or end with spaces")
			}
			if n := utf8.RuneCountInString(s); n < minLen || n > maxLen {
				return nil, invalid(k.name, RuleLength, fmt.Sprintf("must be %d to %d characters long", minLen, maxLen))
			}
			if check != nil {
				if err := check(s); err != nil {
					return nil, invalid(k.name, RuleFormat, err.Error())
				}
			}
			return s, nil
		},
	}
}

func enumStringDef(k Key[string], desc, def string, values ...string) *Definition {
	enum := make([]any, len(values))
	for i, v := range values {
		enum[i] = v
	}
	return &Definition{
		Key: k.name, Description: desc, Type: TypeString, Default: def, Enum: enum,
		parse: func(raw json.RawMessage) (any, error) {
			var s string
			if err := decodeStrict(raw, &s); err != nil || isNull(raw) {
				return nil, invalid(k.name, RuleType, "must be a string")
			}
			if !slices.Contains(values, s) {
				return nil, invalid(k.name, RuleEnum, "must be one of "+strings.Join(values, ", "))
			}
			return s, nil
		},
	}
}

func boolDef(k Key[bool], desc string, def bool) *Definition {
	return &Definition{
		Key: k.name, Description: desc, Type: TypeBoolean, Default: def,
		parse: func(raw json.RawMessage) (any, error) {
			var b bool
			if err := decodeStrict(raw, &b); err != nil || isNull(raw) {
				return nil, invalid(k.name, RuleType, "must be true or false")
			}
			return b, nil
		},
	}
}

// optionalIDDef is a nullable reference to a row (an ID of 1 or more).
func optionalIDDef(k Key[*uint64], desc string) *Definition {
	return &Definition{
		Key: k.name, Description: desc, Type: TypeInteger, Default: (*uint64)(nil), Nullable: true,
		Minimum: new(1.0),
		parse: func(raw json.RawMessage) (any, error) {
			if isNull(raw) {
				return (*uint64)(nil), nil
			}
			n, err := decodeInt(k.name, raw)
			if err != nil {
				return nil, err
			}
			if n < 1 {
				return nil, invalid(k.name, RuleRange, "must be 1 or more, or null")
			}
			return new(uint64(n)), nil
		},
	}
}

func fillerFallbackDef() *Definition {
	k := FillerFallbackKey
	return &Definition{
		Key: k.name, Type: TypeObject,
		Description: `Global fallback filler mode, used when a clock has no default filler mode: ` +
			`{"mode": "random"}, or {"mode": "playlist" or "artist", "source_id": <id>}.`,
		Default: FillerFallback{Mode: FillerRandom},
		parse: func(raw json.RawMessage) (any, error) {
			var v struct {
				Mode     *string `json:"mode"`
				SourceID *uint64 `json:"source_id"`
			}
			if isNull(raw) || bytes.TrimSpace(raw)[0] != '{' {
				return nil, invalid(k.name, RuleType, "must be an object")
			}
			if err := decodeStrict(raw, &v); err != nil {
				if name, ok := unknownField(err); ok {
					return nil, invalid(k.name, RuleUnknownField, "unknown property "+name)
				}
				return nil, invalid(k.name, RuleType, "mode must be a string and source_id an integer")
			}
			switch {
			case v.Mode == nil:
				return nil, invalid(k.name, RuleRequired, "mode is required")
			case !slices.Contains([]string{FillerRandom, FillerPlaylist, FillerArtist}, *v.Mode):
				return nil, invalid(k.name, RuleEnum, "mode must be one of random, playlist, artist")
			case *v.Mode == FillerRandom && v.SourceID != nil:
				return nil, invalid(k.name, RuleInvalid, "source_id is not allowed for mode random")
			case *v.Mode != FillerRandom && v.SourceID == nil:
				return nil, invalid(k.name, RuleRequired, "source_id is required for mode "+*v.Mode)
			case v.SourceID != nil && *v.SourceID < 1:
				return nil, invalid(k.name, RuleRange, "source_id must be 1 or more")
			}
			return FillerFallback{Mode: *v.Mode, SourceID: v.SourceID}, nil
		},
	}
}

// decodeStrict decodes exactly one JSON value without unknown properties.
func decodeStrict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("more than one JSON value")
	}
	return nil
}

func decodeNumber(key string, raw json.RawMessage) (json.Number, error) {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil || dec.More() {
		return "", invalid(key, RuleType, "must be a number")
	}
	n, ok := v.(json.Number)
	if !ok {
		return "", invalid(key, RuleType, "must be a number")
	}
	return n, nil
}

// decodeInt accepts integral numbers only; 3.0 is accepted as 3, 3.5 is not.
func decodeInt(key string, raw json.RawMessage) (int64, error) {
	num, err := decodeNumber(key, raw)
	if err != nil {
		return 0, invalid(key, RuleType, "must be an integer")
	}
	if n, perr := strconv.ParseInt(string(num), 10, 64); perr == nil {
		return n, nil
	}
	f, err := num.Float64()
	switch {
	case err == nil && f != math.Trunc(f):
		return 0, invalid(key, RuleType, "must be an integer")
	case err != nil || math.Abs(f) > math.MaxInt32:
		// Integral but huge: every integer setting is far below this.
		return 0, invalid(key, RuleRange, "is out of range")
	}
	return int64(f), nil
}

func isNull(raw json.RawMessage) bool {
	return len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null"
}

// unknownField extracts the property name from encoding/json's error for
// DisallowUnknownFields.
func unknownField(err error) (string, bool) {
	const prefix = "json: unknown field "
	msg := err.Error()
	if !strings.HasPrefix(msg, prefix) {
		return "", false
	}
	return strings.Trim(strings.TrimPrefix(msg, prefix), `"`), true
}

func invalid(key string, rule Rule, msg string) *InvalidValueError {
	return &InvalidValueError{Key: key, Rule: rule, Message: msg}
}

// Format checks.

func validTimezone(s string) error {
	// LoadLocation accepts "" and "Local" too, which are not IANA names
	// and would make the meaning of the grid depend on the host.
	if s == "Local" {
		return errors.New("must be an IANA timezone name, for example Asia/Yerevan")
	}
	if _, err := time.LoadLocation(s); err != nil {
		return errors.New("must be an IANA timezone name, for example Asia/Yerevan")
	}
	return nil
}

var hostLabel = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

func validHost(s string) error {
	if net.ParseIP(s) != nil {
		return nil
	}
	for label := range strings.SplitSeq(s, ".") {
		if !hostLabel.MatchString(label) {
			return errors.New("must be a host name or an IP address")
		}
	}
	return nil
}

var mediaRootPattern = regexp.MustCompile(`^/[^\x00-\x1f"]*$`)

// validMediaRoot accepts an absolute path without control characters or
// quotes: it becomes part of a Liquidsoap request URI.
func validMediaRoot(s string) error {
	if !mediaRootPattern.MatchString(s) {
		return errors.New("must be an absolute path starting with /, without quotes or control characters")
	}
	return nil
}

// validFolder accepts a clean relative folder path with forward slashes,
// like the track paths it becomes part of (media.ValidatePath).
func validFolder(s string) error {
	if strings.Contains(s, `\`) || strings.HasPrefix(s, "/") || drivePrefix.MatchString(s) ||
		!fs.ValidPath(s) || s == "." || path.Clean(s) != s {
		return errors.New(`must be a folder relative to the media root, with forward slashes and no "." or ".." parts`)
	}
	return nil
}

var drivePrefix = regexp.MustCompile(`^[A-Za-z]:`)

var mountPattern = regexp.MustCompile(`^/[A-Za-z0-9._~-]+$`)

func validMount(s string) error {
	if !mountPattern.MatchString(s) {
		return errors.New("must start with / followed by letters, digits, or . _ ~ -")
	}
	return nil
}
