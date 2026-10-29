// Package errcodes holds the stable machine-readable error codes of the API
// (init plan §9.6).
//
// The panel translates messages by code, so a code is never renamed; new
// codes are added. A code is removed only with the feature it belongs to,
// and the release notes list it (phase 23 removed the separation and filler
// codes). A new refusal rule adds its
// code here in the same change. The English title and detail of a problem are
// for logs and developers only.
//
// Both code lists are published in the OpenAPI document as enums
// (ProblemCode, FieldErrorCode).
package errcodes

import "github.com/danielgtaylor/huma/v2"

// Problem is the code of a whole problem response.
type Problem string

// Problem codes.
const (
	// BadRequest: the request could not be read (malformed JSON, bad
	// parameter syntax).
	BadRequest Problem = "bad_request"
	// ValidationFailed: one or more fields are invalid; see errors[].
	ValidationFailed Problem = "validation_failed"
	// NotFound: no route, or the addressed resource does not exist.
	NotFound Problem = "not_found"
	// MethodNotAllowed: the route exists but not for this method.
	MethodNotAllowed Problem = "method_not_allowed"
	// BodyTooLarge: the request body exceeds the limit.
	BodyTooLarge Problem = "body_too_large"
	// UnsupportedMediaType: the request body is not JSON.
	UnsupportedMediaType Problem = "unsupported_media_type"
	// NotAcceptable: the response cannot be produced in the accepted format.
	NotAcceptable Problem = "not_acceptable"
	// InternalError: an unexpected server error; it is logged with the
	// request ID.
	InternalError Problem = "internal_error"
	// OperationNotFound: no operation has the requested ID.
	OperationNotFound Problem = "operation_not_found"
	// SettingNotFound: no setting has the requested key.
	SettingNotFound Problem = "setting_not_found"
	// SettingNotEditable: the key is a secret; it is set in the environment
	// and changed by restarting the service, never through the API.
	SettingNotEditable Problem = "setting_not_editable"

	// PlaylistTypeNotFound: no playlist type has the requested ID.
	PlaylistTypeNotFound Problem = "playlist_type_not_found"
	// SystemTypeNotDeletable: music, jingle, news, and segment can be
	// renamed but never deleted.
	SystemTypeNotDeletable Problem = "system_type_not_deletable"
	// PlaylistTypeInUse: playlists or tracks have the type; see used_by.
	PlaylistTypeInUse Problem = "playlist_type_in_use"
	// PlaylistNotFound: no playlist has the requested ID.
	PlaylistNotFound Problem = "playlist_not_found"
	// PlaylistInUse: clocks use the playlist as a clock playlist or as an
	// insert source; see used_by.
	PlaylistInUse Problem = "playlist_in_use"
	// ArtistNotFound: no artist has the requested ID.
	ArtistNotFound Problem = "artist_not_found"
	// ArtistInUse: tracks refer to the artist; see used_by.
	ArtistInUse Problem = "artist_in_use"
	// ArtistHasHistory: the play log refers to the artist, so it is kept.
	ArtistHasHistory Problem = "artist_has_history"
	// TrackNotFound: no track has the requested ID.
	TrackNotFound Problem = "track_not_found"
	// TrackInUse: clock inserts or one-time inserts of the plan use the
	// track; see used_by.
	TrackInUse Problem = "track_in_use"
	// TrackHasHistory: the play log or the track plan refers to the track,
	// so it is kept.
	TrackHasHistory Problem = "track_has_history"
	// TrackFileNotFound: the track exists, but its file is missing under
	// the media root.
	TrackFileNotFound Problem = "track_file_not_found"
	// TitleEmpty: a bulk rename would leave the track without a title, so
	// the track keeps its title (refused[] of POST /api/tracks/rename).
	TitleEmpty Problem = "title_empty"
	// TitleTooLong: a bulk rename would make the title longer than 500
	// characters; it is refused, not cut.
	TitleTooLong Problem = "title_too_long"
	// PlayRuleNotFound: no play rule has the requested ID.
	PlayRuleNotFound Problem = "play_rule_not_found"
	// PlayRuleInUse: the rule is applied to tracks or playlists; see
	// used_by.
	PlayRuleInUse Problem = "play_rule_in_use"
	// ClockNotFound: no clock has the requested ID.
	ClockNotFound Problem = "clock_not_found"
	// ClockInUse: grid slots assign the clock; see used_by.
	ClockInUse Problem = "clock_in_use"
	// ClockHasHistory: the play log or the track plan refers to the clock,
	// so it is kept.
	ClockHasHistory Problem = "clock_has_history"
	// GridSlotNotFound: no grid slot has the requested ID.
	GridSlotNotFound Problem = "grid_slot_not_found"
	// GridConflict: another change of the same grid hours was saved at the
	// same time; reload the grid and try again.
	GridConflict Problem = "grid_conflict"
	// HourInsertNotFound: no one-time insert has the requested ID.
	HourInsertNotFound Problem = "hour_insert_not_found"
	// HourStarted: the hour is on air, over, or about to start (its items
	// may be in Liquidsoap's queue); its plan is not changed.
	HourStarted Problem = "hour_started"
	// LiquidsoapUnavailable: the backend cannot reach Liquidsoap's control
	// interface; the command was not applied.
	LiquidsoapUnavailable Problem = "liquidsoap_unavailable"
	// MissedItemNotFound: no missed item with that ID.
	MissedItemNotFound Problem = "missed_item_not_found"
	// MissedItemResolved: the missed item was already rescheduled or
	// dismissed.
	MissedItemResolved Problem = "missed_item_resolved"
	// MediaOperationRunning: an import, analysis, or transcode is running;
	// they run one at a time. Wait for its terminal event.
	MediaOperationRunning Problem = "media_operation_running"
	// MediaToolsUnavailable: ffmpeg or ffprobe is not installed where the
	// backend runs.
	MediaToolsUnavailable Problem = "media_tools_unavailable"
	// LiveSessionActive: a live session is running; send takeover to end
	// it and start a new one.
	LiveSessionActive Problem = "live_session_active"
	// LiveOfferInvalid: the WebRTC offer is not an SDP or has no Opus audio.
	LiveOfferInvalid Problem = "live_offer_invalid"
	// LiveUnavailable: the live voice bridge could not open its UDP port.
	LiveUnavailable Problem = "live_unavailable"
	// TrackOnAir: the track is queued in Liquidsoap or on air, so its cue
	// points are not changed now; try again when it has played.
	TrackOnAir Problem = "track_on_air"
	// TrackNotAnalysed: the track has no duration yet (analyse it first),
	// so its cue points cannot be checked.
	TrackNotAnalysed Problem = "track_not_analysed"
	// WaveformUnavailable: the track file is missing or cannot be decoded,
	// so it has no waveform.
	WaveformUnavailable Problem = "waveform_unavailable"
	// RangeTooLong: the log search's time range is longer than Loki accepts
	// (its query length limit); search a shorter range.
	RangeTooLong Problem = "range_too_long"
	// LogsUnavailable: Loki did not answer in time, so the log lines cannot
	// be read now; the logs are still written to stdout.
	LogsUnavailable Problem = "logs_unavailable"
)

