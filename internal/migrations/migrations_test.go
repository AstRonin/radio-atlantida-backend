package migrations

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var fileName = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.up\.sql$`)

// Migrations are forward-only, numbered without gaps, and all embedded. A
// stray .down.sql or a misnamed file would otherwise be silently ignored by
// the embed pattern.
func TestMigrationFiles(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	want := 1
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".go") {
			continue
		}
		m := fileName.FindStringSubmatch(name)
		require.NotNil(t, m, "unexpected file %q: only NNNN_name.up.sql migrations live here", name)
		v, err := strconv.Atoi(m[1])
		require.NoError(t, err)
		require.Equal(t, want, v, "migration versions must be contiguous (%s)", name)
		want++

		_, err = files.ReadFile(name)
		require.NoError(t, err, "%s is not embedded", name)
	}
	require.Greater(t, want, 1, "no migrations found")
}
