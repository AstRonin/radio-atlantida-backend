package handlers

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/AstRonin/radio-atlantida-backend/internal/airwindow"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
)

// AirWindow builds the air window (*airwindow.Service).
type AirWindow interface {
	Window(ctx context.Context) (airwindow.Window, error)
}

type airWindowOutput struct {
	Body dto.AirWindow
}

func registerAirWindow(api huma.API, log *slog.Logger, svc AirWindow) {
	register(api, log, huma.Operation{
		OperationID: "get-air-window",
		Method:      http.MethodGet,
		Path:        "/api/air-window",
		Summary:     "Get the air window",
		Description: "The last items that were due (aired, truncated, missed), the item on air, and the next planned items, " +
			"in air order. The counts are the settings air.played_count and air.queued_count. " +
			"With the Liquidsoap control link down the answer is still 200, without a now row.",
		Tags:   []string{"Playout"},
		Errors: commonErrors,
	}, func(ctx context.Context, _ *struct{}) (*airWindowOutput, error) {
		w, err := svc.Window(ctx)
		if err != nil {
			return nil, err
		}
		return &airWindowOutput{Body: airWindowDTO(w)}, nil
	})
}

func airWindowDTO(w airwindow.Window) dto.AirWindow {
	rows := make([]dto.AirWindowRow, len(w.Rows))
	for i, r := range w.Rows {
		rows[i] = dto.AirWindowRow{
			Kind: string(r.Kind), Status: r.Status, ItemID: r.ItemID, TrackID: r.TrackID, Title: r.Title,
			ArtistName: r.ArtistName, Source: r.Source, StartUTC: r.Start.UTC(), EndUTC: r.End,
			Truncated: r.Truncated, Resolution: r.Resolution, OutageID: r.OutageID, InLiquidsoap: r.InLiquidsoap,
		}
	}
	return dto.AirWindow{GeneratedAt: w.GeneratedAt.UTC(), ControlLinkConnected: w.ControlLinkConnected, Rows: rows}
}