// Problems lists every problem code, in declaration order.
func Problems() []Problem {
	return []Problem{
		BadRequest, ValidationFailed, NotFound, MethodNotAllowed, BodyTooLarge,
		UnsupportedMediaType, NotAcceptable, InternalError,
		OperationNotFound,
		SettingNotFound, SettingNotEditable,
		PlaylistTypeNotFound, SystemTypeNotDeletable, PlaylistTypeInUse,
		PlaylistNotFound, PlaylistInUse,
		ArtistNotFound, ArtistInUse, ArtistHasHistory,
		TrackNotFound, TrackInUse, TrackHasHistory, TrackFileNotFound, TitleEmpty, TitleTooLong,
		PlayRuleNotFound, PlayRuleInUse,
		ClockNotFound, ClockInUse, ClockHasHistory,
		GridSlotNotFound, GridConflict,
		HourInsertNotFound, HourStarted,
		LiquidsoapUnavailable,
		MissedItemNotFound, MissedItemResolved,
		MediaOperationRunning, MediaToolsUnavailable,
		LiveSessionActive, LiveOfferInvalid, LiveUnavailable,
		TrackOnAir, TrackNotAnalysed, WaveformUnavailable,
		RangeTooLong, LogsUnavailable,
	}
}

// Field is the code of one field error.
type Field string

