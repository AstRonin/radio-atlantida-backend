package handlers

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	humavalidation "github.com/danielgtaylor/huma/v2/validation"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/grid"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/errcodes"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/media"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	"github.com/AstRonin/radio-atlantida-backend/internal/playout"
	"github.com/AstRonin/radio-atlantida-backend/internal/playrules"
	"github.com/AstRonin/radio-atlantida-backend/internal/rotation"
	"github.com/AstRonin/radio-atlantida-backend/internal/separation"
	"github.com/AstRonin/radio-atlantida-backend/internal/settings"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// problemFor maps an error returned by a domain service to the problem
// response. It is the one place where domain errors become HTTP statuses.
// Unexpected errors are logged here, once, at ERROR, and their text is not
// sent to the client.
func problemFor(ctx context.Context, log *slog.Logger, err error) *dto.Problem {
	var (
		p              *dto.Problem
		invalidSetting *settings.InvalidValueError
		invalid        *validation.Error
		libraryInUse   *library.InUseError
		ruleInUse      *playrules.InUseError
		clockInUse     *clocks.InUseError
	)
	switch {
	case errors.As(err, &p):
		// Already a problem (built by a handler).
	case errors.As(err, &invalid):
		p = dto.NewProblem(http.StatusUnprocessableEntity, errcodes.ValidationFailed, "")
		for _, f := range invalid.Fields {
			p.Errors = append(p.Errors, dto.FieldError{Field: f.Field, Code: validationRuleCodes[f.Rule], Message: f.Message})
		}
	case errors.As(err, &libraryInUse):
		p = dto.NewProblem(http.StatusConflict, inUseCodes[libraryInUse.Err], libraryInUse.Error())
		for _, r := range libraryInUse.UsedBy {
			p.UsedBy = append(p.UsedBy, dto.EntityRef{Kind: r.Kind, ID: r.ID, Name: r.Name})
		}
	case errors.As(err, &ruleInUse):
		p = dto.NewProblem(http.StatusConflict, errcodes.PlayRuleInUse, ruleInUse.Error())
		for _, r := range ruleInUse.UsedBy {
			p.UsedBy = append(p.UsedBy, dto.EntityRef{Kind: r.Kind, ID: r.ID, Name: r.Name})
		}
	case errors.As(err, &clockInUse):
		p = dto.NewProblem(http.StatusConflict, errcodes.ClockInUse, clockInUse.Error())
		for _, r := range clockInUse.UsedBy {
			p.UsedBy = append(p.UsedBy, dto.EntityRef{Kind: r.Kind, ID: r.ID, Name: r.Name})
		}
	case errors.Is(err, media.ErrFileNotFound):
		// The manager sees a popup; the log keeps the path (§6.6).
		log.WarnContext(ctx, "track file not found",
			logging.KeyRequestID, middleware.GetReqID(ctx),
			logging.KeyError, err,
		)
		p = dto.NewProblem(http.StatusNotFound, errcodes.TrackFileNotFound, "the track file is missing under the media root")
	case refusalFor(err) != nil:
		r := refusalFor(err)
		p = dto.NewProblem(r.status, r.code, r.err.Error())
	case errors.Is(err, operations.ErrNotFound):
		p = dto.NewProblem(http.StatusNotFound, errcodes.OperationNotFound, "operation not found")
	case errors.Is(err, logging.ErrFileNotFound):
		p = dto.NewProblem(http.StatusNotFound, errcodes.LogFileNotFound, "log file not found")
	case errors.Is(err, settings.ErrNotFound):
		p = dto.NewProblem(http.StatusNotFound, errcodes.SettingNotFound, "setting not found")
	case errors.Is(err, settings.ErrNotEditable):
		p = dto.NewProblem(http.StatusConflict, errcodes.SettingNotEditable,
			"the setting is a secret: set it in the environment and restart the service")
	case errors.As(err, &invalidSetting):
		p = dto.NewProblem(http.StatusUnprocessableEntity, errcodes.ValidationFailed, "")
		p.Errors = []dto.FieldError{{Field: "value", Code: settingRuleCodes[invalidSetting.Rule], Message: invalidSetting.Message}}
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

// refusal is the response to a domain sentinel error.
type refusal struct {
	err    error
	status int
	code   errcodes.Problem
}

// refusals map domain sentinels; the first match wins.
var refusals = []refusal{
	{library.ErrTypeNotFound, http.StatusNotFound, errcodes.PlaylistTypeNotFound},
	{library.ErrPlaylistNotFound, http.StatusNotFound, errcodes.PlaylistNotFound},
	{library.ErrArtistNotFound, http.StatusNotFound, errcodes.ArtistNotFound},
	{library.ErrTrackNotFound, http.StatusNotFound, errcodes.TrackNotFound},
	{library.ErrSystemType, http.StatusConflict, errcodes.SystemTypeNotDeletable},
	{library.ErrArtistHasHistory, http.StatusConflict, errcodes.ArtistHasHistory},
	{library.ErrTrackHasHistory, http.StatusConflict, errcodes.TrackHasHistory},
	{playrules.ErrNotFound, http.StatusNotFound, errcodes.PlayRuleNotFound},
	{playrules.ErrTrackNotFound, http.StatusNotFound, errcodes.TrackNotFound},
	{playrules.ErrPlaylistNotFound, http.StatusNotFound, errcodes.PlaylistNotFound},
	{separation.ErrNotFound, http.StatusNotFound, errcodes.SeparationRuleNotFound},
	{clocks.ErrNotFound, http.StatusNotFound, errcodes.ClockNotFound},
	{clocks.ErrHasHistory, http.StatusConflict, errcodes.ClockHasHistory},
	{grid.ErrNotFound, http.StatusNotFound, errcodes.GridSlotNotFound},
	{grid.ErrConflict, http.StatusConflict, errcodes.GridConflict},
	{rotation.ErrRequestNotFound, http.StatusNotFound, errcodes.FillerRequestNotFound},
	{rotation.ErrRequestClosed, http.StatusConflict, errcodes.FillerRequestClosed},
	{rotation.ErrNotAnswerable, http.StatusConflict, errcodes.FillerRequestNotAnswerable},
	{rotation.ErrHourStarted, http.StatusConflict, errcodes.HourStarted},
	{playout.ErrUnavailable, http.StatusServiceUnavailable, errcodes.LiquidsoapUnavailable},
}

func refusalFor(err error) *refusal {
	for i := range refusals {
		if errors.Is(err, refusals[i].err) {
			return &refusals[i]
		}
	}
	return nil
}

// inUseCodes map the refusals inside a library.InUseError.
var inUseCodes = map[error]errcodes.Problem{
	library.ErrTypeInUse:     errcodes.PlaylistTypeInUse,
	library.ErrPlaylistInUse: errcodes.PlaylistInUse,
	library.ErrArtistInUse:   errcodes.ArtistInUse,
	library.ErrTrackInUse:    errcodes.TrackInUse,
}

// validationRuleCodes map the rules of a domain validation error to field
// codes.
var validationRuleCodes = map[validation.Rule]errcodes.Field{
	validation.Required:   errcodes.Required,
	validation.Range:      errcodes.Range,
	validation.Format:     errcodes.Format,
	validation.Taken:      errcodes.Taken,
	validation.UnknownID:  errcodes.UnknownID,
	validation.Invalid:    errcodes.Invalid,
	validation.Overlap:    errcodes.Overlap,
	validation.DoesNotFit: errcodes.DoesNotFit,
	validation.Order:      errcodes.Order,
	validation.Blocked:    errcodes.Blocked,
}

// settingRuleCodes maps the rules of a refused setting value to field codes.
var settingRuleCodes = map[settings.Rule]errcodes.Field{
	settings.RuleType:         errcodes.Type,
	settings.RuleRange:        errcodes.Range,
	settings.RuleLength:       errcodes.Length,
	settings.RuleEnum:         errcodes.Enum,
	settings.RuleFormat:       errcodes.Format,
	settings.RuleRequired:     errcodes.Required,
	settings.RuleUnknownField: errcodes.UnknownField,
	settings.RuleInvalid:      errcodes.Invalid,
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
	{humavalidation.MsgExpectedRequiredProperty, errcodes.Required},
	{humavalidation.MsgExpectedDependentRequiredProperty, errcodes.Required},
	{humavalidation.MsgUnexpectedProperty, errcodes.UnknownField},

	{humavalidation.MsgExpectedBoolean, errcodes.Type},
	{humavalidation.MsgExpectedNumber, errcodes.Type},
	{humavalidation.MsgExpectedInteger, errcodes.Type},
	{humavalidation.MsgExpectedString, errcodes.Type},
	{humavalidation.MsgExpectedArray, errcodes.Type},
	{humavalidation.MsgExpectedObject, errcodes.Type},

	{humavalidation.MsgExpectedMinimumNumber, errcodes.Range},
	{humavalidation.MsgExpectedExclusiveMinimumNumber, errcodes.Range},
	{humavalidation.MsgExpectedMaximumNumber, errcodes.Range},
	{humavalidation.MsgExpectedExclusiveMaximumNumber, errcodes.Range},
	{humavalidation.MsgExpectedNumberBeMultipleOf, errcodes.Range},

	{humavalidation.MsgExpectedMinLength, errcodes.Length},
	{humavalidation.MsgExpectedMaxLength, errcodes.Length},
	{humavalidation.MsgExpectedMinItems, errcodes.Length},
	{humavalidation.MsgExpectedMaxItems, errcodes.Length},
	{humavalidation.MsgExpectedMinProperties, errcodes.Length},
	{humavalidation.MsgExpectedMaxProperties, errcodes.Length},

	{humavalidation.MsgExpectedMatchPattern, errcodes.Pattern},
	{humavalidation.MsgExpectedBePattern, errcodes.Pattern},

	{humavalidation.MsgExpectedOneOf, errcodes.Enum},
	{humavalidation.MsgExpectedConst, errcodes.Enum},

	{humavalidation.MsgExpectedArrayItemsUnique, errcodes.Unique},

	{humavalidation.MsgExpectedRFC3339DateTime, errcodes.Format},
	{humavalidation.MsgExpectedRFC1123DateTime, errcodes.Format},
	{humavalidation.MsgExpectedRFC3339Date, errcodes.Format},
	{humavalidation.MsgExpectedRFC3339Time, errcodes.Format},
	{humavalidation.MsgExpectedRFC5322Email, errcodes.Format},
	{humavalidation.MsgExpectedRFC5322EmailBare, errcodes.Format},
	{humavalidation.MsgExpectedRFC5890Hostname, errcodes.Format},
	{humavalidation.MsgExpectedRFC2673IPv4, errcodes.Format},
	{humavalidation.MsgExpectedRFC2373IPv6, errcodes.Format},
	{humavalidation.MsgExpectedRFCIPAddr, errcodes.Format},
	{humavalidation.MsgExpectedRFC3986URI, errcodes.Format},
	{humavalidation.MsgExpectedRFC3986AbsoluteURI, errcodes.Format},
	{humavalidation.MsgExpectedRFC4122UUID, errcodes.Format},
	{humavalidation.MsgExpectedRFC6570URITemplate, errcodes.Format},
	{humavalidation.MsgExpectedRFC6901JSONPointer, errcodes.Format},
	{humavalidation.MsgExpectedRFC6901RelativeJSONPointer, errcodes.Format},
	{humavalidation.MsgExpectedRegexp, errcodes.Format},
	{humavalidation.MsgExpectedDuration, errcodes.Format},
	{humavalidation.MsgExpectedBase64String, errcodes.Format},
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
	prefix, suffix, ok := strings.Cut(humavalidation.MsgExpectedRequiredProperty, "%s")
	if !ok || !strings.HasPrefix(msg, prefix) || !strings.HasSuffix(msg, suffix) {
		return "", false
	}
	name := msg[len(prefix) : len(msg)-len(suffix)]
	return name, name != ""
}
