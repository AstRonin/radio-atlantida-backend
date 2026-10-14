// Package playout puts the track plan on air (§8): it keeps Liquidsoap's
// hour queues a few items ahead, turns Liquidsoap's track callbacks into
// play log rows, keeps what is playing, sets the music gain, and picks the
// emergency tracks. Decisions: docs/phase-9-plan.md §2.
package playout

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/liquidsoap"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
)

// ErrUnavailable is returned when the Liquidsoap control link is down.
var ErrUnavailable = liquidsoap.ErrUnavailable

// Item is a plan item that waits to be pushed or waits in a queue.
type Item struct {
	ID       uint64
	TrackID  uint64
	FilePath string
	Start    time.Time
	InMS     int
	// OutMS is the out point, relative to the start of the track.
	OutMS     int
	Truncated bool
	// Queued: pushed to Liquidsoap and not started yet.
	Queued bool
}

// end is the planned end of the item.
func (i Item) end() time.Time {
	return i.Start.Add(time.Duration(i.OutMS-i.InMS) * time.Millisecond)
}

// Track is a library track, for the emergency source and now playing.
type Track struct {
	ID         uint64
	Title      string
	ArtistName *string
	FilePath   string
	DurationMS *int
}

// Source of what plays.
const (
	SourcePlan      = "plan"
	SourceEmergency = "emergency"
)

// NowPlaying is the track on air.
type NowPlaying struct {
	// ItemID is the plan item; nil for an emergency track.
	ItemID     *uint64
	TrackID    uint64
	Title      string
	ArtistName *string
	Source     string
	StartedAt  time.Time
	// OutMS is the planned out point (plan items) or the duration
	// (emergency tracks, when known).
	OutMS     *int
	Truncated bool
}

// QueuedItem is an item in a Liquidsoap queue, for the status.
type QueuedItem struct {
	ID         uint64
	TrackID    uint64
	Title      string
	ArtistName *string
	Start      time.Time
	OutMS      int
	Truncated  bool
	// Queue is the Liquidsoap queue that holds it.
	Queue string
}

// Output is the Icecast output state reported by Liquidsoap.
type Output struct {
	Connected bool
	Since     time.Time
}

// Status is the playout state for the panel.
type Status struct {
	Link       liquidsoap.State
	Output     *Output
	NowPlaying *NowPlaying
	// LastCallbackAt is when the last callback arrived (the heartbeat of
	// §7.1).
	LastCallbackAt *time.Time
	// MusicGain is the gain last set; nil before the first connect.
	MusicGain *float64
	Queued    []QueuedItem
}

// Store persists playout (internal/repository/playout).
type Store interface {
	// WithTx returns the store bound to tx.
	WithTx(tx db.Tx) Store
	// LockPlan takes the transaction-level advisory lock of plan writes,
	// the one the planner holds while it builds an hour.
	LockPlan(ctx context.Context) error
	// Items returns the planned and queued items that start in [from, to),
	// in planned order.
	Items(ctx context.Context, from, to time.Time) ([]Item, error)
	// MarkQueued sets a planned item queued; false when it is not planned
	// any more (rebuilt, started).
	MarkQueued(ctx context.Context, id uint64) (bool, error)
	// MarkPlanned sets a queued item planned again.
	MarkPlanned(ctx context.Context, id uint64) error
	// Unqueue sets the queued items that start in [from, to) planned
	// again and returns how many.
	Unqueue(ctx context.Context, from, to time.Time) (int64, error)
	// Start moves a planned or queued item to the play log, started at; false
	// when the item is not in the plan (moved already, or unknown).
	Start(ctx context.Context, itemID uint64, at time.Time) (bool, error)
	// End records the end of a logged plan item whose end is not recorded
	// yet: the actual duration, and truncated when it played less than its
	// planned length minus cutMarginMS. False when there is no such row.
	End(ctx context.Context, itemID uint64, elapsedMS, cutMarginMS int) (bool, error)
	// LogEmergency logs an emergency track that played; a track already
	// logged with the same start is not logged again.
	LogEmergency(ctx context.Context, trackID uint64, startedAt time.Time, elapsedMS int) error
	// Logged returns the logged plan item as now playing; false if it is
	// not logged.
	Logged(ctx context.Context, itemID uint64) (NowPlaying, bool, error)
	// Track returns a track; false if there is none.
	Track(ctx context.Context, id uint64) (Track, bool, error)
	// PlaylistTracks returns the tracks with a duration of a playlist,
	// ordered by ID.
	PlaylistTracks(ctx context.Context, playlistID uint64) ([]Track, error)
	// Queued returns the queued items, in planned order, at most limit.
	Queued(ctx context.Context, limit int) ([]QueuedItem, error)
}

