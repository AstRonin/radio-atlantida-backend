package library

import (
	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

// TrackQueries is the pgx store of the complex track queries: list,
// statistics, usage, the delete of unreferenced tracks, and the planned and
// missed hours. The writes stay on Tracks (GORM).
type TrackQueries struct {
	q db.Querier
}

// NewTrackQueries returns the store.
func NewTrackQueries(q db.Querier) *TrackQueries { return &TrackQueries{q: q} }

// WithTx returns the store bound to tx.
func (s *TrackQueries) WithTx(tx db.Tx) library.TrackQueries { return &TrackQueries{q: tx.Pgx()} }
