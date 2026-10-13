// Package settings holds the admin settings of the backend (§6.3 of the
// design): every runtime setting except the database connection, secrets,
// and host paths, which come from the environment (internal/config).
//
// Each setting has a typed definition in code with its default, validation,
// and apply mode. Only values the manager saved are stored (the settings
// table, through internal/repository/settings); an unsaved key reads as its
// code default. Secrets are definitions too, but they are never stored or
// returned: the API reports only whether the environment variable is set.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
)

// Errors returned by the service.
var (
	// ErrNotFound: no setting has the key. A store also returns it for a
	// key without a saved value.
	ErrNotFound = errors.New("setting not found")
	// ErrNotEditable: the key is a secret; it is changed in the environment.
	ErrNotEditable = errors.New("setting is not editable")
)

// Rule names the rule a refused value breaks. The API maps it to a field
// error code.
type Rule string

// Rules of InvalidValueError.
const (
	RuleType   Rule = "type"
	RuleRange  Rule = "range"
	RuleLength Rule = "length"
	RuleEnum   Rule = "enum"
	RuleFormat Rule = "format"
	// RuleRequired: a property of an object value is missing.
	RuleRequired Rule = "required"
	// RuleUnknownField: an object value has a property it does not define.
	RuleUnknownField Rule = "unknown_field"
	// RuleInvalid: any other problem.
	RuleInvalid Rule = "invalid"
)

// InvalidValueError refuses a value that fails the definition of its key.
type InvalidValueError struct {
	Key     string
	Rule    Rule
	Message string
}

func (e *InvalidValueError) Error() string {
	return fmt.Sprintf("invalid value for setting %s: %s", e.Key, e.Message)
}

// Stored is a saved value.
type Stored struct {
	Key       string
	Value     json.RawMessage
	UpdatedAt time.Time
}

// Store persists saved values (internal/repository/settings).
type Store interface {
	// List returns every saved value.
	List(ctx context.Context) ([]Stored, error)
	// Get returns the saved value of key, or ErrNotFound if none is saved.
	Get(ctx context.Context, key string) (Stored, error)
	// Upsert saves the value of key.
	Upsert(ctx context.Context, s Stored) (Stored, error)
}

// Setting is the current state of one setting.
type Setting struct {
	Definition *Definition
	// Value is the saved value, or the default when IsDefault.
	Value     any
	IsDefault bool
	// UpdatedAt is set when a value is saved.
	UpdatedAt *time.Time
}

// SecretStatus reports whether a secret is set in the environment.
type SecretStatus struct {
	Definition *Definition
	IsSet      bool
}

// Change is a saved value, sent to the subscribers.
type Change struct {
	Key        string
	Value      any
	Apply      Apply
	AffectsAir bool
	At         time.Time
}

// dbTimeout bounds each statement of the service.
const dbTimeout = 5 * time.Second

// Service reads and saves settings.
//
// The registry is fixed at construction; subs is guarded by mu.
type Service struct {
	reg        *Registry
	store      Store
	secretsSet map[string]bool
	log        *slog.Logger
	now        func() time.Time

	mu   sync.RWMutex
	subs []func(Change)
}

// NewService returns the service. secretsSet tells, by environment variable
// name, which secrets are set; it must name every secret of the registry.
// log is the root logger; now is the clock.
func NewService(reg *Registry, store Store, secretsSet map[string]bool, log *slog.Logger, now func() time.Time) (*Service, error) {
	for _, d := range reg.Secrets() {
		if _, ok := secretsSet[d.EnvName]; !ok {
			return nil, fmt.Errorf("settings: secret %s (%s) is not reported by the configuration", d.Key, d.EnvName)
		}
	}
	return &Service{
		reg:        reg,
		store:      store,
		secretsSet: secretsSet,
		log:        logging.For(log, logging.ComponentAPI),
		now:        now,
	}, nil
}

// Subscribe registers fn for every saved change. fn runs in the goroutine
// that saved the value, after it is stored, so it must return quickly and
// must not call Set.
func (s *Service) Subscribe(fn func(Change)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs = append(s.subs, fn)
}

// List returns every setting that is not a secret, in registry order.
func (s *Service) List(ctx context.Context) ([]Setting, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	stored, err := s.store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list settings: %w", err)
	}
	saved := make(map[string]Stored, len(stored))
	for _, st := range stored {
		saved[st.Key] = st
	}

	defs := s.reg.Settings()
	out := make([]Setting, 0, len(defs))
	for _, d := range defs {
		st, ok := saved[d.Key]
		if !ok {
			out = append(out, defaultSetting(d))
			continue
		}
		out = append(out, s.fromStored(ctx, d, st))
	}
	return out, nil
}

