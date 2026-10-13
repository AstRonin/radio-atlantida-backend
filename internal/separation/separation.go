// Package separation holds the separation rules (§4, §7 of the design): how
// many hours must pass before the same track, or a track of the same
// artist, airs again. Rotation reads every value from here; none is
// hard-coded. The rows are seeded by the first migration and only edited.
//
// Persistence goes through internal/repository/separation.
package separation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// ErrNotFound is returned when no separation rule has the ID.
var ErrNotFound = errors.New("separation rule not found")

// Scopes of a rule.
const (
	ScopeTrack  = "track"
	ScopeArtist = "artist"
)

// Rule is one separation rule.
type Rule struct {
	ID               int64
	Scope            string
	MinIntervalHours int
	Notes            string
}

// Store persists separation rules (internal/repository/separation).
type Store interface {
	// List returns every rule in ID order.
	List(ctx context.Context) ([]Rule, error)
	// Get returns one rule, or ErrNotFound.
	Get(ctx context.Context, id int64) (Rule, error)
	// Update changes the interval and the notes. ErrNotFound.
	Update(ctx context.Context, id int64, minIntervalHours int, notes string) error
}

// MaxHoursFunc returns the longest interval separation may use: the play
// log keeps aired history only for its retention period (§4.1), so a longer
// interval would be checked against history that is gone.
type MaxHoursFunc func(ctx context.Context) (int, error)

// dbTimeout bounds each statement of the service.
const dbTimeout = 5 * time.Second

// Service manages separation rules.
type Service struct {
	store    Store
	maxHours MaxHoursFunc
}

// NewService returns the service.
func NewService(store Store, maxHours MaxHoursFunc) *Service {
	return &Service{store: store, maxHours: maxHours}
}

// List returns every rule.
func (s *Service) List(ctx context.Context) ([]Rule, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	rs, err := s.store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list separation rules: %w", err)
	}
	return rs, nil
}

// Update changes the interval and the notes of a rule.
func (s *Service) Update(ctx context.Context, id int64, minIntervalHours int, notes string) (Rule, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	limit, err := s.maxHours(ctx)
	if err != nil {
		return Rule{}, fmt.Errorf("read play log retention: %w", err)
	}
	switch {
	case minIntervalHours < 1:
		return Rule{}, validation.Field("min_interval_hours", validation.Range, "must be at least 1")
	case minIntervalHours > limit:
		return Rule{}, validation.Field("min_interval_hours", validation.Range,
			"must be at most %d: the play log keeps aired history for that long (setting retention.play_log_days)", limit)
	}
	if err := s.store.Update(ctx, id, minIntervalHours, strings.TrimSpace(notes)); err != nil {
		return Rule{}, err
	}
	return s.store.Get(ctx, id)
}
