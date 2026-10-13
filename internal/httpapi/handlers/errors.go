package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/validation"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/errcodes"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
)

// problemFor maps an error returned by a domain service to the problem
// response. It is the one place where domain errors become HTTP statuses.
// Unexpected errors are logged here, once, at ERROR, and their text is not
// sent to the client.
func problemFor(ctx context.Context, log *slog.Logger, err error) *dto.Problem {
	var p *dto.Problem
	switch {
	case errors.As(err, &p):
		// Already a problem (built by a handler).
	case errors.Is(err, operations.ErrNotFound):
		p = dto.NewProblem(http.StatusNotFound, errcodes.OperationNotFound, "operation not found")
	case errors.Is(err, logging.ErrFileNotFound):
		p = dto.NewProblem(http.StatusNotFound, errcodes.LogFileNotFound, "log file not found")
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		// The client went away or the server is shutting down; nobody reads
		// the response, and it is not an error of the service.
		p = dto.NewProblem(http.StatusServiceUnavailable, errcodes.InternalError, "request cancelled")
	default:
		log.ErrorContext(ctx, "request failed",
			logging.KeyRequestID, middleware.GetReqID(ctx),
			logging.KeyError, err,
		)
		p = dto.NewProblem(http.StatusInternalServerError, errcodes.InternalError, "")
	}
	if p.RequestID == "" {
		p.RequestID = middleware.GetReqID(ctx)
	}
	return p
}

// NewHumaError builds the problem for errors raised by huma itself: request
// decoding, parameter parsing, and schema validation. Install it as
// huma.NewErrorWithContext.
//
// Validation errors (422) become per-field errors with stable codes. Other
// client errors carry their messages in detail. Server errors carry no
// internal text.
func NewHumaError(hctx huma.Context, status int, msg string, errs ...error) huma.StatusError {
	p := dto.NewProblem(status, codeForStatus(status), "")
	if hctx != nil {
		p.RequestID = middleware.GetReqID(hctx.Context())
	}
	if status >= http.StatusInternalServerError {
		return p
	}

	var details []string
	for _, err := range errs {
		if err == nil {
			continue
		}
		var d huma.ErrorDetailer
		switch {
		case !errors.As(err, &d):
			details = append(details, err.Error())
		case status == http.StatusUnprocessableEntity:
			p.Errors = append(p.Errors, fieldError(d.ErrorDetail()))
		default:
			// Only the message: ErrorDetail.Error() would echo the raw value.
			details = append(details, d.ErrorDetail().Message)
		}
	}
	p.Detail = msg
	if len(details) > 0 {
		p.Detail = msg + ": " + strings.Join(details, "; ")
	}
	return p
}

func codeForStatus(status int) errcodes.Problem {
	switch status {
	case http.StatusUnprocessableEntity:
		return errcodes.ValidationFailed
	case http.StatusNotFound:
		return errcodes.NotFound
	case http.StatusMethodNotAllowed:
		return errcodes.MethodNotAllowed
	case http.StatusRequestEntityTooLarge:
		return errcodes.BodyTooLarge
	case http.StatusUnsupportedMediaType:
		return errcodes.UnsupportedMediaType
	case http.StatusNotAcceptable:
		return errcodes.NotAcceptable
	}
	if status >= http.StatusInternalServerError {
		return errcodes.InternalError
	}
	return errcodes.BadRequest
}

// bodyPrefix is stripped from field locations: the panel addresses body
// fields by their own path (elements[2].fixed_minute). Query and path
// parameters keep their prefix (query.limit).
const bodyPrefix = "body."

func fieldError(d *huma.ErrorDetail) dto.FieldError {
	field := d.Location
	code := fieldCode(d.Message)
	if code == errcodes.Required {
		// huma reports a missing property at its parent object.
		if name, ok := requiredProperty(d.Message); ok {
			field += "." + name
		}
	}
	field = strings.TrimPrefix(field, bodyPrefix)
	if field == "body" {
		field = ""
	}
	return dto.FieldError{Field: field, Code: code, Message: d.Message}
}