// Get returns one setting.
func (s *Service) Get(ctx context.Context, key string) (Setting, error) {
	d, err := s.editable(key)
	if err != nil {
		return Setting{}, err
	}
	return s.current(ctx, d)
}

// Set validates and saves the value of key, logs the change, and notifies
// the subscribers.
func (s *Service) Set(ctx context.Context, key string, raw json.RawMessage) (Setting, error) {
	d, err := s.editable(key)
	if err != nil {
		return Setting{}, err
	}
	v, err := d.parse(raw)
	if err != nil {
		return Setting{}, err
	}
	canonical, err := json.Marshal(v)
	if err != nil {
		return Setting{}, fmt.Errorf("encode setting %s: %w", key, err)
	}

	at := s.now().UTC()
	sctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	st, err := s.store.Upsert(sctx, Stored{Key: key, Value: canonical, UpdatedAt: at})
	if err != nil {
		return Setting{}, fmt.Errorf("save setting %s: %w", key, err)
	}

	// Air-affecting changes are recorded with their UTC time (§6.3, §12);
	// the others are logged the same way, so the log tells the whole story.
	s.log.InfoContext(ctx, "setting changed",
		"key", key,
		"value", json.RawMessage(canonical),
		"apply", string(d.Apply),
		"affects_air", d.AffectsAir,
		"changed_at", at.Format(time.RFC3339Nano),
	)

	s.mu.RLock()
	subs := s.subs
	s.mu.RUnlock()
	change := Change{Key: key, Value: v, Apply: d.Apply, AffectsAir: d.AffectsAir, At: at}
	for _, fn := range subs {
		fn(change)
	}

	updated := st.UpdatedAt.UTC()
	return Setting{Definition: d, Value: v, UpdatedAt: &updated}, nil
}

// Secrets reports which secrets are set in the environment, in registry
// order. Their values are never read here.
func (s *Service) Secrets() []SecretStatus {
	defs := s.reg.Secrets()
	out := make([]SecretStatus, len(defs))
	for i, d := range defs {
		out[i] = SecretStatus{Definition: d, IsSet: s.secretsSet[d.EnvName]}
	}
	return out
}

// Value returns the current value of a typed key: the saved value, or its
// default.
func Value[T any](ctx context.Context, s *Service, k Key[T]) (T, error) {
	var zero T
	d, err := s.editable(k.name)
	if err != nil {
		return zero, err
	}
	cur, err := s.current(ctx, d)
	if err != nil {
		return zero, err
	}
	v, ok := cur.Value.(T)
	if !ok {
		return zero, fmt.Errorf("setting %s holds %T, not %T", k.name, cur.Value, zero)
	}
	return v, nil
}

// OnChange subscribes fn to the saved values of one typed key. The rules of
// Subscribe apply.
func OnChange[T any](s *Service, k Key[T], fn func(T)) {
	s.Subscribe(func(c Change) {
		if c.Key != k.name {
			return
		}
		if v, ok := c.Value.(T); ok {
			fn(v)
		}
	})
}

func (s *Service) editable(key string) (*Definition, error) {
	d, ok := s.reg.Lookup(key)
	if !ok {
		return nil, ErrNotFound
	}
	if d.Secret {
		return nil, ErrNotEditable
	}
	return d, nil
}

func (s *Service) current(ctx context.Context, d *Definition) (Setting, error) {
	gctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	st, err := s.store.Get(gctx, d.Key)
	if errors.Is(err, ErrNotFound) {
		return defaultSetting(d), nil
	}
	if err != nil {
		return Setting{}, fmt.Errorf("get setting %s: %w", d.Key, err)
	}
	return s.fromStored(ctx, d, st), nil
}

// fromStored decodes a saved value. A value saved under older rules that
// no longer passes is ignored: the default applies, with a warning, so a
// stricter release never runs on a value it refuses.
func (s *Service) fromStored(ctx context.Context, d *Definition, st Stored) Setting {
	v, err := d.parse(st.Value)
	if err != nil {
		s.log.WarnContext(ctx, "saved setting is invalid, using the default",
			"key", d.Key, logging.KeyError, err)
		return defaultSetting(d)
	}
	updated := st.UpdatedAt.UTC()
	return Setting{Definition: d, Value: v, UpdatedAt: &updated}
}

func defaultSetting(d *Definition) Setting {
	return Setting{Definition: d, Value: d.Default, IsDefault: true}
}
