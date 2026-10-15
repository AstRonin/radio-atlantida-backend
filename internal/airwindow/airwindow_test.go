package airwindow_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/airwindow"
	"github.com/AstRonin/radio-atlantida-backend/internal/liquidsoap"
	"github.com/AstRonin/radio-atlantida-backend/internal/playout"
)

var t0 = time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)

func at(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }

// aired is a played row that started at minute min and played 3 minutes.
func aired(id uint64, min int) airwindow.Played {
	return airwindow.Played{
		ID: id + 100, ScheduledItemID: new(id), Status: airwindow.StatusAired, Source: "auto", TrackID: id,
		Title: "T", PlannedStart: new(at(min)), StartedAt: new(at(min)), PlannedOutMS: new(180000),
		ActualDurationMS: new(180000),
	}
}

func queued(id uint64, min int, inLiquidsoap bool) airwindow.Queued {
	return airwindow.Queued{ID: id, TrackID: id, Title: "Q", Start: at(min), OutMS: 180000, InLiquidsoap: inLiquidsoap}
}

func onAir(id uint64, min int) *playout.NowPlaying {
	return &playout.NowPlaying{
		ItemID: new(id), TrackID: id, Title: "N", Source: playout.SourcePlan, StartedAt: at(min), OutMS: new(180000),
	}
}

// ids returns the rows as kind:item for a compact comparison.
func ids(rows []airwindow.Row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		id := "-"
		if r.ItemID != nil {
			id = strconv.FormatUint(*r.ItemID, 10)
		}
		out[i] = string(r.Kind) + ":" + id
	}
	return out
}

func TestAssembleOrder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		played []airwindow.Played
		now    *playout.NowPlaying
		queued []airwindow.Queued
		count  int
		want   []string
	}{
		{
			name: "played oldest first, now, queued in planned order; the item on air is not a played row",
			// Newest first, as the store returns them; item 4 is on air and
			// in the play log already.
			played: []airwindow.Played{aired(4, 9), aired(3, 6), aired(2, 3), aired(1, 0)},
			now:    onAir(4, 9),
			queued: []airwindow.Queued{queued(5, 12, true), queued(6, 15, false)},
			count:  3,
			want:   []string{"played:101", "played:102", "played:103", "now:4", "queued:5", "queued:6"},
		},
		{
			name:   "fewer rows than the counts",
			played: []airwindow.Played{aired(1, 0)},
			count:  10,
			want:   []string{"played:101"},
		},
		{
			name:   "count limits the played rows to the newest",
			played: []airwindow.Played{aired(3, 6), aired(2, 3), aired(1, 0)},
			count:  2,
			want:   []string{"played:102", "played:103"},
		},
		{
			name:  "no data",
			count: 10,
			want:  []string{},
		},
		{
			name:   "an emergency track on air has no item",
			played: []airwindow.Played{aired(1, 0)},
			now:    &playout.NowPlaying{TrackID: 9, Source: playout.SourceEmergency, StartedAt: at(3)},
			count:  10,
			want:   []string{"played:101", "now:-"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, ids(airwindow.Assemble(tt.played, tt.now, tt.queued, tt.count)))
		})
	}
}

func TestAssembleRows(t *testing.T) {
	t.Parallel()
	truncated := aired(2, 3)
	truncated.Status, truncated.ActualDurationMS = airwindow.StatusTruncated, new(60000)
	missed := airwindow.Played{
		ID: 103, ScheduledItemID: new(uint64(3)), Status: airwindow.StatusMissed, Source: "auto", TrackID: 3,
		PlannedStart: new(at(6)), PlannedOutMS: new(240000), Resolution: new("open"), OutageID: new(uint64(7)),
	}
	emergency := airwindow.Played{
		ID: 104, Status: airwindow.StatusAired, Source: "emergency", TrackID: 4, StartedAt: new(at(10)),
		ActualDurationMS: new(30000),
	}
	rows := airwindow.Assemble(
		[]airwindow.Played{emergency, missed, truncated},
		onAir(5, 12),
		[]airwindow.Queued{queued(6, 15, true), {ID: 7, TrackID: 7, Start: at(18), InMS: 1000, OutMS: 61000}},
		10,
	)
	require.Equal(t, []airwindow.Row{
		{
			Kind: airwindow.KindPlayed, Status: "truncated", ItemID: new(uint64(102)), TrackID: 2, Title: "T",
			Source: "plan", Start: at(3), End: new(at(4)), Truncated: true,
		},
		{
			Kind: airwindow.KindPlayed, Status: "missed", ItemID: new(uint64(103)), TrackID: 3, Source: "plan",
			Start: at(6), End: new(at(10)), Resolution: new("open"), OutageID: new(uint64(7)),
		},
		{
			Kind: airwindow.KindPlayed, Status: "aired", ItemID: new(uint64(104)), TrackID: 4, Source: "emergency",
			Start: at(10), End: new(at(10).Add(30 * time.Second)),
		},
		{
			Kind: airwindow.KindNow, Status: "now", ItemID: new(uint64(5)), TrackID: 5, Title: "N", Source: "plan",
			Start: at(12), End: new(at(15)), InLiquidsoap: true,
		},
		{
			Kind: airwindow.KindQueued, Status: "queued", ItemID: new(uint64(6)), TrackID: 6, Title: "Q", Source: "plan",
			Start: at(15), End: new(at(18)), InLiquidsoap: true,
		},
		{
			Kind: airwindow.KindQueued, Status: "planned", ItemID: new(uint64(7)), TrackID: 7, Source: "plan",
			Start: at(18), End: new(at(19)),
		},
	}, rows)
}