// fieldCodes maps huma's validation message templates to field codes. The
// templates come from huma's validation package, so a reworded message in a
// huma upgrade still matches.
var fieldCodes = []struct {
	template string
	code     errcodes.Field
}{
	{validation.MsgExpectedRequiredProperty, errcodes.Required},
	{validation.MsgExpectedDependentRequiredProperty, errcodes.Required},
	{validation.MsgUnexpectedProperty, errcodes.UnknownField},

	{validation.MsgExpectedBoolean, errcodes.Type},
	{validation.MsgExpectedNumber, errcodes.Type},
	{validation.MsgExpectedInteger, errcodes.Type},
	{validation.MsgExpectedString, errcodes.Type},
	{validation.MsgExpectedArray, errcodes.Type},
	{validation.MsgExpectedObject, errcodes.Type},

	{validation.MsgExpectedMinimumNumber, errcodes.Range},
	{validation.MsgExpectedExclusiveMinimumNumber, errcodes.Range},
	{validation.MsgExpectedMaximumNumber, errcodes.Range},
	{validation.MsgExpectedExclusiveMaximumNumber, errcodes.Range},
	{validation.MsgExpectedNumberBeMultipleOf, errcodes.Range},

	{validation.MsgExpectedMinLength, errcodes.Length},
	{validation.MsgExpectedMaxLength, errcodes.Length},
	{validation.MsgExpectedMinItems, errcodes.Length},
	{validation.MsgExpectedMaxItems, errcodes.Length},
	{validation.MsgExpectedMinProperties, errcodes.Length},
	{validation.MsgExpectedMaxProperties, errcodes.Length},

	{validation.MsgExpectedMatchPattern, errcodes.Pattern},
	{validation.MsgExpectedBePattern, errcodes.Pattern},

	{validation.MsgExpectedOneOf, errcodes.Enum},
	{validation.MsgExpectedConst, errcodes.Enum},

	{validation.MsgExpectedArrayItemsUnique, errcodes.Unique},

	{validation.MsgExpectedRFC3339DateTime, errcodes.Format},
	{validation.MsgExpectedRFC1123DateTime, errcodes.Format},
	{validation.MsgExpectedRFC3339Date, errcodes.Format},
	{validation.MsgExpectedRFC3339Time, errcodes.Format},
	{validation.MsgExpectedRFC5322Email, errcodes.Format},
	{validation.MsgExpectedRFC5322EmailBare, errcodes.Format},
	{validation.MsgExpectedRFC5890Hostname, errcodes.Format},
	{validation.MsgExpectedRFC2673IPv4, errcodes.Format},
	{validation.MsgExpectedRFC2373IPv6, errcodes.Format},
	{validation.MsgExpectedRFCIPAddr, errcodes.Format},
	{validation.MsgExpectedRFC3986URI, errcodes.Format},
	{validation.MsgExpectedRFC3986AbsoluteURI, errcodes.Format},
	{validation.MsgExpectedRFC4122UUID, errcodes.Format},
	{validation.MsgExpectedRFC6570URITemplate, errcodes.Format},
	{validation.MsgExpectedRFC6901JSONPointer, errcodes.Format},
	{validation.MsgExpectedRFC6901RelativeJSONPointer, errcodes.Format},
	{validation.MsgExpectedRegexp, errcodes.Format},
	{validation.MsgExpectedDuration, errcodes.Format},
	{validation.MsgExpectedBase64String, errcodes.Format},
}

// paramCodes maps the parse errors of path and query parameters. huma has
// no exported constants for them; the handler tests pin the texts.
var paramCodes = []struct {
	prefix string
	code   errcodes.Field
}{
	{"invalid integer", errcodes.Type},
	{"invalid float", errcodes.Type},
	{"invalid boolean", errcodes.Type},
	{"invalid date/time", errcodes.Format},
	{"invalid url.URL", errcodes.Format},
}

// fieldCode returns the code of a validation message. A template without
// arguments must match exactly; otherwise the longest matching literal
// prefix wins ("expected string" vs "expected string to be RFC 3339 date").
func fieldCode(msg string) errcodes.Field {
	for _, pc := range paramCodes {
		if strings.HasPrefix(msg, pc.prefix) {
			return pc.code
		}
	}
	best, bestLen := errcodes.Invalid, -1
	for _, fc := range fieldCodes {
		prefix, _, hasArgs := strings.Cut(fc.template, "%")
		switch {
		case !hasArgs && msg == fc.template:
			return fc.code
		case hasArgs && strings.HasPrefix(msg, prefix) && len(prefix) > bestLen:
			best, bestLen = fc.code, len(prefix)
		}
	}
	return best
}

// requiredProperty extracts the property name from the "required" message.
func requiredProperty(msg string) (string, bool) {
	prefix, suffix, ok := strings.Cut(validation.MsgExpectedRequiredProperty, "%s")
	if !ok || !strings.HasPrefix(msg, prefix) || !strings.HasSuffix(msg, suffix) {
		return "", false
	}
	name := msg[len(prefix) : len(msg)-len(suffix)]
	return name, name != ""
}
