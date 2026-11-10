package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/rotation"
)

const jsonHeader = "Content-Type: application/json"

// pinRotation records the pin and move requests the handlers pass on; the
// embedded nil interface panics on any other call.
type pinRotation struct {
	Rotation
	pin  *rotation.PinRequest
	move *rotation.MoveInsertRequest
}

func (f *pinRotation) PinFill(_ context.Context, r rotation.PinRequest) (uint64, error) {
	f.pin = &r
	return 1, nil
}

func (f *pinRotation) MoveInsert(_ context.Context, r rotation.MoveInsertRequest) (uint64, error) {
	f.move = &r
	return 2, nil
}

// The pin and move bodies take tolerance_ms as integer milliseconds, with the
// range of POST /api/track-plan/inserts; the seconds name is not read.
func TestPinAndMoveTakeToleranceInMilliseconds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want *int
	}{
		{"absent takes the default", `{"plan_item_id":5,"offset_ms":1800000}`, nil},
		{"milliseconds are passed as they are", `{"plan_item_id":5,"offset_ms":1800000,"tolerance_ms":20000}`, new(20000)},
		{"zero is a tolerance", `{"plan_item_id":5,"offset_ms":1800000,"tolerance_ms":0}`, new(0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, api := humatest.New(t)
			svc := &pinRotation{}
			registerRotation(api, slog.New(slog.DiscardHandler), svc)

			resp := api.Post("/api/track-plan/pins", jsonHeader, strings.NewReader(tt.body))
			require.Equal(t, http.StatusAccepted, resp.Code, resp.Body.String())
			require.NotNil(t, svc.pin)
			require.Equal(t, tt.want, svc.pin.ToleranceMS)

			resp = api.Patch("/api/track-plan/inserts/3", jsonHeader, strings.NewReader(`{"offset_ms":1800000,"tolerance_ms":20000}`))
			require.Equal(t, http.StatusAccepted, resp.Code, resp.Body.String())
			require.NotNil(t, svc.move)
			require.Equal(t, new(20000), svc.move.ToleranceMS)
		})
	}
}

// A tolerance outside 0 to 600000 ms is refused by the request schema, as on
// POST /api/track-plan/inserts, and so is the old seconds name (unknown
// property); the service is not called.
func TestPinAndMoveRefuseToleranceOutOfRange(t *testing.T) {
	t.Parallel()
	bodies := []string{
		`{"plan_item_id":5,"offset_ms":1800000,"tolerance_ms":-1}`,
		`{"plan_item_id":5,"offset_ms":1800000,"tolerance_ms":600001}`,
		`{"plan_item_id":5,"offset_ms":1800000,"tolerance_s":20}`,
	}
	for _, body := range bodies {
		_, api := humatest.New(t)
		svc := &pinRotation{}
		registerRotation(api, slog.New(slog.DiscardHandler), svc)

		resp := api.Post("/api/track-plan/pins", jsonHeader, strings.NewReader(body))
		require.Equal(t, http.StatusUnprocessableEntity, resp.Code, body+": "+resp.Body.String())
		require.Nil(t, svc.pin)
	}
	for _, tol := range []string{"-1", "600001", "20"} {
		_, api := humatest.New(t)
		svc := &pinRotation{}
		registerRotation(api, slog.New(slog.DiscardHandler), svc)
		field := "tolerance_ms"
		if tol == "20" {
			field = "tolerance_s"
		}
		resp := api.Patch("/api/track-plan/inserts/3", jsonHeader, strings.NewReader(`{"offset_ms":1800000,"`+field+`":`+tol+`}`))
		require.Equal(t, http.StatusUnprocessableEntity, resp.Code, tol+": "+resp.Body.String())
		require.Nil(t, svc.move)
	}
}
