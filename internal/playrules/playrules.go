// Package playrules holds the play rules (§7.3 of the design): named,
// reusable limits on when a track may play, applied to tracks or to whole
// playlists. They replace dayparting.
//
// A rule may combine a time window of the day (station local time, may
// cross midnight) with weekdays, a minimum interval between starts of the
// same track, and a maximum number of starts per clock hour. At least one
// part is set. A rule only limits; how often a slot appears is set by the
// clock. Rotation checks the rules and never relaxes them (later phase).
//
// Persistence goes through internal/repository/playrules.
package playrules

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// Errors returned by the service.
var (
	ErrNotFound         = errors.New("play rule not found")
	ErrTrackNotFound    = errors.New("track not found")
	ErrPlaylistNotFound = errors.New("playlist not found")
	// ErrInUse: the rule is applied to tracks or playlists; returned inside
	// an *InUseError.
	ErrInUse = errors.New("play rule is in use")
)

// Errors returned by the store; the service turns them into field errors.
var (
	ErrNameTaken       = errors.New("play rule name is taken")
	ErrUnknownRule     = errors.New("unknown play rule")
	ErrUnknownTrack    = errors.New("unknown track")
	ErrUnknownPlaylist = errors.New("unknown playlist")
)

// TimeOfDay is a time of day in station local time, in minutes after
// midnight (0 to 1439).
type TimeOfDay int

// ParseTimeOfDay parses "HH:MM" (24-hour).
func ParseTimeOfDay(s string) (TimeOfDay, error) {
	t, err := time.Parse("15:04", s)
	if err != nil || len(s) != len("15:04") {
		return 0, fmt.Errorf("time of day %q is not HH:MM", s)
	}
	return TimeOfDay(t.Hour()*60 + t.Minute()), nil
}

// String formats the time as "HH:MM".
func (t TimeOfDay) String() string { return fmt.Sprintf("%02d:%02d", int(t)/60, int(t)%60) }

// Rule is a stored play rule with its targets.
type Rule struct {
	ID uint64
	RuleInput
	CreatedAt time.Time
	// TrackIDs and PlaylistIDs are the targets, in ID order.
	TrackIDs    []uint64
	PlaylistIDs []uint64
}

// RuleInput is the editable part of a rule. Nil parts do not limit.
type RuleInput struct {
	Name       string
	WindowFrom *TimeOfDay
	WindowTo   *TimeOfDay
	// Weekdays are ISO numbers (1 = Monday ... 7 = Sunday). With a window
	// that crosses midnight, the day on which the window starts.
	Weekdays           []int
	MinIntervalMinutes *int
	MaxPerHour         *int
	Notes              string
}

// Ref names a target that blocks a delete.
type Ref struct {
	Kind string
	ID   uint64
	Name string
}

// Kinds of targets.
const (
	KindTrack    = "track"
	KindPlaylist = "playlist"
)

// MaxUsers bounds the targets of each kind listed in an *InUseError.
const MaxUsers = 50

// InUseError refuses the delete of a rule that is applied to tracks or
// playlists, and names them.
type InUseError struct {
	UsedBy []Ref
}

func (e *InUseError) Error() string { return ErrInUse.Error() }

func (e *InUseError) Unwrap() error { return ErrInUse }

