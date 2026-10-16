package playout

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/liquidsoap"
	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

// EventKind is the kind of a Liquidsoap callback.
type EventKind string

// Callback kinds (dev/liquidsoap/radio.liq).
const (
	EventStart  EventKind = "start"
	EventEnd    EventKind = "end"
	EventOutput EventKind = "output"
	// EventHeartbeat is sent every few seconds while Liquidsoap runs, so the
	// outage detector can tell a stalled playout from a long track (§7.1).
	// It is not written to the journal.
	EventHeartbeat EventKind = "heartbeat"
)

// Event is one callback from Liquidsoap: a line of its playout journal
// (§8.3). Journal lines are replayed through Replay.
type Event struct {
	Kind EventKind
	// Time is when Liquidsoap wrote the event (UTC).
	Time time.Time
	// Source is SourcePlan or SourceEmergency (start and end).
	Source string
	// ItemID is the plan item (source plan).
	ItemID *uint64
	// TrackID is the emergency track (source emergency).
	TrackID *uint64
	// ElapsedMS is how long the track played (end).
	ElapsedMS int
	// Connected is the Icecast output state (output).
	Connected bool
}

// Callback records an event from Liquidsoap. Every event is idempotent: a
// repeated start changes nothing, and the first end of an item wins (a
// track cut by the hour switch reports its end twice).
func (s *Service) Callback(ctx context.Context, e Event) error {
	at := s.now().UTC()
	s.mu.Lock()
	s.lastCallback = &at
	s.mu.Unlock()

	switch {
	case e.Kind == EventHeartbeat:
		return nil
	case e.Kind == EventOutput:
		s.setOutput(ctx, e)
		return nil
	case e.Source == SourcePlan && e.ItemID != nil && e.Kind == EventStart:
		defer s.Wake() // a queue slot was freed
		return s.started(ctx, *e.ItemID, e.Time, true)
	case e.Source == SourcePlan && e.ItemID != nil && e.Kind == EventEnd:
		return s.ended(ctx, *e.ItemID, e, true)
	case e.Source == SourceEmergency && e.TrackID != nil && e.Kind == EventStart:
		return s.emergencyStarted(ctx, *e.TrackID, e.Time)
	case e.Source == SourceEmergency && e.TrackID != nil && e.Kind == EventEnd:
		return s.emergencyEnded(ctx, *e.TrackID, e, true)
	default:
		// A request pushed by hand over telnet, or an older script.
		s.log.Warn("liquidsoap callback without an item or a track ignored",
			"event", string(e.Kind), "source", e.Source)
		return nil
	}
}

// Replay records a journal line that may have missed its callback (§7.1):
// the same play log changes as Callback, without what only a live event
// does (now playing, the last callback time, the output state). It is
// idempotent like Callback, so a window may be replayed any number of
// times. Lines other than track starts and ends are ignored.
func (s *Service) Replay(ctx context.Context, e Event) error {
	switch {
	case e.Source == SourcePlan && e.ItemID != nil && e.Kind == EventStart:
		return s.started(ctx, *e.ItemID, e.Time, false)
	case e.Source == SourcePlan && e.ItemID != nil && e.Kind == EventEnd:
		return s.ended(ctx, *e.ItemID, e, false)
	case e.Source == SourceEmergency && e.TrackID != nil && e.Kind == EventEnd:
		return s.emergencyEnded(ctx, *e.TrackID, e, false)
	}
	return nil
}

// LastCallback returns when the last callback arrived; nil before the
// first.
func (s *Service) LastCallback() *time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastCallback == nil {
		return nil
	}
	return new(*s.lastCallback)
}

