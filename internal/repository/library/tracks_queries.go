package library

import (
	"gorm.io/gorm"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

// TrackQueries is the pgx store of the complex track queries: list,
// statistics, usage, the delete of unreferenced tracks, and the planned and
// missed hours. The writes stay on Tracks (GORM).
type TrackQueries struct {
	q db.Querier
	g *gorm.DB
}

// NewTrackQueries returns the store on the database.
func NewTrackQueries(d *db.DB) *TrackQueries { return &TrackQueries{q: d.Pool, g: d.Gorm} }

// WithTx returns the store bound to tx.
func (s *TrackQueries) WithTx(tx db.Tx) library.TrackQueries {
	return &TrackQueries{q: tx.Pgx(), g: tx.Gorm()}
}
