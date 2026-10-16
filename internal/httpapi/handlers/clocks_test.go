package handlers

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/httpapi/dto"
	"github.com/AstRonin/radio-atlantida-backend/internal/rotation"
)

// The panel translates warnings by code, so the published enum must list
// every code the dry check and the sample build can return: the dry
// check's, then the build's that the dry check does not have.
func TestClockWarningEnumMatchesDomain(t *testing.T) {
	t.Parallel()
	f, ok := reflect.TypeFor[dto.ClockWarning]().FieldByName("Code")
	require.True(t, ok)
	var codes []string
	for _, c := range clocks.WarningCodes() {
		codes = append(codes, string(c))
	}
	for _, c := range rotation.WarningCodes() {
		if !slices.Contains(codes, string(c)) {
			codes = append(codes, string(c))
		}
	}
	require.Equal(t, strings.Join(codes, ","), f.Tag.Get("enum"))
}
