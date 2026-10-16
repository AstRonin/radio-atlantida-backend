package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/outage"
	"github.com/AstRonin/radio-atlantida-backend/internal/rotation"
)

// Outages serves the played-tracks list, the missed items, and the outages
// (*outage.Service).
type Outages interface {
	History(ctx context.Context, f outage.HistoryFilter) ([]outage.Entry, *outage.Cursor, error)
	Missed(ctx context.Context, r outage.Resolution) ([]outage.Entry, error)
	Dismiss(ctx context.Context, id uint64) (outage.Entry, error)
	Outages(ctx context.Context, from, to time.Time) ([]outage.Outage, error)
	OpenOutage(ctx context.Context) (outage.Outage, bool, error)
}

// Rescheduler places a missed item into a later hour (*rotation.Service).
type Rescheduler interface {
	Reschedule(ctx context.Context, r rotation.RescheduleRequest) (uint64, error)
}

type playHistoryInput struct {
	From   time.Time `query:"from" required:"true" doc:"Rows that aired (or were planned, for missed items) at or after this instant (UTC)."`
	To     time.Time `query:"to" required:"true" doc:"... and before this one; at most 31 days after from."`
	Status string    `query:"status" enum:"aired,truncated,missed" doc:"Only rows with this status."`
	Limit  int       `query:"limit" minimum:"1" maximum:"500" default:"100"`
	Cursor string    `query:"cursor" doc:"next_cursor of the previous page."`
}

type playHistoryOutput struct {
	Body dto.PlayHistory
}

type missedListInput struct {
	Resolution string `query:"resolution" enum:"open,rescheduled,dismissed" default:"open"`
}

type missedListOutput struct {
	Body dto.MissedItemList
}

type missedItemOutput struct {
	Body dto.PlayHistoryEntry
}

type rescheduleInput struct {
	ID   uint64 `path:"id" minimum:"1"`
	Body dto.RescheduleBody
}

type outageListInput struct {
	From time.Time `query:"from" required:"true" doc:"Outages that end at or after this instant (UTC), or are open."`
	To   time.Time `query:"to" required:"true" doc:"... and start before this one; at most 7 days after from."`
}

type outageListOutput struct {
	Body dto.OutageList
}