// started moves the item to the play log and, for a live event, makes it
// now playing.
func (s *Service) started(ctx context.Context, itemID uint64, at time.Time, live bool) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	moved, err := s.store.Start(ctx, itemID, at)
	if err != nil {
		return fmt.Errorf("log start of item %d: %w", itemID, err)
	}
	np, logged, err := s.store.Logged(ctx, itemID)
	if err != nil {
		return fmt.Errorf("read logged item %d: %w", itemID, err)
	}
	if !logged {
		s.log.Warn("liquidsoap started an item that is not in the plan", "item_id", itemID)
		return nil
	}
	if moved && live {
		s.setNowPlaying(&np)
	}
	if moved && !live {
		s.log.Info("start of an item recorded from the journal", "item_id", itemID, "started_at", at)
	}
	return nil
}

// ended records the actual duration. An end whose start never arrived
// (lost callback) logs the item first, started elapsed before the end.
func (s *Service) ended(ctx context.Context, itemID uint64, e Event, live bool) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	var recorded bool
	err := s.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		st := s.store.WithTx(tx)
		ok, err := st.End(ctx, itemID, e.ElapsedMS, cutMarginMS)
		if err != nil || ok {
			recorded = ok
			return err
		}
		started := e.Time.Add(-time.Duration(e.ElapsedMS) * time.Millisecond)
		moved, err := st.Start(ctx, itemID, started)
		if err != nil || !moved {
			return err // already ended, or not a plan item
		}
		s.log.Warn("liquidsoap start callback of an item was lost; logged from its end", "item_id", itemID)
		recorded, err = st.End(ctx, itemID, e.ElapsedMS, cutMarginMS)
		return err
	})
	if err != nil {
		return fmt.Errorf("log end of item %d: %w", itemID, err)
	}
	if !live {
		return nil
	}
	s.mu.Lock()
	if s.nowPlaying != nil && s.nowPlaying.ItemID != nil && *s.nowPlaying.ItemID == itemID {
		s.nowPlaying = nil
	}
	s.mu.Unlock()
	if !recorded {
		return nil // a repeated end: the first one was reported
	}
	// The status is the play log's after the end: truncated when it was cut.
	np, logged, err := s.store.Logged(ctx, itemID)
	if err != nil {
		return fmt.Errorf("read ended item %d: %w", itemID, err)
	}
	if logged {
		s.trackEnded(ws.TrackEndedData{
			ItemID: new(itemID), TrackID: np.TrackID, Source: SourcePlan, Status: endStatus(np.Truncated),
			StartedAt: np.StartedAt.UTC(), EndedAt: e.Time.UTC(), ActualDurationMS: e.ElapsedMS,
		})
	}
	return nil
}

// endStatus is the play log status of an ended track.
func endStatus(truncated bool) string {
	if truncated {
		return "truncated"
	}
	return "aired"
}

func (s *Service) trackEnded(data ws.TrackEndedData) {
	s.pub.Publish(ws.TrackEndedEvent{Type: ws.EventTrackEnded, Time: s.now().UTC(), Data: data})
}

func (s *Service) emergencyStarted(ctx context.Context, trackID uint64, at time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	t, ok, err := s.store.Track(ctx, trackID)
	if err != nil {
		return fmt.Errorf("read emergency track %d: %w", trackID, err)
	}
	if !ok {
		s.log.Warn("liquidsoap started an unknown emergency track", "track_id", trackID)
		return nil
	}
	s.log.Warn("emergency track on air: the plan has nothing to play", "track_id", trackID, "title", t.Title)
	s.setNowPlaying(&NowPlaying{
		TrackID: t.ID, Title: t.Title, ArtistName: t.ArtistName, Source: SourceEmergency, StartedAt: at, OutMS: t.DurationMS,
	})
	return nil
}

// emergencyEnded logs the emergency track (§8.6): only on its end, with the
// time it actually played. A repeated end (journal replay) is a no-op.
func (s *Service) emergencyEnded(ctx context.Context, trackID uint64, e Event, live bool) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	started := e.Time.Add(-time.Duration(e.ElapsedMS) * time.Millisecond)
	if err := s.store.LogEmergency(ctx, trackID, started, e.ElapsedMS); err != nil {
		return fmt.Errorf("log emergency track %d: %w", trackID, err)
	}
	if !live {
		return nil
	}
	s.mu.Lock()
	if s.nowPlaying != nil && s.nowPlaying.ItemID == nil && s.nowPlaying.TrackID == trackID {
		s.nowPlaying = nil
	}
	s.mu.Unlock()
	// Emergency tracks are logged aired, with the time they played.
	s.trackEnded(ws.TrackEndedData{
		TrackID: trackID, Source: SourceEmergency, Status: endStatus(false),
		StartedAt: started.UTC(), EndedAt: e.Time.UTC(), ActualDurationMS: e.ElapsedMS,
	})
	return nil
}