// Control is the Liquidsoap control client (*liquidsoap.Client).
type Control interface {
	Push(ctx context.Context, queue, uri string) (int, error)
	Queue(ctx context.Context, queue string) ([]int, error)
	SetVar(ctx context.Context, name, value string) error
	Ping(ctx context.Context) error
	Generation() uint64
	State() liquidsoap.State
}

// Publisher sends an event to the panels without blocking.
type Publisher interface {
	Publish(event any)
}

// TxRunner runs fn in one transaction (*db.DB).
type TxRunner interface {
	WithTx(ctx context.Context, fn func(ctx context.Context, tx db.Tx) error) error
}

// Settings are the settings playout reads on every pass.
type Settings struct {
	Location *time.Location
	// Lookahead is how many items wait in the queue of an hour on air.
	Lookahead int
	// Prepush is how long before its start an hour's items are pushed.
	Prepush   time.Duration
	FadeOutMS int
	// MediaRoot is the media directory as Liquidsoap sees it.
	MediaRoot string
	// MusicGain is the default music gain, set on the first connect.
	MusicGain           float64
	EmergencyPlaylistID *uint64
}

// SettingsFunc reads the settings.
type SettingsFunc func(ctx context.Context) (Settings, error)

const (
	// dbTimeout bounds each database call of a pass or a callback.
	dbTimeout = 5 * time.Second
	// cutMarginMS: an item that played this much less than its planned
	// length was cut (by the hour switch or a skip), so it is truncated.
	cutMarginMS = 1000
	// queuedListLimit bounds the queued items of the status; there are at
	// most a few per hour queue.
	queuedListLimit = 20
)

// Service is playout. The pusher state (pusher.go) belongs to the Run
// goroutine; mu guards the rest, which callbacks and requests share.
type Service struct {
	tx       TxRunner
	store    Store
	ctl      Control
	settings SettingsFunc
	pub      Publisher
	log      *slog.Logger // component liquidsoap
	now      func() time.Time
	// wake asks the pusher for a pass now (a slot was freed, the gain
	// changed); buffered, so a send never blocks.
	wake chan struct{}

	// Owned by the Run goroutine.
	push pusherState

	mu            sync.Mutex
	rnd           *rand.Rand
	nowPlaying    *NowPlaying
	lastCallback  *time.Time
	output        *Output
	gain          *float64
	gainPending   bool
	lastEmergency uint64
}

// NewService returns the service. log is the root logger; rnd picks the
// emergency tracks.
func NewService(tx TxRunner, store Store, ctl Control, settings SettingsFunc, pub Publisher,
	log *slog.Logger, now func() time.Time, rnd *rand.Rand,
) *Service {
	return &Service{
		tx: tx, store: store, ctl: ctl, settings: settings, pub: pub,
		log: logging.For(log, logging.ComponentLiquidsoap), now: now, rnd: rnd,
		wake: make(chan struct{}, 1),
	}
}

// Wake asks the pusher for a pass now.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default: // a pass is due already
	}
}

// Status returns the playout state.
func (s *Service) Status(ctx context.Context) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	queued, err := s.store.Queued(ctx, queuedListLimit)
	if err != nil {
		return Status{}, fmt.Errorf("read queued items: %w", err)
	}
	set, err := s.settings(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("read playout settings: %w", err)
	}
	for i := range queued {
		queued[i].Queue = QueueName(hourStart(queued[i].Start, set.Location), set.Location)
	}
	st := Status{Link: s.ctl.State(), Queued: queued}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.output != nil {
		st.Output = new(*s.output)
	}
	if s.nowPlaying != nil {
		st.NowPlaying = new(*s.nowPlaying)
	}
	if s.lastCallback != nil {
		st.LastCallbackAt = new(*s.lastCallback)
	}
	if s.gain != nil {
		st.MusicGain = new(*s.gain)
	}
	return st, nil
}

// Link returns the state of the Liquidsoap control link.
func (s *Service) Link() liquidsoap.State { return s.ctl.State() }

// SetMusicGain sets the live music gain (§8.5). It is kept in memory and
// set again after a reconnect; the setting liquidsoap.music_gain is only
// the value at startup.
func (s *Service) SetMusicGain(ctx context.Context, gain float64) error {
	if err := s.ctl.SetVar(ctx, varMusicGain, liquidsoap.FormatFloat(gain)); err != nil {
		return fmt.Errorf("set music gain: %w", err)
	}
	s.mu.Lock()
	s.gain, s.gainPending = new(gain), false
	s.mu.Unlock()
	s.log.Info("music gain set", "gain", gain)
	return nil
}

// MusicGainSettingChanged applies a new value of the setting
// liquidsoap.music_gain at the next pass (§6.3: it takes effect at once).
func (s *Service) MusicGainSettingChanged(gain float64) {
	s.mu.Lock()
	s.gain, s.gainPending = new(gain), true
	s.mu.Unlock()
	s.Wake()
}
