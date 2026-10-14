package media

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
}

func TestWalk(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, "incoming/b.mp3", "bb")
	write(t, root, "incoming/a/z.mp3", "z")
	write(t, root, "incoming/.hidden.mp3", "h")
	write(t, root, "incoming/.trash/x.mp3", "x")
	write(t, root, "library/c.mp3", "c")
	outside := filepath.Join(t.TempDir(), "o.mp3")
	require.NoError(t, os.WriteFile(outside, []byte("o"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "incoming", "link.mp3")))
	lib := New(root)

	files, more, err := lib.Walk(t.Context(), "incoming", 10)
	require.NoError(t, err)
	require.False(t, more)
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.Path
	}
	require.Equal(t, []string{"incoming/a/z.mp3", "incoming/b.mp3"}, paths)
	require.Equal(t, int64(2), files[1].Size)

	files, more, err = lib.Walk(t.Context(), "incoming", 1)
	require.NoError(t, err)
	require.True(t, more)
	require.Len(t, files, 1)

	files, more, err = lib.Walk(t.Context(), "missing", 10)
	require.NoError(t, err)
	require.False(t, more)
	require.Empty(t, files)

	_, _, err = lib.Walk(t.Context(), "../x", 10)
	require.ErrorIs(t, err, ErrInvalidPath)
}

func TestChecksum(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, "a.mp3", "abc")
	sum, err := New(root).Checksum(t.Context(), "a.mp3")
	require.NoError(t, err)
	require.Equal(t, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", sum)
	_, err = New(root).Checksum(t.Context(), "b.mp3")
	require.ErrorIs(t, err, ErrFileNotFound)
}

func TestFreeNameAndMove(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, "incoming/x/song.mp3", "new")
	write(t, root, "library/x/song.mp3", "old")
	write(t, root, "library/x/song (2).mp3", "older")
	lib := New(root)

	name, err := lib.FreeName("library/x/song.mp3")
	require.NoError(t, err)
	require.Equal(t, "library/x/song (3).mp3", name)
	name, err = lib.FreeName("library/y/song.mp3")
	require.NoError(t, err)
	require.Equal(t, "library/y/song.mp3", name)

	require.ErrorIs(t, lib.Move("incoming/x/song.mp3", "library/x/song.mp3"), ErrExists, "never overwrites")
	require.NoError(t, lib.Move("incoming/x/song.mp3", "library/y/song.mp3"), "creates the folder")
	b, err := os.ReadFile(filepath.Join(root, "library", "y", "song.mp3")) //nolint:gosec // G304: test file in a temp dir
	require.NoError(t, err)
	require.Equal(t, "new", string(b))
	ok, err := lib.Exists("incoming/x/song.mp3")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestCommitAndRemove(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, "m/a.mp3", "old")
	write(t, root, "m/.a.tmp.mp3", "new")
	lib := New(root)

	require.NoError(t, lib.Commit("m/.a.tmp.mp3", "m/a.mp3"), "replaces the file")
	b, err := os.ReadFile(filepath.Join(root, "m", "a.mp3")) //nolint:gosec // G304: test file in a temp dir
	require.NoError(t, err)
	require.Equal(t, "new", string(b))
	ok, err := lib.Exists("m/.a.tmp.mp3")
	require.NoError(t, err)
	require.False(t, ok)

	require.NoError(t, lib.Remove("m/a.mp3"))
	require.NoError(t, lib.Remove("m/a.mp3"), "a missing file is not an error")
	require.ErrorIs(t, lib.Remove("/etc/passwd"), ErrInvalidPath)
}