func registerOutages(api huma.API, log *slog.Logger, svc Outages, rs Rescheduler) {
	tags := []string{"Outages"}

	register(api, log, huma.Operation{
		OperationID: "get-play-history",
		Method:      http.MethodGet,
		Path:        "/api/play-history",
		Summary:     "Get the played-tracks list",
		Description: "Aired, truncated, and missed items in one list, newest first (§7.1). Missed items carry their outage and resolution. Paginated by cursor.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *playHistoryInput) (*playHistoryOutput, error) {
		f := outage.HistoryFilter{From: in.From.UTC(), To: in.To.UTC(), Limit: in.Limit}
		if in.Status != "" {
			f.Status = new(outage.Status(in.Status))
		}
		if in.Cursor != "" {
			c, err := outage.ParseCursor(in.Cursor)
			if err != nil {
				return nil, err
			}
			f.After = &c
		}
		rows, next, err := svc.History(ctx, f)
		if err != nil {
			return nil, err
		}
		out := &playHistoryOutput{Body: dto.PlayHistory{Items: entriesDTO(rows)}}
		if next != nil {
			out.Body.NextCursor = new(next.String())
		}
		return out, nil
	})

	register(api, log, huma.Operation{
		OperationID: "list-missed-items",
		Method:      http.MethodGet,
		Path:        "/api/missed-items",
		Summary:     "List missed items",
		Description: "Items that did not air because of an outage (§7.1), newest planned first. Open items wait for the manager to reschedule or dismiss them.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *missedListInput) (*missedListOutput, error) {
		rows, err := svc.Missed(ctx, outage.Resolution(in.Resolution))
		if err != nil {
			return nil, err
		}
		return &missedListOutput{Body: dto.MissedItemList{Items: entriesDTO(rows)}}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "dismiss-missed-item",
		Method:      http.MethodPost,
		Path:        "/api/missed-items/{id}/dismiss",
		Summary:     "Dismiss a missed item",
		Description: "The item will not be broadcast again. Its row is then kept for the play log retention period.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *idInput) (*missedItemOutput, error) {
		e, err := svc.Dismiss(ctx, in.ID)
		if err != nil {
			return nil, err
		}
		return &missedItemOutput{Body: entryDTO(e)}, nil
	})

	register(api, log, huma.Operation{
		OperationID:   "reschedule-missed-item",
		Method:        http.MethodPost,
		Path:          "/api/missed-items/{id}/reschedule",
		Summary:       "Reschedule a missed item",
		Description:   "Places the item into a later hour, at its start, its end, or a fixed minute, for that hour only; the clock template is not changed (§7.1, §7.2). Checks that need no build are refused per field; the hour is then rebuilt by a reschedule operation, which fails when the track breaks separation or a play rule at its place or does not fit whole.",
		Tags:          tags,
		DefaultStatus: http.StatusAccepted,
		Errors:        errorsWith(http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *rescheduleInput) (*operationAcceptedOutput, error) {
		id, err := rs.Reschedule(ctx, rotation.RescheduleRequest{
			MissedID: in.ID, HourStart: in.Body.TargetHourStartUTC.UTC(), Placement: clocks.Placement(in.Body.Placement),
			FixedMinute: in.Body.FixedMinute, FixedToleranceMS: in.Body.FixedToleranceMS,
		})
		if err != nil {
			return nil, err
		}
		return &operationAcceptedOutput{Body: dto.OperationAccepted{OperationID: id}}, nil
	})

	register(api, log, huma.Operation{
		OperationID: "list-outages",
		Method:      http.MethodGet,
		Path:        "/api/outages",
		Summary:     "List outages",
		Description: "Broadcast interruptions that overlap the range, newest first, with the number of items they kept off the air (§7.1). An open outage has no ended_at.",
		Tags:        tags,
		Errors:      errorsWith(http.StatusUnprocessableEntity),
	}, func(ctx context.Context, in *outageListInput) (*outageListOutput, error) {
		os, err := svc.Outages(ctx, in.From.UTC(), in.To.UTC())
		if err != nil {
			return nil, err
		}
		out := &outageListOutput{Body: dto.OutageList{Items: make([]dto.Outage, len(os))}}
		for i, o := range os {
			out.Body.Items[i] = dto.Outage{
				ID: o.ID, StartedAt: o.StartedAt, EndedAt: o.EndedAt, DetectedBy: string(o.DetectedBy),
				Component: string(o.Component), Reason: o.Reason, ReconciledAt: o.ReconciledAt, MissedCount: o.MissedCount,
			}
		}
		return out, nil
	})
}

func entriesDTO(es []outage.Entry) []dto.PlayHistoryEntry {
	out := make([]dto.PlayHistoryEntry, len(es))
	for i, e := range es {
		out[i] = entryDTO(e)
	}
	return out
}

func entryDTO(e outage.Entry) dto.PlayHistoryEntry {
	out := dto.PlayHistoryEntry{
		ID: e.ID, Status: string(e.Status), Source: e.Source, TrackID: e.TrackID, Title: e.Title, ArtistName: e.ArtistName,
		PlannedStartUTC: e.PlannedStart, StartedAt: e.StartedAt, PlannedInMS: e.PlannedInMS, PlannedOutMS: e.PlannedOutMS,
		ActualDurationMS: e.ActualDurationMS, ResolvedAt: e.ResolvedAt, ResolvedItemID: e.ResolvedItemID,
	}
	if e.ClockID != nil {
		out.Clock = &dto.PlayHistoryClock{ID: *e.ClockID, Name: e.ClockName}
	}
	if o := e.Outage; o != nil {
		out.Outage = &dto.OutageRef{ID: o.ID, Component: string(o.Component), Reason: o.Reason}
	}
	if e.Resolution != nil {
		out.Resolution = new(string(*e.Resolution))
	}
	return out
}