// Field error codes.
const (
	// Required: the field is missing.
	Required Field = "required"
	// UnknownField: the field is not part of the request.
	UnknownField Field = "unknown_field"
	// Type: the value has the wrong JSON type.
	Type Field = "type"
	// Range: a number is outside its allowed range.
	Range Field = "range"
	// Length: a string or a list is too short or too long.
	Length Field = "length"
	// Pattern: a string does not match the required pattern.
	Pattern Field = "pattern"
	// Format: a string is not in the required format (date-time, URI, ...).
	Format Field = "format"
	// Enum: the value is not one of the allowed values.
	Enum Field = "enum"
	// Unique: a list contains duplicates.
	Unique Field = "unique"
	// Invalid: any other problem with the value.
	Invalid Field = "invalid"
	// Taken: another entity already uses the value (a name, a file path).
	Taken Field = "taken"
	// UnknownID: the field refers to an entity that does not exist.
	UnknownID Field = "unknown_id"
	// Overlap: the value collides with another one.
	Overlap Field = "overlap"
	// DoesNotFit: the content does not fit into its time (a one-time
	// insert that runs past the end of the hour).
	DoesNotFit Field = "does_not_fit"
	// Order: the items are out of order.
	Order Field = "order"
	// Blocked: the track breaks a play rule at the time it would play.
	Blocked Field = "blocked"
	// InvalidCharacters: the text contains characters it must not have
	// (control characters such as line breaks and tabs in a title).
	InvalidCharacters Field = "invalid_characters"
	// CueOrder: the cue out point is not after the cue in point.
	CueOrder Field = "cue_order"
	// CueBeyondEnd: the cue point is at or past the end of the file.
	CueBeyondEnd Field = "cue_beyond_end"
	// AirTooShort: the part between the cue points is shorter than 5 s.
	AirTooShort Field = "air_too_short"
	// FadesTooLong: the fade in and the fade out together are longer than
	// the part between the cue points.
	FadesTooLong Field = "fades_too_long"
	// IntervalBeyondRetention: a play rule interval is longer than the play
	// log keeps aired history (retention.play_log_days), or the retention
	// would become shorter than an existing interval.
	IntervalBeyondRetention Field = "interval_beyond_retention"
	// FillPlaylistRequired: a clock needs at least one playlist whose
	// placement rule is fill.
	FillPlaylistRequired Field = "fill_playlist_required"
	// FillWeightsPartial: either every fill playlist of a clock has a
	// weight or none has.
	FillWeightsPartial Field = "fill_weights_partial"
	// ClockCountRequired: the spread placement rule needs clock_count.
	ClockCountRequired Field = "clock_count_required"
	// ClockCountNotAllowed: clock_count is set only for the spread rule.
	ClockCountNotAllowed Field = "clock_count_not_allowed"
	// FillWeightNotAllowed: fill_weight is set only on fill playlists.
	FillWeightNotAllowed Field = "fill_weight_not_allowed"
	// InsertOffsetTaken: another insert of the clock starts at this time.
	InsertOffsetTaken Field = "insert_offset_taken"
	// InsertSourceRequired: an insert names exactly one of track_id and
	// playlist_id.
	InsertSourceRequired Field = "insert_source_required"
)

// Fields lists every field error code, in declaration order.
func Fields() []Field {
	return []Field{
		Required, UnknownField, Type, Range, Length, Pattern, Format, Enum, Unique, Invalid, Taken, UnknownID, Overlap, DoesNotFit,
		Order, Blocked, InvalidCharacters, CueOrder, CueBeyondEnd, AirTooShort, FadesTooLong,
		IntervalBeyondRetention, FillPlaylistRequired, FillWeightsPartial, ClockCountRequired, ClockCountNotAllowed,
		FillWeightNotAllowed, InsertOffsetTaken, InsertSourceRequired,
	}
}

const schemaPrefix = "#/components/schemas/"

// Schema publishes the problem codes as the named enum ProblemCode.
func (Problem) Schema(r huma.Registry) *huma.Schema {
	return namedEnum(r, "ProblemCode", "Stable code of a problem response. Never renamed once released.", Problems())
}

// Schema publishes the field error codes as the named enum FieldErrorCode.
func (Field) Schema(r huma.Registry) *huma.Schema {
	return namedEnum(r, "FieldErrorCode", "Stable code of a field error. Never renamed once released.", Fields())
}

func namedEnum[T ~string](r huma.Registry, name, doc string, values []T) *huma.Schema {
	if _, ok := r.Map()[name]; !ok {
		enum := make([]any, len(values))
		for i, v := range values {
			enum[i] = string(v)
		}
		r.Map()[name] = &huma.Schema{Type: huma.TypeString, Description: doc, Enum: enum}
	}
	return &huma.Schema{Ref: schemaPrefix + name}
}
