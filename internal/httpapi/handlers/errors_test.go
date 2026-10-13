package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/validation"
	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/errcodes"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
)

func TestFieldCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		msg  string
		want errcodes.Field
	}{
		{fmt.Sprintf(validation.MsgExpectedRequiredProperty, "name"), errcodes.Required},
		{validation.MsgUnexpectedProperty, errcodes.UnknownField},
		{validation.MsgExpectedString, errcodes.Type},
		{validation.MsgExpectedInteger, errcodes.Type},
		{fmt.Sprintf(validation.MsgExpectedMinimumNumber, 1), errcodes.Range},
		{fmt.Sprintf(validation.MsgExpectedMaximumNumber, 59), errcodes.Range},
		{fmt.Sprintf(validation.MsgExpectedMinLength, 1), errcodes.Length},
		{fmt.Sprintf(validation.MsgExpectedMaxItems, 3), errcodes.Length},
		{fmt.Sprintf(validation.MsgExpectedMatchPattern, "^a$"), errcodes.Pattern},
		{fmt.Sprintf(validation.MsgExpectedOneOf, "flow, fixed"), errcodes.Enum},
		{validation.MsgExpectedRFC3339DateTime, errcodes.Format},
		{validation.MsgExpectedArrayItemsUnique, errcodes.Unique},
		{"invalid integer", errcodes.Type},
		{"invalid date/time for format 2006-01-02T15:04:05Z07:00", errcodes.Format},
		{"something else", errcodes.Invalid},
	}
	for _, tt := range tests {
		t.Run(tt.msg, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, fieldCode(tt.msg))
		})
	}
}

func TestNewHumaErrorValidation(t *testing.T) {
	t.Parallel()

	err := NewHumaError(nil, http.StatusUnprocessableEntity, "validation failed",
		&huma.ErrorDetail{
			Location: "body.elements[2]",
			Message:  fmt.Sprintf(validation.MsgExpectedRequiredProperty, "fixed_minute"),
		},
		&huma.ErrorDetail{
			Location: "body.elements[0].fixed_minute",
			Message:  fmt.Sprintf(validation.MsgExpectedMaximumNumber, 59),
			Value:    61,
		},
		&huma.ErrorDetail{Location: "query.limit", Message: fmt.Sprintf(validation.MsgExpectedMinimumNumber, 1)},
		&huma.ErrorDetail{Location: "body", Message: fmt.Sprintf(validation.MsgExpectedRequiredProperty, "name")},
	)

	p := requireProblem(t, err)
	require.Equal(t, errcodes.ValidationFailed, p.Code)
	require.Equal(t, []dto.FieldError{
		{Field: "elements[2].fixed_minute", Code: errcodes.Required, Message: "expected required property fixed_minute to be present"},
		{Field: "elements[0].fixed_minute", Code: errcodes.Range, Message: "expected number <= 59"},
		{Field: "query.limit", Code: errcodes.Range, Message: "expected number >= 1"},
		{Field: "name", Code: errcodes.Required, Message: "expected required property name to be present"},
	}, p.Errors)
}

func TestNewHumaErrorClientAndServerErrors(t *testing.T) {
	t.Parallel()
	secretBody := `{"password":"hunter2"`

	bad := requireProblem(t, NewHumaError(nil, http.StatusBadRequest, "validation failed",
		&huma.ErrorDetail{Location: "body", Message: "unexpected end of JSON input", Value: secretBody}))
	require.Equal(t, errcodes.BadRequest, bad.Code)
	require.Equal(t, "validation failed: unexpected end of JSON input", bad.Detail)
	require.Empty(t, bad.Errors)

	internal := requireProblem(t, NewHumaError(nil, http.StatusInternalServerError, "unexpected error occurred",
		errors.New("dial tcp 10.0.0.1:5432: connection refused")))
	require.Equal(t, errcodes.InternalError, internal.Code)
	require.Empty(t, internal.Detail)

	for status, code := range map[int]errcodes.Problem{
		http.StatusNotFound:              errcodes.NotFound,
		http.StatusRequestEntityTooLarge: errcodes.BodyTooLarge,
		http.StatusUnsupportedMediaType:  errcodes.UnsupportedMediaType,
		http.StatusRequestTimeout:        errcodes.BadRequest,
	} {
		require.Equal(t, code, requireProblem(t, NewHumaError(nil, status, "x")).Code, status)
	}
}

func TestProblemForMapsDomainErrorsAndLogsUnexpectedOnce(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	log := slog.New(logging.NewHandler(&logs, slog.LevelDebug))
	ctx := t.Context()

	require.Equal(t, errcodes.OperationNotFound,
		problemFor(ctx, log, fmt.Errorf("get: %w", operations.ErrNotFound)).Code)
	require.Equal(t, errcodes.LogFileNotFound, problemFor(ctx, log, logging.ErrFileNotFound).Code)
	require.Empty(t, logs.String())

	p := problemFor(ctx, log, errors.New("boom"))
	require.Equal(t, http.StatusInternalServerError, p.Status)
	require.Empty(t, p.Detail)
	var entry map[string]any
	require.NoError(t, json.Unmarshal(logs.Bytes(), &entry))
	require.Equal(t, "ERROR", entry["level"])
	require.Equal(t, "boom", entry["err"])

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	logs.Reset()
	require.Equal(t, http.StatusServiceUnavailable, problemFor(cancelled, log, context.Canceled).Status)
	require.Empty(t, logs.String())
}

func requireProblem(t *testing.T, err error) *dto.Problem {
	t.Helper()
	var p *dto.Problem
	require.ErrorAs(t, err, &p)
	return p
}
