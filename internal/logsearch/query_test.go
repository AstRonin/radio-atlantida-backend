package logsearch

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

var (
	t0 = time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Hour)
)

func valid() Query { return Query{From: t0, To: t1} }

func TestValidateAcceptsValidQueries(t *testing.T) {
	t.Parallel()
	tests := map[string]Query{
		"range only":        valid(),
		"all filters":       {From: t0, To: t1, MinLevel: "WARN", Component: "api", Search: "boom", RequestID: "host/abc+/=-000001", Limit: 500, Cursor: encodeCursor(t0)},
		"search at 200":     {From: t0, To: t1, Search: strings.Repeat("я", 200)},
		"limit 1":           {From: t0, To: t1, Limit: 1},
		"request id prefix": {From: t0, To: t1, RequestID: "a-000001"},
	}
	for name, q := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.NoError(t, q.Validate())
		})
	}
}

func TestValidateRefusesWithFieldAndRule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		q     Query
		field string
		rule  validation.Rule
	}{
		{"from missing", Query{To: t1}, "query.from", validation.Required},
		{"to missing", Query{From: t0}, "query.to", validation.Required},
		{"to before from", Query{From: t1, To: t0}, "query.to", validation.Order},
		{"to equals from", Query{From: t0, To: t0}, "query.to", validation.Order},
		{"unknown level", Query{From: t0, To: t1, MinLevel: "TRACE"}, "query.level", validation.Enum},
		{"lower case level", Query{From: t0, To: t1, MinLevel: "warn"}, "query.level", validation.Enum},
		{"unknown component", Query{From: t0, To: t1, Component: "playout"}, "query.component", validation.Enum},
		{"search too long", Query{From: t0, To: t1, Search: strings.Repeat("a", 201)}, "query.search", validation.Length},
		{"search 201 runes", Query{From: t0, To: t1, Search: strings.Repeat("я", 201)}, "query.search", validation.Length},
		{"request id with space", Query{From: t0, To: t1, RequestID: "a b"}, "query.request_id", validation.Pattern},
		{"request id with quote", Query{From: t0, To: t1, RequestID: `a"b`}, "query.request_id", validation.Pattern},
		{"limit 501", Query{From: t0, To: t1, Limit: 501}, "query.limit", validation.Range},
		{"limit negative", Query{From: t0, To: t1, Limit: -1}, "query.limit", validation.Range},
		{"cursor garbage", Query{From: t0, To: t1, Cursor: "!!"}, "query.cursor", validation.Invalid},
		{"cursor of another format", Query{From: t0, To: t1, Cursor: "bm90LWEtY3Vyc29y"}, "query.cursor", validation.Invalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.q.Validate()
			require.Error(t, err)
			var ve *validation.Error
			require.True(t, errors.As(err, &ve), "want *validation.Error, got %T", err)
			require.Len(t, ve.Fields, 1, ve.Error())
			require.Equal(t, tt.field, ve.Fields[0].Field)
			require.Equal(t, tt.rule, ve.Fields[0].Rule)
		})
	}
}

func TestLevelRegexExpandsToHigherLevels(t *testing.T) {
	t.Parallel()
	require.Equal(t, "DEBUG|INFO|WARN|ERROR", levelRegex("DEBUG"))
	require.Equal(t, "INFO|WARN|ERROR", levelRegex("INFO"))
	require.Equal(t, "WARN|ERROR", levelRegex("WARN"))
	require.Equal(t, "ERROR", levelRegex("ERROR"))
}

func TestComponentEnumIsTheLoggingComponents(t *testing.T) {
	t.Parallel()
	var want []string
	for _, c := range logging.Components() {
		want = append(want, string(c))
	}
	require.Equal(t, want, componentNames())
}

func TestPageSizeDefault(t *testing.T) {
	t.Parallel()
	require.Equal(t, DefaultLimit, Query{}.pageSize())
	require.Equal(t, 7, Query{Limit: 7}.pageSize())
}