// Store persists play rules and their links (internal/repository/playrules).
type Store interface {
	// WithTx returns the store bound to tx.
	WithTx(tx db.Tx) Store
	// List returns every rule with its targets, in name order.
	List(ctx context.Context) ([]Rule, error)
	// Get returns one rule, or ErrNotFound.
	Get(ctx context.Context, id uint64) (Rule, error)
	// Create adds a rule. ErrNameTaken.
	Create(ctx context.Context, in RuleInput) (uint64, error)
	// Update changes a rule. ErrNotFound, ErrNameTaken.
	Update(ctx context.Context, id uint64, in RuleInput) error
	// Delete removes a rule. ErrNotFound; ErrInUse while it has targets.
	Delete(ctx context.Context, id uint64) error
	// Users returns the tracks and playlists the rule is applied to, at
	// most limit of each kind.
	Users(ctx context.Context, id uint64, limit int) ([]Ref, error)
	// LockRule, LockTrack, and LockPlaylist keep the row from being deleted
	// until the transaction ends. ErrNotFound, ErrTrackNotFound,
	// ErrPlaylistNotFound.
	LockRule(ctx context.Context, id uint64) error
	LockTrack(ctx context.Context, id uint64) error
	LockPlaylist(ctx context.Context, id uint64) error
	// SetTargets makes trackIDs and playlistIDs the targets of the rule.
	// ErrUnknownTrack, ErrUnknownPlaylist.
	SetTargets(ctx context.Context, ruleID uint64, trackIDs, playlistIDs []uint64) error
	// SetTrackRules makes ruleIDs the rules of the track. ErrUnknownRule.
	SetTrackRules(ctx context.Context, trackID uint64, ruleIDs []uint64) error
	// SetPlaylistRules makes ruleIDs the rules of the playlist.
	// ErrUnknownRule.
	SetPlaylistRules(ctx context.Context, playlistID uint64, ruleIDs []uint64) error
	// TrackRules and PlaylistRules return the rule IDs of a track or a
	// playlist, in ID order.
	TrackRules(ctx context.Context, trackID uint64) ([]uint64, error)
	PlaylistRules(ctx context.Context, playlistID uint64) ([]uint64, error)
}

// TxRunner runs fn in one transaction (*db.DB).
type TxRunner interface {
	WithTx(ctx context.Context, fn func(ctx context.Context, tx db.Tx) error) error
}

// dbTimeout bounds each statement or transaction of the service.
const dbTimeout = 5 * time.Second

// Service manages play rules.
type Service struct {
	tx    TxRunner
	store Store
}

// NewService returns the service.
func NewService(tx TxRunner, store Store) *Service {
	return &Service{tx: tx, store: store}
}

// List returns every rule with its targets.
func (s *Service) List(ctx context.Context) ([]Rule, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	rs, err := s.store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list play rules: %w", err)
	}
	return rs, nil
}

// Get returns one rule.
func (s *Service) Get(ctx context.Context, id uint64) (Rule, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	return s.store.Get(ctx, id)
}

// Create adds a rule without targets.
func (s *Service) Create(ctx context.Context, in RuleInput) (Rule, error) {
	in, err := Validate(in)
	if err != nil {
		return Rule{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	id, err := s.store.Create(ctx, in)
	if err != nil {
		return Rule{}, asFieldError(err)
	}
	return s.store.Get(ctx, id)
}

// Update changes a rule; its targets stay.
func (s *Service) Update(ctx context.Context, id uint64, in RuleInput) (Rule, error) {
	in, err := Validate(in)
	if err != nil {
		return Rule{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	if err := s.store.Update(ctx, id, in); err != nil {
		return Rule{}, asFieldError(err)
	}
	return s.store.Get(ctx, id)
}

// Delete deletes a rule that is applied to no track or playlist. An
// *InUseError names the targets.
func (s *Service) Delete(ctx context.Context, id uint64) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	err := s.store.Delete(ctx, id)
	if !errors.Is(err, ErrInUse) {
		return err
	}
	users, err := s.store.Users(ctx, id, MaxUsers)
	if err != nil {
		return fmt.Errorf("list users of play rule %d: %w", id, err)
	}
	return &InUseError{UsedBy: users}
}

// SetTargets makes trackIDs and playlistIDs the targets of a rule, in one
// transaction.
func (s *Service) SetTargets(ctx context.Context, id uint64, trackIDs, playlistIDs []uint64) (Rule, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	err := s.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		st := s.store.WithTx(tx)
		if err := st.LockRule(ctx, id); err != nil {
			return err
		}
		return st.SetTargets(ctx, id, trackIDs, playlistIDs)
	})
	if err != nil {
		return Rule{}, asFieldError(err)
	}
	return s.store.Get(ctx, id)
}

// SetTrackRules makes ruleIDs the rules of one track and returns them.
func (s *Service) SetTrackRules(ctx context.Context, trackID uint64, ruleIDs []uint64) ([]uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	err := s.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		st := s.store.WithTx(tx)
		if err := st.LockTrack(ctx, trackID); err != nil {
			return err
		}
		return st.SetTrackRules(ctx, trackID, ruleIDs)
	})
	if err != nil {
		return nil, asFieldError(err)
	}
	return s.store.TrackRules(ctx, trackID)
}

