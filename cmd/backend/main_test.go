package main

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRunArguments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		args      []string
		wantUsage bool
	}{
		{"no command", nil, true},
		{"unknown command", []string{"start"}, true},
		{"migrate down does not exist", []string{"migrate", "down"}, true},
		{"migrate without direction", []string{"migrate"}, true},
		{"serve with arguments", []string{"serve", "x"}, true},
		{"openapi not implemented yet", []string{"openapi"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := run(t.Context(), tt.args, &bytes.Buffer{})

			require.Error(t, err)
			require.Equal(t, tt.wantUsage, errors.Is(err, errUsage))
		})
	}
}

func TestRunHelp(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer

	require.NoError(t, run(t.Context(), []string{"help"}, &out))

	require.Equal(t, usage, out.String())
}
