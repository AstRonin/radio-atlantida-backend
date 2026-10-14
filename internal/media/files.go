package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

// ErrExists is returned when a move would overwrite a file.
var ErrExists = errors.New("media file exists")

// File is a regular file under the media root.
type File struct {
	// Path is relative to the root, with forward slashes.
	Path    string
	Size    int64
	ModTime time.Time
}

// Walk lists the regular files under dir (relative to the root) in path
// order, at most limit of them; more reports that files were left out.
// Hidden entries (a name starting with ".") are skipped, directories
// included, and so are symbolic links. A missing dir has no files.
func (l *Library) Walk(ctx context.Context, dir string, limit int) (files []File, more bool, err error) {
	if err = ValidatePath(dir); err != nil {
		return nil, false, err
	}
	root, err := os.OpenRoot(l.root)
	if err != nil {
		return nil, false, fmt.Errorf("open media root: %w", err)
	}
	defer func() { _ = root.Close() }()

	err = fs.WalkDir(root.FS(), dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if p != dir && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if len(files) == limit {
			more = true
			return fs.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files = append(files, File{Path: p, Size: info.Size(), ModTime: info.ModTime().UTC()})
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("walk %s: %w", dir, err)
	}
	return files, more, nil
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

// Exists reports whether anything (a file, a directory, a link) is at rel.
func (l *Library) Exists(rel string) (bool, error) {
	if err := ValidatePath(rel); err != nil {
		return false, err
	}
	root, err := os.OpenRoot(l.root)
	if err != nil {
		return false, fmt.Errorf("open media root: %w", err)
	}
	defer func() { _ = root.Close() }()
	_, err = root.Lstat(rel)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("stat %s: %w", rel, err)
	}
}

// maxNameTries bounds the search for a free name.
const maxNameTries = 1000

// FreeName returns rel when nothing is there, else the first free
// "<name> (n).<ext>" next to it, n from 2.
func (l *Library) FreeName(rel string) (string, error) {
	ext := path.Ext(rel)
	stem := strings.TrimSuffix(rel, ext)
	candidate := rel
	for n := 2; n <= maxNameTries+1; n++ {
		taken, err := l.Exists(candidate)
		if err != nil {
			return "", err
		}
		if !taken {
			return candidate, nil
		}
		candidate = stem + " (" + strconv.Itoa(n) + ")" + ext
	}
	return "", fmt.Errorf("%w: no free name for %s", ErrExists, rel)
}

// Move moves the file at src to dst, creating the folders of dst. It never
// overwrites: when something is at dst it returns ErrExists.
func (l *Library) Move(src, dst string) error {
	if err := ValidatePath(src); err != nil {
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
	if _, err := root.Lstat(dst); err == nil {
		return fmt.Errorf("%w: %s", ErrExists, dst)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", dst, err)
	}
	if dir := path.Dir(dst); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create folder %s: %w", dir, err)
		}
	}
	if err := root.Rename(src, dst); err != nil {
		return fmt.Errorf("move %s to %s: %w", src, dst, err)
	}
	return nil
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
