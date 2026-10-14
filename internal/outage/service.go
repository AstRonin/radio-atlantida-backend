package outage

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

const (
	// dbTimeout bounds each read or write of a request.
	dbTimeout = 10 * time.Second
	// MaxHistoryDays bounds the range of one played-tracks request. Aired
	// rows are kept 10 days by default; open missed items longer.
	MaxHistoryDays = 31
	// MaxHistoryLimit bounds one page of the played-tracks list.
	MaxHistoryLimit = 500
	// MaxOutageDays bounds the range of one outages request.
	MaxOutageDays = 7
	// missedListLimit bounds the missed items listed; open ones are few, and
	// resolved ones are deleted after the retention period.
	missedListLimit = 500
)

// Service serves the played-tracks list, the missed items, and the outages.
type Service struct {
	store Store
	now   func() time.Time
}

// NewService returns the service. now is the clock.
func NewService(store Store, now func() time.Time) *Service {
	return &Service{store: store, now: now}
}

// History returns a page of the played-tracks list (§7.1): aired, truncated,
// and missed rows, newest first, and the cursor of the next page (nil on the
// last page).
func (s *Service) History(ctx context.Context, f HistoryFilter) ([]Entry, *Cursor, error) {
	if err := checkRange(f.From, f.To, MaxHistoryDays); err != nil {
		return nil, nil, err
	}
	if f.Limit < 1 || f.Limit > MaxHistoryLimit {
		return nil, nil, validation.Field("query.limit", validation.Range, "must be between 1 and %d", MaxHistoryLimit)
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	limit := f.Limit
	f.Limit++ // one more row tells whether there is a next page
	rows, err := s.store.History(ctx, f)
	if err != nil {
		return nil, nil, fmt.Errorf("read play history: %w", err)
	}
	if len(rows) <= limit {
		return rows, nil, nil
	}
	rows = rows[:limit]
	last := rows[limit-1]
	return rows, &Cursor{SortTime: last.SortTime, ID: last.ID}, nil
}

// Missed returns the missed items with the resolution (§7.1).
func (s *Service) Missed(ctx context.Context, r Resolution) ([]Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	rows, err := s.store.Missed(ctx, r, missedListLimit)
	if err != nil {
		return nil, fmt.Errorf("read missed items: %w", err)
	}
	return rows, nil
}

// Dismiss resolves an open missed item: it will not be broadcast again
// (§7.1). The row then follows the play log retention from now.
func (s *Service) Dismiss(ctx context.Context, id uint64) (Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	e, err := s.store.Dismiss(ctx, id, s.now().UTC())
	if err != nil {
		return Entry{}, fmt.Errorf("dismiss missed item %d: %w", id, err)
	}
	return e, nil
}

// OpenOutage returns the open outage; false if there is none.
func (s *Service) OpenOutage(ctx context.Context) (Outage, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	o, ok, err := s.store.OpenOutage(ctx)
	if err != nil {
		return Outage{}, false, fmt.Errorf("read open outage: %w", err)
	}
	return o, ok, nil
}

// Outages returns the outages that overlap [from, to), newest first.
func (s *Service) Outages(ctx context.Context, from, to time.Time) ([]Outage, error) {
	if err := checkRange(from, to, MaxOutageDays); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	out, err := s.store.Outages(ctx, from, to)
	if err != nil {
		return nil, fmt.Errorf("read outages: %w", err)
	}
	return out, nil
}

// checkRange refuses an empty or too long range of query parameters.
func checkRange(from, to time.Time, maxDays int) error {
	if !to.After(from) {
		return validation.Field("query.to", validation.Range, "must be after from")
	}
	if to.Sub(from) > time.Duration(maxDays)*24*time.Hour {
		return validation.Field("query.to", validation.Range, "the range is at most %d days", maxDays)
	}
	return nil
}

// String encodes the cursor for the API: opaque to the client.
func (c Cursor) String() string {
	raw := c.SortTime.UTC().Format(time.RFC3339Nano) + "_" + strconv.FormatUint(c.ID, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// ParseCursor decodes a cursor from the API; a validation error on the
// query parameter cursor when it was not made by Cursor.String.
func ParseCursor(s string) (Cursor, error) {
	invalid := validation.Field("query.cursor", validation.Format, "not a cursor of this list")
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, invalid
	}
	ts, id, ok := strings.Cut(string(raw), "_")
	if !ok {
		return Cursor{}, invalid
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return Cursor{}, invalid
	}
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return Cursor{}, invalid
	}
	return Cursor{SortTime: t.UTC(), ID: n}, nil
}
