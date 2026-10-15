// Package airwindow is the air as the manager needs it now
// (docs/phase-17-plan.md): the last items that were due (aired, truncated,
// missed), the item on air, and the next planned items, in air order. It
// only reads: the play log and the track plan from the store, the item on
// air and the control link from playout.
//
// Persistence goes through internal/repository/airwindow.
package airwindow

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/liquidsoap"
	"github.com/AstRonin/radio-atlantida-backend/internal/playout"
)

// Kind is the block a row belongs to.
type Kind string

// Kinds, in air order.
const (
	KindPlayed Kind = "played"
	KindNow    Kind = "now"
	KindQueued Kind = "queued"
)

// Status of a row.
const (
	// Played rows: the play log status.
	StatusAired     = "aired"
	StatusTruncated = "truncated"
	StatusMissed    = "missed"
	// StatusNow is the row on air.
	StatusNow = "now"
	// Queued rows: pushed to Liquidsoap, or planned only.
	StatusQueued  = "queued"
	StatusPlanned = "planned"
)

// Source of a row's track.
const (
	SourcePlan      = "plan"
	SourceEmergency = "emergency"
	SourceLive      = "live"
)

// Played is a play log row: an aired, truncated, or missed item.
type Played struct {
	// ID is the play_log row.
	ID uint64
	// ScheduledItemID is the plan item it was; nil for an emergency track.
	ScheduledItemID *uint64
	Status          string
	// Source is the play log source (auto, live, emergency).
	Source     string
	TrackID    uint64
	Title      string
	ArtistName *string
	// PlannedStart is nil for an emergency track; StartedAt for a missed
	// item.
	PlannedStart *time.Time
	StartedAt    *time.Time
	PlannedOutMS *int
	// ActualDurationMS is nil for a missed item.
	ActualDurationMS *int
	// Resolution and OutageID: missed items only.
	Resolution *string
	OutageID   *uint64
}

// Queued is a plan item that has not started: planned, or pushed to
// Liquidsoap.
type Queued struct {
	ID         uint64
	TrackID    uint64
	Title      string
	ArtistName *string
	Start      time.Time
	InMS       int
	OutMS      int
	Truncated  bool
	// InLiquidsoap: status queued (pushed), not planned.
	InLiquidsoap bool
}

// Row is one line of the window.
type Row struct {
	Kind   Kind
	Status string
	// ItemID is the play_log row (played) or the plan item (now, queued);
	// nil for a now row without a plan item (an emergency track).
	ItemID     *uint64
	TrackID    uint64
	Title      string
	ArtistName *string
	Source     string
	Start      time.Time
	// End is nil only for a row whose length is unknown: an emergency
	// track without a duration.
	End          *time.Time
	Truncated    bool
	Resolution   *string
	OutageID     *uint64
	InLiquidsoap bool
}

// Window is the list for the panel.
type Window struct {
	GeneratedAt time.Time
	// ControlLinkConnected: false means the item on air is unknown, not
	// that nothing plays.
	ControlLinkConnected bool
	Rows                 []Row
}

// Settings are the counts of the window (group air).
type Settings struct {
	PlayedCount int
	QueuedCount int
}

// SettingsFunc reads the settings on every request.
type SettingsFunc func(ctx context.Context) (Settings, error)

// Store reads the play log and the plan (internal/repository/airwindow).
type Store interface {
	// Played returns the last limit play log rows by the moment they were
	// due (started, or planned for a missed item), newest first.
	Played(ctx context.Context, limit int) ([]Played, error)
	// Queued returns the planned and queued items that start at or after
	// from, in planned order, at most limit.
	Queued(ctx context.Context, from time.Time, limit int) ([]Queued, error)
}

// Playout is the playout state (*playout.Service).
type Playout interface {
	NowPlaying() *playout.NowPlaying
	Link() liquidsoap.State
}

// dbTimeout bounds the reads of one window.
const dbTimeout = 5 * time.Second

// Service builds the window.
type Service struct {
	store    Store
	playout  Playout
	settings SettingsFunc
	now      func() time.Time
}

// NewService returns the service.
func NewService(store Store, p Playout, settings SettingsFunc, now func() time.Time) *Service {
	return &Service{store: store, playout: p, settings: settings, now: now}
}

