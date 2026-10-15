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

	files, more, err = lib.Walk(t.Context(), "", 10)
	require.NoError(t, err, "the root")
	require.False(t, more)
	require.Len(t, files, 3)
	require.Equal(t, "library/c.mp3", files[2].Path)
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

func TestTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, "music/rock/a.mp3", "a")
	write(t, root, "music/b.mp3", "bb")
	write(t, root, "music-old/c.mp3", "c")
	write(t, root, "jingles/.hidden/x.mp3", "x")
	write(t, root, "top.mp3", "t")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "empty"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(root, ".trash", "inner"), 0o750))
	require.NoError(t, os.Symlink(filepath.Join(root, "music"), filepath.Join(root, "linked")))
	lib := New(root)

	tree, err := lib.Tree(t.Context(), "", 10)
	require.NoError(t, err)
	require.Equal(t, []string{"empty", "jingles", "music", "music/rock", "music-old"}, tree.Folders,
		"a folder before its subfolders; hidden folders and links skipped")
	paths := make([]string, len(tree.Files))
	for i, f := range tree.Files {
		paths[i] = f.Path
	}
	require.Equal(t, []string{"music/b.mp3", "music/rock/a.mp3", "music-old/c.mp3", "top.mp3"}, paths)

	tree, err = lib.Tree(t.Context(), "music", 10)
	require.NoError(t, err)
	require.Equal(t, []string{"music/rock"}, tree.Folders)
	require.Len(t, tree.Files, 2)

	_, err = lib.Tree(t.Context(), "", 3)
	require.ErrorIs(t, err, ErrTooManyFiles)
	_, err = lib.Tree(t.Context(), "/etc", 3)
	require.ErrorIs(t, err, ErrInvalidPath)
}

func TestIsDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, "music/a.mp3", "a")
	require.NoError(t, os.Symlink(filepath.Join(root, "music"), filepath.Join(root, "linked")))
	lib := New(root)
	for rel, want := range map[string]bool{"music": true, "music/a.mp3": false, "missing": false, "linked": false} {
		ok, err := lib.IsDir(rel)
		require.NoError(t, err, rel)
		require.Equal(t, want, ok, rel)
	}
	_, err := lib.IsDir("../x")
	require.ErrorIs(t, err, ErrInvalidPath)
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
	_, err = os.Stat(filepath.Join(root, "m", ".a.tmp.mp3"))
	require.ErrorIs(t, err, os.ErrNotExist)

	require.NoError(t, lib.Remove("m/a.mp3"))
	require.NoError(t, lib.Remove("m/a.mp3"), "a missing file is not an error")
	require.ErrorIs(t, lib.Remove("/etc/passwd"), ErrInvalidPath)
}
