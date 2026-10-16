// Package grid stores the grid slots (§4 of the design). It implements
// grid.Store of the domain package with the same name; import it as
// gridrepo next to the domain. GORM: admin CRUD (§5).
//
// Slot dates are station local calendar dates (date columns). They are
// passed as midnight UTC and cast to date, which the UTC session keeps
// exact.
package grid

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/grid"
)

// Slots is the grid slot store.
type Slots struct {
	g *gorm.DB
}

// New returns the store on the database.
func New(d *db.DB) *Slots { return &Slots{g: d.Gorm} }

// WithTx returns the store bound to tx, as the domain's interface type.
func (s *Slots) WithTx(tx db.Tx) grid.Store { return &Slots{g: tx.Gorm()} }

var writeErrors = map[string]error{
	"fk_grid_slots_clock_id":           grid.ErrUnknownClock,
	"ex_grid_slots_weekday_hour_valid": grid.ErrConflict,
}

func writeError(op string, err error) error {
	if ce, ok := db.AsConstraintError(err); ok {
		if mapped, ok := writeErrors[ce.Constraint]; ok {
			return mapped
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}

type slotRow struct {
	ID        uint64
	Weekday   int
	Hour      int
	ValidFrom time.Time
	ValidTo   *time.Time
	ClockID   uint64
	ClockName string
}

func (r slotRow) domain() grid.Slot {
	out := grid.Slot{
		ID: r.ID, Weekday: r.Weekday, Hour: r.Hour, ValidFrom: civil(r.ValidFrom),
		Clock: grid.ClockRef{ID: r.ClockID, Name: r.ClockName},
	}
	if r.ValidTo != nil {
		out.ValidTo = new(civil(*r.ValidTo))
	}
	return out
}

// civil returns the date as midnight UTC, whatever location the driver
// gave it.
func civil(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

const selectSlots = `
SELECT s.id, s.weekday, s.hour, s.valid_from, s.valid_to,
       c.id AS clock_id, c.name AS clock_name
FROM grid_slots s
JOIN clocks c ON c.id = s.clock_id`

func scan(g *gorm.DB, op, sql string, args ...any) ([]grid.Slot, error) {
	var rows []slotRow
	if err := g.Raw(sql, args...).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	out := make([]grid.Slot, len(rows))
	for i, r := range rows {
		out[i] = r.domain()
	}
	return out, nil
}

// List returns the slots that match f in weekday, hour, valid_from order.
// The grid holds at most a few thousand rows (168 hours, a few ranges
// each); the clock filter uses ix_grid_slots_clock_id.
func (s *Slots) List(ctx context.Context, f grid.Filter) ([]grid.Slot, error) {
	var where []string
	var args []any
	if f.ClockID != 0 {
		where = append(where, "s.clock_id = ?")
		args = append(args, f.ClockID)
	}
	if f.From != nil {
		where = append(where, "(s.valid_to IS NULL OR s.valid_to >= CAST(? AS date))")
		args = append(args, *f.From)
	}
	if f.To != nil {
		where = append(where, "s.valid_from <= CAST(? AS date)")
		args = append(args, *f.To)
	}
	sql := selectSlots
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	return scan(s.g.WithContext(ctx), "select grid slots", sql+" ORDER BY s.weekday, s.hour, s.valid_from", args...)
}

// Get returns one slot, or grid.ErrNotFound.
func (s *Slots) Get(ctx context.Context, id uint64) (grid.Slot, error) {
	slots, err := scan(s.g.WithContext(ctx), fmt.Sprintf("select grid slot %d", id), selectSlots+" WHERE s.id = ?", id)
	if err != nil {
		return grid.Slot{}, err
	}
	if len(slots) == 0 {
		return grid.Slot{}, grid.ErrNotFound
	}
	return slots[0], nil
}

// LockOverlapping returns, locked, the slots of the weekday and hour whose
// dates overlap from ... to. The overlap test is the one of
// ex_grid_slots_weekday_hour_valid, so its GiST index serves it.
func (s *Slots) LockOverlapping(ctx context.Context, weekday, hour int, from time.Time, to *time.Time) ([]grid.Slot, error) {
	return scan(s.g.WithContext(ctx), "lock overlapping grid slots", selectSlots+`
WHERE s.weekday = ? AND s.hour = ?
  AND daterange(s.valid_from, s.valid_to, '[]') && daterange(CAST(? AS date), CAST(? AS date), '[]')
ORDER BY s.valid_from
FOR UPDATE OF s`, weekday, hour, from, to)
}

// Insert adds a slot.
func (s *Slots) Insert(ctx context.Context, n grid.NewSlot) (uint64, error) {
	var id uint64
	err := s.g.WithContext(ctx).Raw(`
INSERT INTO grid_slots (weekday, hour, clock_id, valid_from, valid_to)
VALUES (?, ?, ?, CAST(? AS date), CAST(? AS date))
RETURNING id`, n.Weekday, n.Hour, n.ClockID, n.ValidFrom, n.ValidTo).Scan(&id).Error
	if err != nil {
		return 0, writeError("insert grid slot", err)
	}
	return id, nil
}

// SetRange changes the dates of a slot.
func (s *Slots) SetRange(ctx context.Context, id uint64, from time.Time, to *time.Time) error {
	res := s.g.WithContext(ctx).Exec(
		`UPDATE grid_slots SET valid_from = CAST(? AS date), valid_to = CAST(? AS date) WHERE id = ?`, from, to, id)
	if res.Error != nil {
		return writeError(fmt.Sprintf("update grid slot %d", id), res.Error)
	}
	if res.RowsAffected == 0 {
		return grid.ErrNotFound
	}
	return nil
}

// Delete removes a slot. The play log and the track plan keep their rows
// and lose only grid_slot_id (ON DELETE SET NULL).
func (s *Slots) Delete(ctx context.Context, id uint64) error {
	res := s.g.WithContext(ctx).Exec(`DELETE FROM grid_slots WHERE id = ?`, id)
	if res.Error != nil {
		return fmt.Errorf("delete grid slot %d: %w", id, res.Error)
	}
	if res.RowsAffected == 0 {
		return grid.ErrNotFound
	}
	return nil
}