// SetPlaylistRules makes ruleIDs the rules of one playlist and returns
// them. They apply to every track of the playlist, each checked on its own.
func (s *Service) SetPlaylistRules(ctx context.Context, playlistID uint64, ruleIDs []uint64) ([]uint64, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	err := s.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		st := s.store.WithTx(tx)
		if err := st.LockPlaylist(ctx, playlistID); err != nil {
			return err
		}
		return st.SetPlaylistRules(ctx, playlistID, ruleIDs)
	})
	if err != nil {
		return nil, asFieldError(err)
	}
	return s.store.PlaylistRules(ctx, playlistID)
}

// Validate checks a rule and returns it normalised (trimmed name and notes,
// weekdays in order). The database enforces the same invariants
// (ck_play_rules_*); this check names the field.
func Validate(in RuleInput) (RuleInput, error) {
	errs := &validation.Error{}
	in.Name = strings.TrimSpace(in.Name)
	in.Notes = strings.TrimSpace(in.Notes)
	if in.Name == "" {
		errs.Add("name", validation.Required, "must not be blank")
	}

	switch {
	case in.WindowFrom != nil && in.WindowTo == nil:
		errs.Add("window_to", validation.Required, "a window needs both window_from and window_to")
	case in.WindowFrom == nil && in.WindowTo != nil:
		errs.Add("window_from", validation.Required, "a window needs both window_from and window_to")
	case in.WindowFrom != nil && *in.WindowFrom == *in.WindowTo:
		errs.Add("window_to", validation.Invalid, "must differ from window_from; a window may cross midnight")
	}
	for _, t := range []struct {
		field string
		v     *TimeOfDay
	}{{"window_from", in.WindowFrom}, {"window_to", in.WindowTo}} {
		if t.v != nil && (*t.v < 0 || *t.v >= 24*60) {
			errs.Add(t.field, validation.Range, "must be between 00:00 and 23:59")
		}
	}

	if in.Weekdays != nil {
		days := slices.Clone(in.Weekdays)
		slices.Sort(days)
		switch {
		case len(days) == 0:
			errs.Add("weekdays", validation.Required, "omit weekdays for every day, or list at least one")
		case days[0] < 1 || days[len(days)-1] > 7:
			errs.Add("weekdays", validation.Range, "weekdays are 1 (Monday) to 7 (Sunday)")
		case len(slices.Compact(slices.Clone(days))) != len(days):
			errs.Add("weekdays", validation.Invalid, "a weekday is listed twice")
		}
		in.Weekdays = days
	}
	if in.MinIntervalMinutes != nil && *in.MinIntervalMinutes < 1 {
		errs.Add("min_interval_minutes", validation.Range, "must be at least 1")
	}
	if in.MaxPerHour != nil && *in.MaxPerHour < 1 {
		errs.Add("max_per_hour", validation.Range, "must be at least 1")
	}
	if in.WindowFrom == nil && in.WindowTo == nil && in.Weekdays == nil &&
		in.MinIntervalMinutes == nil && in.MaxPerHour == nil {
		errs.Add("", validation.Required,
			"set at least one limit: a window, weekdays, min_interval_minutes, or max_per_hour")
	}
	return in, errs.Err()
}

func asFieldError(err error) error {
	switch {
	case errors.Is(err, ErrNameTaken):
		return validation.Field("name", validation.Taken, "another play rule has this name")
	case errors.Is(err, ErrUnknownRule):
		return validation.Field("rule_ids", validation.UnknownID, "a play rule ID does not exist")
	case errors.Is(err, ErrUnknownTrack):
		return validation.Field("track_ids", validation.UnknownID, "a track ID does not exist")
	case errors.Is(err, ErrUnknownPlaylist):
		return validation.Field("playlist_ids", validation.UnknownID, "a playlist ID does not exist")
	}
	return err
}
