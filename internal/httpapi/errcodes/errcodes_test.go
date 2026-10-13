package errcodes

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

var codePattern = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

func TestCodesAreUniqueSnakeCase(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	var all []string
	for _, c := range Problems() {
		all = append(all, string(c))
	}
	for _, c := range Fields() {
		all = append(all, string(c))
	}
	for _, c := range all {
		require.Regexp(t, codePattern, c)
		require.False(t, seen[c], "duplicate code %q", c)
		seen[c] = true
	}
}