func (s *Service) setNowPlaying(np *NowPlaying) {
	s.mu.Lock()
	s.nowPlaying = np
	s.mu.Unlock()
	s.pub.Publish(ws.NowPlayingEvent{Type: ws.EventNowPlaying, Time: s.now().UTC(), Data: nowPlayingData(*np)})
}

func nowPlayingData(np NowPlaying) ws.NowPlayingData {
	return ws.NowPlayingData{
		ItemID: np.ItemID, TrackID: np.TrackID, Title: np.Title, ArtistName: np.ArtistName, Source: np.Source,
		StartedAt: np.StartedAt.UTC(), OutMS: np.OutMS, Truncated: np.Truncated,
	}
}

// setOutput records the Icecast output state and sends icecast.state when
// it changes. Every report goes on to the broadcast state, which may have
// forgotten an unchanged one (a silent heartbeat).
func (s *Service) setOutput(ctx context.Context, e Event) {
	defer s.outputs.OnOutput(ctx, e.Connected, e.Time)
	s.mu.Lock()
	changed := s.output == nil || s.output.Connected != e.Connected
	if changed {
		s.output = &Output{Connected: e.Connected, Since: e.Time.UTC()}
	}
	s.mu.Unlock()
	if !changed {
		return
	}
	state := ws.IcecastConnected
	if !e.Connected {
		state = ws.IcecastDisconnected
		s.log.Warn("liquidsoap lost its icecast output")
	} else {
		s.log.Info("liquidsoap connected its icecast output")
	}
	s.pub.Publish(ws.IcecastStateEvent{Type: ws.EventIcecastState, Time: e.Time.UTC(), Data: ws.IcecastStateData{State: state}})
}

// EmergencyNext returns the request of the next emergency track (§8.2,
// §8.6): a random track of the emergency playlist, not the one before.
// False when there is no emergency playlist or it has no playable track.
func (s *Service) EmergencyNext(ctx context.Context) (string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	set, err := s.settings(ctx)
	if err != nil {
		return "", false, fmt.Errorf("read playout settings: %w", err)
	}
	if set.EmergencyPlaylistID == nil {
		return "", false, nil
	}
	tracks, err := s.store.PlaylistTracks(ctx, *set.EmergencyPlaylistID)
	if err != nil {
		return "", false, fmt.Errorf("read emergency playlist %d: %w", *set.EmergencyPlaylistID, err)
	}
	s.mu.Lock()
	if len(tracks) > 1 {
		for i, t := range tracks {
			if t.ID == s.lastEmergency {
				tracks = append(tracks[:i:i], tracks[i+1:]...)
				break
			}
		}
	}
	if len(tracks) == 0 {
		s.mu.Unlock()
		return "", false, nil
	}
	t := tracks[s.rnd.IntN(len(tracks))]
	s.lastEmergency = t.ID
	s.mu.Unlock()
	return EmergencyURI(t, set), true, nil
}

// EmergencyURI returns the request of an emergency track: the file under
// the media root, annotated with the track ID and, as RequestURI, the
// library title and artist.
func EmergencyURI(t Track, set Settings) string {
	artist := ""
	if t.ArtistName != nil {
		artist = *t.ArtistName
	}
	return liquidsoap.Annotate(path.Join(set.MediaRoot, t.FilePath),
		liquidsoap.Pair{Key: "track_id", Value: strconv.FormatUint(t.ID, 10)},
		liquidsoap.Pair{Key: "title", Value: t.Title},
		liquidsoap.Pair{Key: "artist", Value: artist})
}
