package media

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
)

// File is a regular file under the media root.
type File struct {
	// Path is relative to the root, with forward slashes.
	Path    string
	Size    int64
	ModTime time.Time
}

// Tree is what a folder holds at any depth.
type Tree struct {
	// Folders are relative to the root, in path order: a folder comes
	// before its subfolders, siblings by name. The folder itself is not
	// listed.
	Folders []string
	// Files are in path order.
	Files []File
}

// ErrTooManyFiles is returned by Tree when the folder holds more files
// than its limit.
var ErrTooManyFiles = errors.New("too many files in the media folder")

// Walk lists the regular files under dir (relative to the root, "" for the
// root itself) in path order, at most limit of them; more reports that
// files were left out. Hidden entries (a name starting with ".") are
// skipped, directories included, and so are symbolic links. A missing dir
// has no files.
func (l *Library) Walk(ctx context.Context, dir string, limit int) (files []File, more bool, err error) {
	err = l.walk(ctx, dir, func(p string, d fs.DirEntry) error {
		if !d.Type().IsRegular() {
			return nil
		}
		if len(files) == limit {
			more = true
			return fs.SkipAll
		}
		f, ferr := fileOf(p, d)
		if ferr != nil {
			return ferr
		}
		files = append(files, f)
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return files, more, nil
}

// Tree lists the folders and the regular files under dir ("" for the root)
// with the rules of Walk. A folder with more than limit files returns
// ErrTooManyFiles: counts made from a part of a tree would be wrong.
func (l *Library) Tree(ctx context.Context, dir string, limit int) (Tree, error) {
	var t Tree
	err := l.walk(ctx, dir, func(p string, d fs.DirEntry) error {
		switch {
		case d.IsDir():
			t.Folders = append(t.Folders, p)
		case d.Type().IsRegular():
			if len(t.Files) == limit {
				return fmt.Errorf("%w: more than %d", ErrTooManyFiles, limit)
			}
			f, err := fileOf(p, d)
			if err != nil {
				return err
			}
			t.Files = append(t.Files, f)
		}
		return nil
	})
	if err != nil {
		return Tree{}, err
	}
	return t, nil
}

// IsDir reports whether a directory (not a link to one) is at rel.
func (l *Library) IsDir(rel string) (bool, error) {
	if err := ValidatePath(rel); err != nil {
		return false, err
	}
	root, err := os.OpenRoot(l.root)
	if err != nil {
		return false, fmt.Errorf("open media root: %w", err)
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat(rel)
	switch {
	case err == nil:
		return info.IsDir(), nil
	case errors.Is(err, fs.ErrNotExist) || isEscape(err):
		return false, nil
	default:
		return false, fmt.Errorf("stat %s: %w", rel, err)
	}
}

// walk calls fn for every entry under dir ("" for the root) in path order,
// except dir itself and hidden entries (a hidden directory is not
// entered). A symbolic link is passed with its link type, so it is
// neither a directory nor a regular file to fn. A missing dir has no
// entries.
func (l *Library) walk(ctx context.Context, dir string, fn func(p string, d fs.DirEntry) error) error {
	start := dir
	if dir == "" {
		start = "."
	} else if err := ValidatePath(dir); err != nil {
		return err
	}
	root, err := os.OpenRoot(l.root)
	if err != nil {
		return fmt.Errorf("open media root: %w", err)
	}
	defer func() { _ = root.Close() }()

	err = fs.WalkDir(root.FS(), start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == start && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if p == start {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		return fn(p, d)
	})
	if err != nil {
		return fmt.Errorf("walk %s: %w", cmp.Or(dir, "media root"), err)
	}
	return nil
}

func fileOf(p string, d fs.DirEntry) (File, error) {
	info, err := d.Info()
	if err != nil {
		return File{}, err
	}
	return File{Path: p, Size: info.Size(), ModTime: info.ModTime().UTC()}, nil
}

// Checksum returns the SHA-256 of the file at rel, in hex (§8.7: a changed
// file is detected by its checksum).
func (l *Library) Checksum(ctx context.Context, rel string) (string, error) {
	f, err := l.Open(rel)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, ctxReader{ctx: ctx, r: f}); err != nil {
		return "", fmt.Errorf("hash %s: %w", rel, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ctxReader stops a long read when ctx ends.
type ctxReader struct {
	ctx context.Context //nolint:containedctx // lives only for one io.Copy
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// Commit makes the finished file at tmp durable and renames it to dst,
// replacing a file there. A reader of dst sees the old file or the new one,
// never a part of it.
func (l *Library) Commit(tmp, dst string) error {
	if err := ValidatePath(tmp); err != nil {
		return err
	}
	if err := ValidatePath(dst); err != nil {
		return err
	}
	root, err := os.OpenRoot(l.root)
	if err != nil {
		return fmt.Errorf("open media root: %w", err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(tmp, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", tmp, err)
	}
	serr := f.Sync()
	cerr := f.Close()
	if err := errors.Join(serr, cerr); err != nil {
		return fmt.Errorf("sync %s: %w", tmp, err)
	}
	if err := root.Rename(tmp, dst); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmp, dst, err)
	}
	return nil
}

// Remove deletes the file at rel. A missing file is not an error.
func (l *Library) Remove(rel string) error {
	if err := ValidatePath(rel); err != nil {
		return err
	}
	root, err := os.OpenRoot(l.root)
	if err != nil {
		return fmt.Errorf("open media root: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := root.Remove(rel); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", rel, err)
	}
	return nil
}
