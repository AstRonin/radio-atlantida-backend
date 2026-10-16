package handlers

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/playrules"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// PlayRules manages play rules and their targets (*playrules.Service).
type PlayRules interface {
	List(ctx context.Context) ([]playrules.Rule, error)
	Get(ctx context.Context, id uint64) (playrules.Rule, error)
	Create(ctx context.Context, in playrules.RuleInput) (playrules.Rule, error)
	Update(ctx context.Context, id uint64, in playrules.RuleInput) (playrules.Rule, error)
	Delete(ctx context.Context, id uint64) error
	SetTargets(ctx context.Context, id uint64, trackIDs, playlistIDs []uint64) (playrules.Rule, error)
	SetTrackRules(ctx context.Context, trackID uint64, ruleIDs []uint64) ([]uint64, error)
	SetPlaylistRules(ctx context.Context, playlistID uint64, ruleIDs []uint64) ([]uint64, error)
}

type playRuleOutput struct {
	Body dto.PlayRule
}

type playRuleListOutput struct {
	Body dto.PlayRuleList
}

type createPlayRuleInput struct {
	Body dto.PlayRuleBody
}

type updatePlayRuleInput struct {
	ID   uint64 `path:"id" minimum:"1"`
	Body dto.PlayRuleBody
}

type setPlayRuleTargetsInput struct {
	ID   uint64 `path:"id" minimum:"1"`
	Body dto.SetPlayRuleTargets
}

func registerPlayRules(api huma.API, log *slog.Logger, svc PlayRules) {
	tags := []string{"Play rules"}

	register(api, log, huma.Operation{
		OperationID: "list-play-rules",
		Method:      http.MethodGet,
		Path:        "/api/play-rules",
		Summary:     "List play rules",
		Description: "Every rule with its targets and usage counts, in name order.",
		Tags:        tags,
		Errors:      commonErrors,
	}, func(ctx context.Context, _ *struct{}) (*playRuleListOutput, error) {
		rs, err := svc.List(ctx)
		if err != nil {
			return nil, err
		}
		out := &playRuleListOutput{Body: dto.PlayRuleList{Items: make([]dto.PlayRule, 0, len(rs))}}
		for _, r := range rs {
			out.Body.Items = append(out.Body.Items, playRuleDTO(r))
		}
		return out, nil
	})

	register(api, log, huma.Operation{
		OperationID: "get-play-rule",
		Method:      http.MethodGet,
		Path:        "/api/play-rules/{id}",
		Summary:     "Get a play rule",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *idInput) (*playRuleOutput, error) {
		r, err := svc.Get(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &playRuleOutput{Body: playRuleDTO(r)}, nil
	})

	register(api, log, huma.Operation{
		OperationID:   "create-play-rule",
		Method:        http.MethodPost,
		Path:          "/api/play-rules",
		Summary:       "Create a play rule",
		Description:   "At least one limit is set; window_from and window_to are set together and may cross midnight. An interval is at most the play log retention (interval_beyond_retention).",
		Tags:          tags,
		DefaultStatus: http.StatusCreated,
		Errors:        errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *createPlayRuleInput) (*playRuleOutput, error) {
		ri, err := ruleInput(in.Body)
		if err != nil {
			return nil, err
		}
		r, err := svc.Create(ctx, ri)
		if err != nil {
			return nil, err
		}
		return &playRuleOutput{Body: playRuleDTO(r)}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "update-play-rule",
		Method:      http.MethodPut,
		Path:        "/api/play-rules/{id}",
		Summary:     "Update a play rule",
		Description: "Replaces the limits of the rule; a limit left out is removed. The targets stay.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *updatePlayRuleInput) (*playRuleOutput, error) {
		ri, err := ruleInput(in.Body)
		if err != nil {
			return nil, err
		}
		r, err := svc.Update(ctx, in.ID, ri)
		if err != nil {
			return nil, err
		}
		return &playRuleOutput{Body: playRuleDTO(r)}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "delete-play-rule",
		Method:      http.MethodDelete,
		Path:        "/api/play-rules/{id}",
		Summary:     "Delete a play rule",
		Description: "Refused while the rule is applied to tracks or playlists (play_rule_in_use, with used_by listing them).",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *idInput) (*struct{}, error) {
		return nil, svc.Delete(ctx, in.ID)
	})

	register(api, log, huma.Operation{
		OperationID: "set-play-rule-targets",
		Method:      http.MethodPut,
		Path:        "/api/play-rules/{id}/targets",
		Summary:     "Set the targets of a play rule",
		Description: "Replaces the tracks and playlists the rule is applied to.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *setPlayRuleTargetsInput) (*playRuleOutput, error) {
		r, err := svc.SetTargets(ctx, in.ID, in.Body.TrackIDs, in.Body.PlaylistIDs)
		if err != nil {
			return nil, err
		}
		return &playRuleOutput{Body: playRuleDTO(r)}, nil
	})
}

// ruleInput converts the body. The times already match HH:MM (schema
// pattern); a parse failure is still reported per field.
func ruleInput(b dto.PlayRuleBody) (playrules.RuleInput, error) {
	in := playrules.RuleInput{
		Name: b.Name, Weekdays: b.Weekdays,
		MinIntervalMinutes: b.MinIntervalMinutes, ArtistMinIntervalMinutes: b.ArtistMinInterval, MaxPerHour: b.MaxPerHour, Notes: b.Notes,
	}
	errs := &validation.Error{}
	for _, t := range []struct {
		field string
		src   *string
		dst   **playrules.TimeOfDay
	}{{"window_from", b.WindowFrom, &in.WindowFrom}, {"window_to", b.WindowTo, &in.WindowTo}} {
		if t.src == nil {
			continue
		}
		v, err := playrules.ParseTimeOfDay(*t.src)
		if err != nil {
			errs.Add(t.field, validation.Format, "%s", err)
			continue
		}
		*t.dst = &v
	}
	return in, errs.Err()
}

func playRuleDTO(r playrules.Rule) dto.PlayRule {
	out := dto.PlayRule{
		ID: r.ID, Name: r.Name, Weekdays: r.Weekdays,
		MinIntervalMinutes: r.MinIntervalMinutes, ArtistMinInterval: r.ArtistMinIntervalMinutes, MaxPerHour: r.MaxPerHour, Notes: r.Notes,
		TrackIDs: emptyIDs(r.TrackIDs), PlaylistIDs: emptyIDs(r.PlaylistIDs),
		TrackCount: len(r.TrackIDs), PlaylistCount: len(r.PlaylistIDs),
		CreatedAt: r.CreatedAt.UTC(),
	}
	if r.WindowFrom != nil {
		out.WindowFrom = new(r.WindowFrom.String())
	}
	if r.WindowTo != nil {
		out.WindowTo = new(r.WindowTo.String())
	}
	return out
}
