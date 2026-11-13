package playrules_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/config"
	"github.com/AstRonin/radio-atlantida-backend/internal/playrules"
	"github.com/AstRonin/radio-atlantida-backend/internal/settings"
)

// longestStore is a play rule store that answers only LongestInterval.
type longestStore struct {
	playrules.Store
	minutes int
	err     error
}

func (f longestStore) LongestInterval(context.Context) (int, error) { return f.minutes, f.err }

// memSettings keeps saved settings in memory.
type memSettings map[string]settings.Stored

func (m memSettings) List(context.Context) ([]settings.Stored, error) {
	out := make([]settings.Stored, 0, len(m))
	for _, s := range m {
		out = append(out, s)
	}
	return out, nil
}

func (m memSettings) Get(_ context.Context, key string) (settings.Stored, error) {
	s, ok := m[key]
	if !ok {
		return settings.Stored{}, settings.ErrNotFound
	}
	return s, nil
}

func (m memSettings) UpsertMany(_ context.Context, sts []settings.Stored) ([]settings.Stored, error) {
	for _, s := range sts {
		m[s.Key] = s
	}
	return sts, nil
}

// The play log retention may not become shorter than the longest play rule
// interval (design §4.1).
func TestGuardRetention(t *testing.T) {
	t.Parallel()
	const days = 10
	errStore := errors.New("store down")
	for _, tc := range []struct {
		name    string
		longest int
		err     error
		refused string // the refusal message; empty when the save passes
	}{
		{name: "longest equals the retention", longest: days * 24 * 60},
		{
			name: "one minute over the retention", longest: days*24*60 + 1,
			refused: "a play rule has an interval of 14401 minutes; the play log must keep at least 11 days",
		},
		{name: "no rules", longest: 0},
		{name: "store error", err: errStore},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			saved := memSettings{}
			set, err := settings.NewService(settings.NewRegistry(), saved, config.Secrets{}.IsSet(),
				slog.New(slog.DiscardHandler), time.Now)
			require.NoError(t, err)
			rules := playrules.NewService(nil, longestStore{minutes: tc.longest, err: tc.err}, nil)
			rules.GuardRetention(set)

			_, err = set.Set(t.Context(), settings.RetentionPlayLogDays.Name(), json.RawMessage(`10`))
			switch {
			case tc.err != nil:
				require.ErrorIs(t, err, errStore)
				var ive *settings.InvalidValueError
				require.NotErrorAs(t, err, &ive, "an error, not a refusal")
				require.Empty(t, saved, "nothing saved")
			case tc.refused != "":
				var ive *settings.InvalidValueError
				require.ErrorAs(t, err, &ive)
				require.Equal(t, settings.RetentionPlayLogDays.Name(), ive.Key)
				require.Equal(t, settings.RuleIntervalBeyondRetention, ive.Rule)
				require.Equal(t, tc.refused, ive.Message)
				require.Empty(t, saved, "nothing saved")
			default:
				require.NoError(t, err)
				require.Contains(t, saved, settings.RetentionPlayLogDays.Name())
			}
		})
	}
}