func TestQueuedFrom(t *testing.T) {
	t.Parallel()
	require.Equal(t, at(9), airwindow.QueuedFrom(onAir(4, 9), at(11)), "the start of the item on air")
	require.Equal(t, at(11), airwindow.QueuedFrom(nil, at(11)), "nothing on air: the current time")
}

type fakeStore struct {
	played      []airwindow.Played
	queued      []airwindow.Queued
	err         error
	playedLimit int
	queuedFrom  *time.Time
	queuedLimit int
}

func (f *fakeStore) Played(_ context.Context, limit int) ([]airwindow.Played, error) {
	f.playedLimit = limit
	return f.played, f.err
}

func (f *fakeStore) Queued(_ context.Context, from time.Time, limit int) ([]airwindow.Queued, error) {
	f.queuedFrom, f.queuedLimit = &from, limit
	return f.queued, f.err
}

type fakePlayout struct {
	now       *playout.NowPlaying
	connected bool
}

func (f fakePlayout) NowPlaying() *playout.NowPlaying { return f.now }
func (f fakePlayout) Link() liquidsoap.State          { return liquidsoap.State{Connected: f.connected} }

func counts(played, queued int) airwindow.SettingsFunc {
	return func(context.Context) (airwindow.Settings, error) {
		return airwindow.Settings{PlayedCount: played, QueuedCount: queued}, nil
	}
}

func TestWindow(t *testing.T) {
	t.Parallel()
	clock := func() time.Time { return at(11).In(time.FixedZone("x", 4*3600)) }
	tests := []struct {
		name      string
		playout   fakePlayout
		queued    int
		wantFrom  *time.Time
		wantRows  []string
		connected bool
	}{
		{
			name:      "on air: queued from its start",
			playout:   fakePlayout{now: onAir(4, 9), connected: true},
			queued:    9,
			wantFrom:  new(at(9)),
			wantRows:  []string{"played:103", "now:4", "queued:5"},
			connected: true,
		},
		{
			name:      "between tracks: queued from now",
			playout:   fakePlayout{connected: true},
			queued:    9,
			wantFrom:  new(at(11)),
			wantRows:  []string{"played:103", "played:104", "queued:5"},
			connected: true,
		},
		{
			name: "control link down: no now row, the state is unknown",
			// The playout state still holds a track; it is not trusted.
			playout:  fakePlayout{now: onAir(4, 9)},
			queued:   9,
			wantFrom: new(at(11)),
			wantRows: []string{"played:103", "played:104", "queued:5"},
		},
		{
			name:      "zero queued: no query",
			playout:   fakePlayout{connected: true},
			wantRows:  []string{"played:103", "played:104"},
			connected: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeStore{
				played: []airwindow.Played{aired(4, 9), aired(3, 6)},
				queued: []airwindow.Queued{queued(5, 12, true)},
			}
			if tt.queued == 0 {
				store.queued = nil
			}
			w, err := airwindow.NewService(store, tt.playout, counts(10, tt.queued), clock).Window(t.Context())
			require.NoError(t, err)
			require.Equal(t, at(11), w.GeneratedAt)
			require.Equal(t, time.UTC, w.GeneratedAt.Location())
			require.Equal(t, tt.connected, w.ControlLinkConnected)
			require.Equal(t, tt.wantRows, ids(w.Rows))
			require.Equal(t, 11, store.playedLimit, "one more than shown, for the item on air")
			require.Equal(t, tt.wantFrom, store.queuedFrom)
			if tt.queued > 0 {
				require.Equal(t, tt.queued, store.queuedLimit)
			}
		})
	}
}

func TestWindowErrors(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	_, err := airwindow.NewService(&fakeStore{err: boom}, fakePlayout{}, counts(10, 9), time.Now).Window(t.Context())
	require.ErrorIs(t, err, boom)
	_, err = airwindow.NewService(&fakeStore{}, fakePlayout{}, func(context.Context) (airwindow.Settings, error) {
		return airwindow.Settings{}, boom
	}, time.Now).Window(t.Context())
	require.ErrorIs(t, err, boom)
}
