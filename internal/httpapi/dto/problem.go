// Package dto holds the request and response bodies of the API. Database
// structs are never sent or accepted directly; every endpoint has its own
// DTOs, and the OpenAPI document is generated from them.
package dto

import (
	"net/http"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/errcodes"
)

// ContentTypeProblem is the media type of every error response.
const ContentTypeProblem = "application/problem+json"

// Problem is the one error body of the API (RFC 9457 problem details). The
// panel reacts to Code and to the codes of Errors; Title and Detail are
// English and meant for logs and developers.
type Problem struct {
	Title     string           `json:"title" doc:"Short English summary of the problem type."`
	Status    int              `json:"status" doc:"HTTP status code."`
	Code      errcodes.Problem `json:"code"`
	Detail    string           `json:"detail,omitempty" doc:"English explanation of this occurrence."`
	Errors    []FieldError     `json:"errors,omitempty" nullable:"false" doc:"Per-field errors (validation)."`
	UsedBy    []EntityRef      `json:"used_by,omitempty" nullable:"false" doc:"Entities that block a refused request, for example the playlists that use a type."`
	RequestID string           `json:"request_id,omitempty" doc:"ID of the request in the logs."`
}

// FieldError is the error of one request field.
type FieldError struct {
	Field   string         `json:"field" doc:"Path of the field, for example elements[2].fixed_minute. Query and path parameters are prefixed with query. or path."`
	Code    errcodes.Field `json:"code"`
	Message string         `json:"message" doc:"English message for developers."`
}

// EntityRef names an entity in a refusal.
type EntityRef struct {
	Kind string `json:"kind" doc:"Entity kind, for example playlist, track, clock."`
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// NewProblem returns a problem with the standard title of the status.
func NewProblem(status int, code errcodes.Problem, detail string) *Problem {
	return &Problem{Title: http.StatusText(status), Status: status, Code: code, Detail: detail}
}

// Error implements error, so handlers can return a *Problem.
func (p *Problem) Error() string {
	if p.Detail != "" {
		return p.Detail
	}
	return p.Title
}

// GetStatus implements huma.StatusError.
func (p *Problem) GetStatus() int { return p.Status }

// ContentType implements huma.ContentTypeFilter: problems are sent as
// application/problem+json.
func (p *Problem) ContentType(string) string { return ContentTypeProblem }
