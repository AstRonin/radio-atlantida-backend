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
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
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
	// RuleIntervalBeyondRetention: the play log retention would become
	// shorter than an interval of a play rule.
	RuleIntervalBeyondRetention Rule = "interval_beyond_retention"
	// RuleEncoderLimit: the stream codec, channels, sample rate, and
	// bitrate together make a format the encoder cannot start
	// (internal/streamformat).
	RuleEncoderLimit Rule = "encoder_limit"
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

// InvalidValuesError refuses a batch (SetMany): one refusal per refused
// key, known keys in registry order, then unknown keys in key order.
// Nothing of the batch was saved.
type InvalidValuesError struct {
	Errors []*InvalidValueError
}

func (e *InvalidValuesError) Error() string {
	msgs := make([]string, len(e.Errors))
	for i, ive := range e.Errors {
		msgs[i] = ive.Error()
	}
	return strings.Join(msgs, "; ")
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
	// UpsertMany saves the values in one statement, all or none, and
	// returns the stored rows.
	UpsertMany(ctx context.Context, s []Stored) ([]Stored, error)
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
// The registry is fixed at construction; subs, guards, and checks are
// guarded by mu. saveMu serializes the saves (Set, SetMany), so the checks
// of one save and its write see the same saved values within the process
// (one backend per database), and subscribers get the saves in order.
type Service struct {
	reg        *Registry
	store      Store
	secretsSet map[string]bool
	log        *slog.Logger
	now        func() time.Time

	saveMu sync.Mutex

	mu     sync.RWMutex
	subs   []func([]Change)
	guards map[string][]func(ctx context.Context, v any) error
	checks []check
}

// check is a cross-key rule (Check).
type check struct {
	keys []string
	fn   func(ctx context.Context, v View) error
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

// Subscribe registers fn for every save: one call per save, after the
// write, with every saved key in registry order (one element for Set). fn
// runs in the goroutine that saved the values, while the next save waits,
// so it must return quickly and must not call Set or SetMany.
func (s *Service) Subscribe(fn func([]Change)) {
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
// the subscribers. It is SetMany with one key, so the cross-key checks see
// the saved values with this one on top. An unknown key returns
// ErrNotFound, a secret ErrNotEditable, and a refused value an
// *InvalidValueError.
func (s *Service) Set(ctx context.Context, key string, raw json.RawMessage) (Setting, error) {
	if _, err := s.editable(key); err != nil {
		return Setting{}, err
	}
	saved, err := s.SetMany(ctx, map[string]json.RawMessage{key: raw})
	var refused *InvalidValuesError
	if errors.As(err, &refused) && len(refused.Errors) > 0 {
		return Setting{}, refused.Errors[0]
	}
	if err != nil {
		return Setting{}, err
	}
	return saved[0], nil
}

// SetMany validates a batch of values and saves it all or nothing (§6.3).
// Every value is checked by its own rules and its guards, and the cross-key
// checks (Check) run against the merged view: the saved values with the
// batch on top, so the result does not depend on the order of the keys.
// Any refusal returns an *InvalidValuesError with every refusal (an
// unknown key or a secret is one, RuleUnknownField) and saves nothing.
// Otherwise the batch is written in one statement, each change is logged,
// and the subscribers are notified once. The saved settings are returned in
// registry order.
func (s *Service) SetMany(ctx context.Context, values map[string]json.RawMessage) ([]Setting, error) {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	var refused []*InvalidValueError
	parsed := make(map[string]any, len(values))
	for key, raw := range values {
		d, err := s.editable(key)
		if err != nil {
			msg := "no setting has this key"
			if errors.Is(err, ErrNotEditable) {
				msg = "the setting is a secret: set it in the environment and restart the service"
			}
			refused = append(refused, &InvalidValueError{Key: key, Rule: RuleUnknownField, Message: msg})
			continue
		}
		v, err := d.parse(raw)
		var ive *InvalidValueError
		if errors.As(err, &ive) {
			refused = append(refused, ive)
			continue
		}
		if err != nil {
			return nil, err
		}
		parsed[key] = v
	}

	for key, v := range parsed {
		if err := s.guard(ctx, key, v); err != nil {
			var ive *InvalidValueError
			if !errors.As(err, &ive) {
				return nil, err
			}
			refused = append(refused, ive)
		}
	}

	failed := make(map[string]bool, len(refused))
	for _, ive := range refused {
		failed[ive.Key] = true
	}
	more, err := s.check(ctx, parsed, failed)
	if err != nil {
		return nil, err
	}
	refused = append(refused, more...)
	if len(refused) > 0 {
		return nil, &InvalidValuesError{Errors: s.sortRefusals(refused)}
	}
	if len(parsed) == 0 {
		return []Setting{}, nil
	}
	return s.write(ctx, parsed)
}

// write saves the parsed batch, logs each change, and notifies the
// subscribers once. The caller holds saveMu.
func (s *Service) write(ctx context.Context, parsed map[string]any) ([]Setting, error) {
	at := s.now().UTC()
	defs := make([]*Definition, 0, len(parsed))
	rows := make([]Stored, 0, len(parsed))
	for _, d := range s.reg.Settings() {
		v, ok := parsed[d.Key]
		if !ok {
			continue
		}
		canonical, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("encode setting %s: %w", d.Key, err)
		}
		defs = append(defs, d)
		rows = append(rows, Stored{Key: d.Key, Value: canonical, UpdatedAt: at})
	}

	sctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	stored, err := s.store.UpsertMany(sctx, rows)
	if err != nil {
		keys := make([]string, len(rows))
		for i, r := range rows {
			keys[i] = r.Key
		}
		return nil, fmt.Errorf("save setting %s: %w", strings.Join(keys, ", "), err)
	}
	updatedAt := make(map[string]time.Time, len(stored))
	for _, st := range stored {
		updatedAt[st.Key] = st.UpdatedAt.UTC()
	}

	out := make([]Setting, len(defs))
	changes := make([]Change, len(defs))
	for i, d := range defs {
		v := parsed[d.Key]
		// Air-affecting changes are recorded with their UTC time (§6.3,
		// §12); the others are logged the same way, so the log tells the
		// whole story.
		s.log.InfoContext(ctx, "setting changed",
			"key", d.Key,
			"value", rows[i].Value,
			"apply", string(d.Apply),
			"affects_air", d.AffectsAir,
			"changed_at", at.Format(time.RFC3339Nano),
		)
		updated, ok := updatedAt[d.Key]
		if !ok {
			updated = at
		}
		out[i] = Setting{Definition: d, Value: v, UpdatedAt: &updated}
		changes[i] = Change{Key: d.Key, Value: v, Apply: d.Apply, AffectsAir: d.AffectsAir, At: at}
	}

	s.mu.RLock()
	subs := s.subs
	s.mu.RUnlock()
	for _, fn := range subs {
		fn(changes)
	}
	return out, nil
}

// sortRefusals orders refusals by the registry, then unknown keys by key.
func (s *Service) sortRefusals(refused []*InvalidValueError) []*InvalidValueError {
	pos := map[string]int{}
	for i, d := range s.reg.Settings() {
		pos[d.Key] = i
	}
	slices.SortStableFunc(refused, func(a, b *InvalidValueError) int {
		pa, okA := pos[a.Key]
		pb, okB := pos[b.Key]
		switch {
		case okA && okB:
			return cmp.Compare(pa, pb)
		case okA:
			return -1
		case okB:
			return 1
		default:
			return strings.Compare(a.Key, b.Key)
		}
	})
	return refused
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

// OnChange subscribes fn to the saved values of one typed key: it runs
// once for a save that carries the key. The rules of Subscribe apply.
func OnChange[T any](s *Service, k Key[T], fn func(T)) {
	s.Subscribe(func(changes []Change) {
		for _, c := range changes {
			if c.Key != k.name {
				continue
			}
			if v, ok := c.Value.(T); ok {
				fn(v)
			}
		}
	})
}

// Guard registers fn to check a new value of one typed key against data
// outside the settings (the play log retention against the play rules)
// before it is saved. A refusal is returned as it is; an *InvalidValueError
// names the rule. fn runs in the goroutine that saves the value.
func Guard[T any](s *Service, k Key[T], fn func(ctx context.Context, v T) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.guards == nil {
		s.guards = map[string][]func(context.Context, any) error{}
	}
	s.guards[k.name] = append(s.guards[k.name], func(ctx context.Context, v any) error {
		tv, ok := v.(T)
		if !ok {
			return fmt.Errorf("setting %s holds %T", k.name, v)
		}
		return fn(ctx, tv)
	})
}

// Refuse returns the refusal of a value of key, for a guard or a check.
// A check that refuses several keys joins the refusals with errors.Join.
func Refuse(key string, rule Rule, msg string) error { return invalid(key, rule, msg) }

// View is the merged state a cross-key check sees: the saved values (or
// the defaults) with the values of the batch on top.
type View struct {
	values  map[string]any
	changed map[string]bool
}

// Changed reports whether the batch carries key.
func (v View) Changed(key string) bool { return v.changed[key] }

// ViewValue returns the value of a typed key in the view: the batch value,
// else the saved one, else the default.
func ViewValue[T any](v View, k Key[T]) (T, error) {
	var zero T
	raw, ok := v.values[k.name]
	if !ok {
		return zero, fmt.Errorf("setting %s is not in the view", k.name)
	}
	tv, ok := raw.(T)
	if !ok {
		return zero, fmt.Errorf("setting %s holds %T, not %T", k.name, raw, zero)
	}
	return tv, nil
}

// Check registers a cross-key rule: fn runs once per save whose batch
// carries any of keys, against the merged view (View), so a combination is
// judged as it will be saved, whatever the order of the fields. It is
// skipped when one of keys was already refused by its own rules or a
// guard. fn refuses with Refuse, once per changed key it blames (several
// joined with errors.Join); any other error fails the save. Guard stays for
// checks against data outside the settings.
func Check(s *Service, keys []string, fn func(ctx context.Context, v View) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks = append(s.checks, check{keys: slices.Clone(keys), fn: fn})
}

// check runs the cross-key checks that the batch touches. failed holds the
// keys already refused. The caller holds saveMu.
func (s *Service) check(ctx context.Context, parsed map[string]any, failed map[string]bool) ([]*InvalidValueError, error) {
	s.mu.RLock()
	checks := s.checks
	s.mu.RUnlock()
	var run []check
	for _, c := range checks {
		touched, blocked := false, false
		for _, k := range c.keys {
			if _, ok := parsed[k]; ok {
				touched = true
			}
			if failed[k] {
				blocked = true
			}
		}
		if touched && !blocked {
			run = append(run, c)
		}
	}
	if len(run) == 0 {
		return nil, nil
	}

	view, err := s.view(ctx, parsed)
	if err != nil {
		return nil, err
	}
	var refused []*InvalidValueError
	for _, c := range run {
		cctx, cancel := context.WithTimeout(ctx, dbTimeout)
		err := c.fn(cctx, view)
		cancel()
		if err == nil {
			continue
		}
		errs := []error{err}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			errs = joined.Unwrap()
		}
		for _, e := range errs {
			var ive *InvalidValueError
			if !errors.As(e, &ive) {
				return nil, err
			}
			refused = append(refused, ive)
		}
	}
	return refused, nil
}

// view reads the saved values once and puts the batch on top.
func (s *Service) view(ctx context.Context, parsed map[string]any) (View, error) {
	lctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	stored, err := s.store.List(lctx)
	if err != nil {
		return View{}, fmt.Errorf("list settings: %w", err)
	}
	saved := make(map[string]Stored, len(stored))
	for _, st := range stored {
		saved[st.Key] = st
	}
	v := View{values: map[string]any{}, changed: map[string]bool{}}
	for _, d := range s.reg.Settings() {
		if st, ok := saved[d.Key]; ok {
			v.values[d.Key] = s.fromStored(ctx, d, st).Value
		} else {
			v.values[d.Key] = d.Default
		}
	}
	for k, val := range parsed {
		v.values[k] = val
		v.changed[k] = true
	}
	return v, nil
}

func (s *Service) guard(ctx context.Context, key string, v any) error {
	s.mu.RLock()
	guards := s.guards[key]
	s.mu.RUnlock()
	gctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	for _, fn := range guards {
		if err := fn(gctx, v); err != nil {
			return err
		}
	}
	return nil
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
