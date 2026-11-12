// Package settings stores the saved values of the admin settings in the
// settings table (§6.3 of the design). It implements settings.Store of the
// domain package with the same name; import it as settingsrepo next to the
// domain. Only saved values are stored: defaults live in code.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/settings"
)

// Store is the settings repository (GORM: admin CRUD).
type Store struct {
	g *gorm.DB
}

// New returns a repository on the database.
func New(d *db.DB) *Store { return &Store{g: d.Gorm} }

// WithTx returns a copy of the repository bound to tx.
func (s *Store) WithTx(tx db.Tx) *Store { return &Store{g: tx.Gorm()} }

type row struct {
	Key       string          `gorm:"column:key;primaryKey"`
	Value     json.RawMessage `gorm:"column:value;type:jsonb"`
	UpdatedAt time.Time       `gorm:"column:updated_at"`
}

func (row) TableName() string { return "settings" }

func (r row) stored() settings.Stored {
	return settings.Stored{Key: r.Key, Value: r.Value, UpdatedAt: r.UpdatedAt.UTC()}
}

var columns = []string{"key", "value", "updated_at"}

// List returns every saved value in key order. The table holds one row per
// saved key (a few dozen), so it is read whole.
func (s *Store) List(ctx context.Context) ([]settings.Stored, error) {
	var rows []row
	if err := s.g.WithContext(ctx).Select(columns).Order("key").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("select settings: %w", err)
	}
	out := make([]settings.Stored, len(rows))
	for i, r := range rows {
		out[i] = r.stored()
	}
	return out, nil
}

// Get returns the saved value of key, or settings.ErrNotFound.
func (s *Store) Get(ctx context.Context, key string) (settings.Stored, error) {
	var r row
	err := s.g.WithContext(ctx).Select(columns).Where("key = ?", key).Take(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return settings.Stored{}, settings.ErrNotFound
	}
	if err != nil {
		return settings.Stored{}, fmt.Errorf("select setting %s: %w", key, err)
	}
	return r.stored(), nil
}

// UpsertMany saves the values and returns the stored rows. It is one
// multi-row INSERT ... ON CONFLICT statement, which PostgreSQL runs as one
// implicit transaction: the batch is saved whole or not at all, and a
// concurrent reader never sees half of it.
func (s *Store) UpsertMany(ctx context.Context, sts []settings.Stored) ([]settings.Stored, error) {
	if len(sts) == 0 {
		return []settings.Stored{}, nil
	}
	rows := make([]row, len(sts))
	for i, st := range sts {
		rows[i] = row{Key: st.Key, Value: st.Value, UpdatedAt: st.UpdatedAt.UTC()}
	}
	err := s.g.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "key"}},
			DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at"}),
		}).
		CreateInBatches(&rows, len(rows)).Error
	if err != nil {
		return nil, fmt.Errorf("upsert settings: %w", err)
	}
	out := make([]settings.Stored, len(rows))
	for i, r := range rows {
		out[i] = r.stored()
	}
	return out, nil
}