// Window returns the air window. Playout being down is not an error: the
// window then has no now row and the rows from the database.
func (s *Service) Window(ctx context.Context) (Window, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	set, err := s.settings(ctx)
	if err != nil {
		return Window{}, fmt.Errorf("read air window settings: %w", err)
	}
	t := s.now().UTC()
	connected := s.playout.Link().Connected
	var now *playout.NowPlaying
	if connected {
		now = s.playout.NowPlaying()
	}
	// One more than shown: the item on air is in the play log already
	// (playout moves it there when it starts), and Assemble drops it.
	played, err := s.store.Played(ctx, set.PlayedCount+1)
	if err != nil {
		return Window{}, fmt.Errorf("read played items: %w", err)
	}
	var queued []Queued
	if set.QueuedCount > 0 {
		queued, err = s.store.Queued(ctx, QueuedFrom(now, t), set.QueuedCount)
		if err != nil {
			return Window{}, fmt.Errorf("read queued items: %w", err)
		}
	}
	return Window{
		GeneratedAt: t, ControlLinkConnected: connected,
		Rows: Assemble(played, now, queued, set.PlayedCount),
	}, nil
}

// QueuedFrom is where the queued block starts: the start of the item on
// air, or t when nothing is known to be on air. Items whose start has
// passed and that did not air belong to the outage code (decision 6).
func QueuedFrom(now *playout.NowPlaying, t time.Time) time.Time {
	if now != nil {
		return now.StartedAt.UTC()
	}
	return t
}

// Assemble builds the rows in air order: the played rows oldest first
// (played comes newest first, as the store returns it), the now row, then
// the queued rows in planned order. The play log row of the item on air
// is not a played row. At most playedCount played rows are kept.
func Assemble(played []Played, now *playout.NowPlaying, queued []Queued, playedCount int) []Row {
	rows := make([]Row, 0, min(len(played), playedCount)+1+len(queued))
	for _, p := range played {
		if len(rows) == playedCount {
			break
		}
		if now != nil && now.ItemID != nil && p.ScheduledItemID != nil && *p.ScheduledItemID == *now.ItemID {
			continue
		}
		rows = append(rows, playedRow(p))
	}
	slices.Reverse(rows)
	if now != nil {
		rows = append(rows, nowRow(*now))
	}
	for _, q := range queued {
		rows = append(rows, queuedRow(q))
	}
	return rows
}

func playedRow(p Played) Row {
	r := Row{
		Kind: KindPlayed, Status: p.Status, ItemID: new(p.ID), TrackID: p.TrackID, Title: p.Title,
		ArtistName: p.ArtistName, Source: playedSource(p.Source), Truncated: p.Status == StatusTruncated,
	}
	switch {
	case p.StartedAt != nil:
		// Aired or truncated: the cut that aired.
		r.Start = p.StartedAt.UTC()
		if p.ActualDurationMS != nil {
			r.End = new(r.Start.Add(ms(*p.ActualDurationMS)))
		}
	case p.PlannedStart != nil:
		// Missed: the planned length.
		r.Start = p.PlannedStart.UTC()
		if p.PlannedOutMS != nil {
			r.End = new(r.Start.Add(ms(*p.PlannedOutMS)))
		}
	}
	if p.Status == StatusMissed {
		r.Resolution, r.OutageID = p.Resolution, p.OutageID
	}
	return r
}

// playedSource maps the play log source to the window's: a planned item
// is auto in the play log.
func playedSource(s string) string {
	switch s {
	case SourceEmergency, SourceLive:
		return s
	default:
		return SourcePlan
	}
}

func nowRow(np playout.NowPlaying) Row {
	r := Row{
		Kind: KindNow, Status: StatusNow, ItemID: np.ItemID, TrackID: np.TrackID, Title: np.Title,
		ArtistName: np.ArtistName, Source: np.Source, Start: np.StartedAt.UTC(), Truncated: np.Truncated,
		InLiquidsoap: true,
	}
	if np.OutMS != nil {
		r.End = new(r.Start.Add(ms(*np.OutMS)))
	}
	return r
}

func queuedRow(q Queued) Row {
	r := Row{
		Kind: KindQueued, Status: StatusPlanned, ItemID: new(q.ID), TrackID: q.TrackID, Title: q.Title,
		ArtistName: q.ArtistName, Source: SourcePlan, Start: q.Start.UTC(), Truncated: q.Truncated,
		InLiquidsoap: q.InLiquidsoap,
	}
	if q.InLiquidsoap {
		r.Status = StatusQueued
	}
	r.End = new(r.Start.Add(ms(q.OutMS - q.InMS)))
	return r
}

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }
