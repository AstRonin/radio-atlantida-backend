// Package separation stores the separation rules (§4 of the design). It
// implements separation.Store of the domain package with the same name;
// import it as separationrepo next to the domain. GORM: admin CRUD (§5).
package separation

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/separation"
)

// Store is the separation rule repository.
type Store struct {
	g *gorm.DB
}

// New returns a repository on the database.
func New(d *db.DB) *Store { return &Store{g: d.Gorm} }

type row struct {
	ID               int64
	Scope            string
	MinIntervalHours int
	Notes            string
}

func (row) TableName() string { return "separation_rules" }

func (r row) domain() separation.Rule {
	return separation.Rule{ID: r.ID, Scope: r.Scope, MinIntervalHours: r.MinIntervalHours, Notes: r.Notes}
}

var columns = []string{"id", "scope", "min_interval_hours", "notes"}

// List returns every rule in ID order (a handful of rows).
func (s *Store) List(ctx context.Context) ([]separation.Rule, error) {
	var rows []row
	if err := s.g.WithContext(ctx).Select(columns).Order("id").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("select separation rules: %w", err)
	}
	out := make([]separation.Rule, len(rows))
	for i, r := range rows {
		out[i] = r.domain()
	}
	return out, nil
}

// Get returns one rule, or separation.ErrNotFound.
func (s *Store) Get(ctx context.Context, id int64) (separation.Rule, error) {
	var r row
	err := s.g.WithContext(ctx).Select(columns).Where("id = ?", id).Take(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return separation.Rule{}, separation.ErrNotFound
	}
	if err != nil {
		return separation.Rule{}, fmt.Errorf("select separation rule %d: %w", id, err)
	}
	return r.domain(), nil
}

// Update changes the interval and the notes of a rule.
func (s *Store) Update(ctx context.Context, id int64, minIntervalHours int, notes string) error {
	res := s.g.WithContext(ctx).Model(&row{}).Where("id = ?", id).
		Updates(map[string]any{"min_interval_hours": minIntervalHours, "notes": notes})
	if res.Error != nil {
		return fmt.Errorf("update separation rule %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return separation.ErrNotFound
	}
	return nil
}
