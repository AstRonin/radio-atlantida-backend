package rotation

import (
	"context"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
)

// queryCount counts the store calls of a pass: the "queries" of its log line
// and of a simulation. One call is one repository method, whatever statements
// it runs (ReplaceHour runs three).
type queryCount struct {
	n int
}

// add counts n calls; a nil count counts nothing.
func (c *queryCount) add(n int) {
	if c != nil {
		c.n += n
	}
}

// countingStore counts the calls it passes to a Store.
type countingStore struct {
	Store
	n *queryCount
}

// WithTx returns the counting store bound to tx.
func (c countingStore) WithTx(tx db.Tx) Store {
	return countingStore{Store: c.Store.WithTx(tx), n: c.n}
}

func (c countingStore) LockPlan(ctx context.Context) error {
	c.n.add(1)
	return c.Store.LockPlan(ctx)
}

func (c countingStore) Library(ctx context.Context, scope LibraryScope) (Library, error) {
	c.n.add(1)
	return c.Store.Library(ctx, scope)
}

func (c countingStore) TracksOf(ctx context.Context, ids []uint64) ([]Track, error) {
	c.n.add(1)
	return c.Store.TracksOf(ctx, ids)
}

func (c countingStore) History(ctx context.Context, from, to, hourStart time.Time) ([]Play, error) {
	c.n.add(1)
	return c.Store.History(ctx, from, to, hourStart)
}

func (c countingStore) Window(ctx context.Context, from, to time.Time) ([]Play, error) {
	c.n.add(1)
	return c.Store.Window(ctx, from, to)
}

func (c countingStore) PlanHours(ctx context.Context, from, to time.Time) ([]PlanHour, error) {
	c.n.add(1)
	return c.Store.PlanHours(ctx, from, to)
}

func (c countingStore) PlanHour(ctx context.Context, start time.Time) (PlanHour, bool, error) {
	c.n.add(1)
	return c.Store.PlanHour(ctx, start)
}

func (c countingStore) ReplaceHour(ctx context.Context, h PlanHour, items []Item) error {
	c.n.add(1)
	return c.Store.ReplaceHour(ctx, h, items)
}

func (c countingStore) ClearHour(ctx context.Context, start time.Time) error {
	c.n.add(1)
	return c.Store.ClearHour(ctx, start)
}

func (c countingStore) Plan(ctx context.Context, from, to time.Time) ([]PlannedHour, error) {
	c.n.add(1)
	return c.Store.Plan(ctx, from, to)
}

func (c countingStore) Missed(ctx context.Context, id uint64) (MissedItem, error) {
	c.n.add(1)
	return c.Store.Missed(ctx, id)
}

func (c countingStore) LockMissed(ctx context.Context, id uint64) (MissedItem, error) {
	c.n.add(1)
	return c.Store.LockMissed(ctx, id)
}

func (c countingStore) Inserts(ctx context.Context, hourStart time.Time) ([]Insert, error) {
	c.n.add(1)
	return c.Store.Inserts(ctx, hourStart)
}

func (c countingStore) InsertsRange(ctx context.Context, from, to time.Time) ([]Insert, error) {
	c.n.add(1)
	return c.Store.InsertsRange(ctx, from, to)
}

func (c countingStore) QueuedHours(ctx context.Context, from, to time.Time) ([]time.Time, error) {
	c.n.add(1)
	return c.Store.QueuedHours(ctx, from, to)
}

func (c countingStore) Insert(ctx context.Context, id uint64) (Insert, error) {
	c.n.add(1)
	return c.Store.Insert(ctx, id)
}

func (c countingStore) CreateInsert(ctx context.Context, ins Insert) (uint64, error) {
	c.n.add(1)
	return c.Store.CreateInsert(ctx, ins)
}

func (c countingStore) DeleteInsert(ctx context.Context, id uint64) error {
	c.n.add(1)
	return c.Store.DeleteInsert(ctx, id)
}

func (c countingStore) ReopenMissed(ctx context.Context, missedID uint64) error {
	c.n.add(1)
	return c.Store.ReopenMissed(ctx, missedID)
}

func (c countingStore) LinkRescheduled(ctx context.Context, missedID uint64, at time.Time) error {
	c.n.add(1)
	return c.Store.LinkRescheduled(ctx, missedID, at)
}
