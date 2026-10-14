// Package errcodes holds the stable machine-readable error codes of the API
// (init plan §9.6).
//
// The panel translates messages by code, so a code is never renamed or
// removed once released; new codes are added. A new refusal rule adds its
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
	// LogFileNotFound: no log file has the requested name.
	LogFileNotFound Problem = "log_file_not_found"
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
	// PlaylistInUse: clocks use the playlist in an element or as filler;
	// see used_by.
	PlaylistInUse Problem = "playlist_in_use"
	// ArtistNotFound: no artist has the requested ID.
	ArtistNotFound Problem = "artist_not_found"
	// ArtistInUse: tracks or clocks (as filler) refer to the artist; see
	// used_by.
	ArtistInUse Problem = "artist_in_use"
	// ArtistHasHistory: the play log refers to the artist, so it is kept.
	ArtistHasHistory Problem = "artist_has_history"
	// TrackNotFound: no track has the requested ID.
	TrackNotFound Problem = "track_not_found"
	// TrackInUse: clock filler pools hold the track; see used_by.
	TrackInUse Problem = "track_in_use"
	// TrackHasHistory: the play log or the track plan refers to the track,
	// so it is kept.
	TrackHasHistory Problem = "track_has_history"
	// TrackFileNotFound: the track exists, but its file is missing under
	// the media root.
	TrackFileNotFound Problem = "track_file_not_found"
	// PlayRuleNotFound: no play rule has the requested ID.
	PlayRuleNotFound Problem = "play_rule_not_found"
	// PlayRuleInUse: the rule is applied to tracks or playlists; see
	// used_by.
	PlayRuleInUse Problem = "play_rule_in_use"
	// SeparationRuleNotFound: no separation rule has the requested ID.
	SeparationRuleNotFound Problem = "separation_rule_not_found"
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
	// FillerRequestNotFound: no filler reminder has the requested ID.
	FillerRequestNotFound Problem = "filler_request_not_found"
	// FillerRequestClosed: the reminder was already answered, defaulted, or
	// expired.
	FillerRequestClosed Problem = "filler_request_closed"
	// FillerRequestNotAnswerable: a pool_empty reminder is closed by
	// refilling the clock's filler pool, not by a track.
	FillerRequestNotAnswerable Problem = "filler_request_not_answerable"
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
)

// Problems lists every problem code, in declaration order.
func Problems() []Problem {
	return []Problem{
		BadRequest, ValidationFailed, NotFound, MethodNotAllowed, BodyTooLarge,
		UnsupportedMediaType, NotAcceptable, InternalError,
		OperationNotFound, LogFileNotFound,
		SettingNotFound, SettingNotEditable,
		PlaylistTypeNotFound, SystemTypeNotDeletable, PlaylistTypeInUse,
		PlaylistNotFound, PlaylistInUse,
		ArtistNotFound, ArtistInUse, ArtistHasHistory,
		TrackNotFound, TrackInUse, TrackHasHistory, TrackFileNotFound,
		PlayRuleNotFound, PlayRuleInUse,
		SeparationRuleNotFound,
		ClockNotFound, ClockInUse, ClockHasHistory,
		GridSlotNotFound, GridConflict,
		FillerRequestNotFound, FillerRequestClosed, FillerRequestNotAnswerable, HourStarted,
		LiquidsoapUnavailable,
		MissedItemNotFound, MissedItemResolved,
		MediaOperationRunning, MediaToolsUnavailable,
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
	// Overlap: the value collides with another one (two fixed clock
	// elements whose start windows overlap).
	Overlap Field = "overlap"
	// DoesNotFit: the content does not fit into its time (material before
	// a fixed clock element, a fixed element past the end of the hour).
	DoesNotFit Field = "does_not_fit"
	// Order: the items are out of order (fixed clock elements not in
	// minute order).
	Order Field = "order"
	// Blocked: the track breaks separation or a play rule at the time it
	// would play.
	Blocked Field = "blocked"
)

// Fields lists every field error code, in declaration order.
func Fields() []Field {
	return []Field{Required, UnknownField, Type, Range, Length, Pattern, Format, Enum, Unique, Invalid, Taken, UnknownID, Overlap, DoesNotFit, Order, Blocked}
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
