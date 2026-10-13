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
)

// Problems lists every problem code, in declaration order.
func Problems() []Problem {
	return []Problem{
		BadRequest, ValidationFailed, NotFound, MethodNotAllowed, BodyTooLarge,
		UnsupportedMediaType, NotAcceptable, InternalError,
		OperationNotFound, LogFileNotFound,
		SettingNotFound, SettingNotEditable,
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
)

// Fields lists every field error code, in declaration order.
func Fields() []Field {
	return []Field{Required, UnknownField, Type, Range, Length, Pattern, Format, Enum, Unique, Invalid}
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
