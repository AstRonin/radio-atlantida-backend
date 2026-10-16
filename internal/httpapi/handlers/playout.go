package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/playout"
	"github.com/AstRonin/radio-atlantida-backend/internal/ws"
)

// Playout puts the plan on air (*playout.Service).
type Playout interface {
	Callback(ctx context.Context, e playout.Event) error
	EmergencyNext(ctx context.Context) (string, bool, error)
	Status(ctx context.Context) (playout.Status, error)
	SetMusicGain(ctx context.Context, gain float64) error
}

type playoutCallbackInput struct {
	Body dto.PlayoutCallbackBody
}

type emergencyNextOutput struct {
	Status      int
	ContentType string `header:"Content-Type"`
	Body        []byte
}

type playoutStatusOutput struct {
	Body dto.PlayoutStatus
}

type musicGainInput struct {
	Body dto.MusicGainBody
}

type musicGainOutput struct {
	Body dto.MusicGain
}

func registerPlayout(api huma.API, log *slog.Logger, svc Playout) {
	tags := []string{"Playout"}

	register(api, log, huma.Operation{
		OperationID:   "playout-callback",
		Method:        http.MethodPost,
		Path:          "/api/playout/callback",
		Summary:       "Liquidsoap callback",
		Description:   "Called by Liquidsoap only: a track start or end, or the Icecast output state (§8.3). A started plan item moves to the play log; the first end of an item records its actual duration. Repeated events change nothing.",
		Tags:          tags,
		DefaultStatus: http.StatusNoContent,
		Errors:        errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *playoutCallbackInput) (*struct{}, error) {
		if err := svc.Callback(ctx, callbackEvent(in.Body)); err != nil {
			return nil, err
		}
		return &struct{}{}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "playout-emergency-next",
		Method:      http.MethodGet,
		Path:        "/api/playout/emergency/next",
		Summary:     "Next emergency track",
		Description: "Called by Liquidsoap only, when the plan has nothing to play (§8.2, §8.6): a Liquidsoap request for a random track of the emergency playlist (setting playout.emergency_playlist_id), or 204 when there is none.",
		Tags:        tags,
		Errors:      errorsWith(),
		Responses: map[string]*huma.Response{
			"200": {
				Description: "The request to play, for example annotate:track_id=\"5\":/media/a.mp3.",
				Content:     map[string]*huma.MediaType{"text/plain": {Schema: &huma.Schema{Type: huma.TypeString}}},
			},
			"204": {Description: "No emergency playlist, or it has no playable track."},
		},
	}, func(ctx context.Context, _ *struct{}) (*emergencyNextOutput, error) {
		uri, ok, err := svc.EmergencyNext(ctx)
		if err != nil {
			return nil, err
		}
		if !ok {
			return &emergencyNextOutput{Status: http.StatusNoContent}, nil
		}
		return &emergencyNextOutput{Status: http.StatusOK, ContentType: "text/plain; charset=utf-8", Body: []byte(uri)}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "get-playout-status",
		Method:      http.MethodGet,
		Path:        "/api/playout/status",
		Summary:     "Get the playout state",
		Description: "The Liquidsoap control link, the Icecast output, the track on air, the items waiting in Liquidsoap's queues, and the music gain.",
		Tags:        tags,
		Errors:      errorsWith(),
	}, func(ctx context.Context, _ *struct{}) (*playoutStatusOutput, error) {
		st, err := svc.Status(ctx)
		if err != nil {
			return nil, err
		}
		return &playoutStatusOutput{Body: playoutStatusDTO(st)}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "set-music-gain",
		Method:      http.MethodPut,
		Path:        "/api/playout/music-gain",
		Summary:     "Set the music gain",
		Description: "Sets the live music gain in Liquidsoap (§8.5), for example 0 for an interview and 1 afterwards. The value is kept until changed and set again after a reconnect; the setting liquidsoap.music_gain is the value at startup.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusUnprocessableEntity, http.StatusServiceUnavailable),
	}, func(ctx context.Context, in *musicGainInput) (*musicGainOutput, error) {
		if err := svc.SetMusicGain(ctx, in.Body.Gain); err != nil {
			return nil, err
		}
		return &musicGainOutput{Body: dto.MusicGain{Gain: in.Body.Gain}}, nil
	})
}

func callbackEvent(b dto.PlayoutCallbackBody) playout.Event {
	return playout.Event{
		Kind: playout.EventKind(b.Event), Time: b.Time.UTC(), Source: b.Source,
		ItemID: parseID(b.ItemID), TrackID: parseID(b.TrackID), ElapsedMS: b.ElapsedMS, Connected: b.Connected,
	}
}

// parseID reads an annotation: empty when absent. The DTO pattern allows
// digits only; a value out of range counts as absent.
func parseID(s string) *uint64 {
	id, err := strconv.ParseUint(s, 10, 64)
	if err != nil || id == 0 {
		return nil
	}
	return &id
}

func playoutStatusDTO(st playout.Status) dto.PlayoutStatus {
	out := dto.PlayoutStatus{
		ControlLink:    dto.PlayoutLink{Connected: st.Link.Connected, Since: nonZero(st.Link.Since), LastOKAt: nonZero(st.Link.LastOK)},
		LastCallbackAt: st.LastCallbackAt,
		MusicGain:      st.MusicGain,
		Queued:         make([]dto.PlayoutQueuedItem, len(st.Queued)),
	}
	if st.Output != nil {
		state := ws.IcecastConnected
		if !st.Output.Connected {
			state = ws.IcecastDisconnected
		}
		out.Output = &dto.PlayoutOutput{State: state, Since: st.Output.Since}
	}
	if np := st.NowPlaying; np != nil {
		out.NowPlaying = &dto.NowPlaying{
			ItemID: np.ItemID, TrackID: np.TrackID, Title: np.Title, ArtistName: np.ArtistName, Source: np.Source,
			StartedAt: np.StartedAt, InMS: np.InMS, OutMS: np.OutMS, Truncated: np.Truncated,
		}
	}
	for i, q := range st.Queued {
		out.Queued[i] = dto.PlayoutQueuedItem{
			ID: q.ID, TrackID: q.TrackID, Title: q.Title, ArtistName: q.ArtistName, PlannedStartUTC: q.Start,
			PlannedInMS: q.InMS, PlannedOutMS: q.OutMS, Truncated: q.Truncated, Queue: q.Queue,
		}
	}
	return out
}

// nonZero returns nil for the zero time, so the API leaves it out.
func nonZero(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
