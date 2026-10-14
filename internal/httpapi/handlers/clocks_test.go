package handlers

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
)

// The panel translates warnings by code, so the published enum must list
// every code the dry check can return.
func TestClockWarningEnumMatchesDomain(t *testing.T) {
	t.Parallel()
	f, ok := reflect.TypeFor[dto.ClockWarning]().FieldByName("Code")
	require.True(t, ok)
	codes := make([]string, 0, len(clocks.WarningCodes()))
	for _, c := range clocks.WarningCodes() {
		codes = append(codes, string(c))
	}
	require.Equal(t, strings.Join(codes, ","), f.Tag.Get("enum"))
}
