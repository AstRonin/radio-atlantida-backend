package rotation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// locationName is the name of the station timezone in a fingerprint.
func locationName(loc *time.Location) string {
	if loc == nil {
		return "UTC"
	}
	return loc.String()
}

// checkRange refuses a range that is empty or longer than MaxPlanDays.
func checkRange(r Range) error {
	switch {
	case !r.To.After(r.From):
		return validation.Field("to", validation.Range, "must be after from")
	case r.To.Sub(r.From) > MaxPlanDays*24*time.Hour:
		return validation.Field("to", validation.Range, "at most %d days per request", MaxPlanDays)
	}
	return nil
}

// Plan returns the built hours that start in the range, with their items
// and one-time inserts.
func (s *Service) Plan(ctx context.Context, r Range) ([]PlannedHour, error) {
	if err := checkRange(r); err != nil {
		return nil, validationQuery(err)
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	hs, err := s.store.Plan(ctx, r.From, r.To)
	if err != nil {
		return nil, fmt.Errorf("read track plan: %w", err)
	}
	return hs, nil
}

// validationQuery moves the field errors of a range to the query
// parameters.
func validationQuery(err error) error {
	var v *validation.Error
	if !errors.As(err, &v) {
		return err
	}
	out := &validation.Error{}
	for _, f := range v.Fields {
		if !strings.HasPrefix(f.Field, "query.") {
			f.Field = "query." + f.Field
		}
		out.Fields = append(out.Fields, f)
	}
	return out
}
