// Package media resolves track files under MEDIA_ROOT (§8.7 of the design).
//
// The database stores paths relative to the media root, with forward
// slashes, so the same database works on macOS and Windows hosts. Every file
// is opened inside the root (os.OpenInRoot): neither "..", an absolute path,
// nor a symbolic link can reach a file outside it. The client never sends a
// path to read; the preview resolves a track by its ID (§6.6).
package media

import (
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Errors returned by the package.
var (
	// ErrInvalidPath: the path is not a clean path relative to the root.
	ErrInvalidPath = errors.New("invalid media path")
	// ErrFileNotFound: no regular file exists at the path.
	ErrFileNotFound = errors.New("media file not found")
)

// MaxPathLength bounds a stored path; the API schema uses the same limit.
const MaxPathLength = 1024

var drivePrefix = regexp.MustCompile(`^[A-Za-z]:`)

// ValidatePath checks that p is a clean relative path with forward slashes:
// no leading slash, no drive letter, no backslash, no "." or ".." element,
// no empty element. The database checks the same rules
// (ck_tracks_file_path_relative); this check gives the reason.
func ValidatePath(p string) error {
	var reason string
	switch {
	case p == "":
		reason = "is empty"
	case len(p) > MaxPathLength:
		reason = fmt.Sprintf("is longer than %d bytes", MaxPathLength)
	case !utf8.ValidString(p) || strings.ContainsRune(p, 0):
		reason = "is not valid text"
	case strings.Contains(p, `\`):
		reason = "contains a backslash; use forward slashes"
	case strings.HasPrefix(p, "/") || drivePrefix.MatchString(p):
		reason = "is absolute; give it relative to the media root"
	case !fs.ValidPath(p) || p == ".":
		reason = `has an empty, "." or ".." element`
	case path.Clean(p) != p:
		reason = "is not in clean form"
	default:
		return nil
	}
	return fmt.Errorf("%w: path %s", ErrInvalidPath, reason)
}

// Library is the media root.
type Library struct {
	root string
}

// New returns the library at root (MEDIA_ROOT). The directory is not opened
// here: it may appear after startup (a network share), and every call opens
// the file afresh.
func New(root string) *Library { return &Library{root: root} }

// Open opens the regular file at the relative path rel for reading. A
// missing file, a directory, or a path that leaves the root returns
// ErrFileNotFound. The caller closes the file.
func (l *Library) Open(rel string) (*os.File, error) {
	if err := ValidatePath(rel); err != nil {
		return nil, err
	}
	f, err := os.OpenInRoot(l.root, filepath.FromSlash(rel))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || isEscape(err) {
			return nil, fmt.Errorf("%w: %s", ErrFileNotFound, rel)
		}
		return nil, fmt.Errorf("open media file %s: %w", rel, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat media file %s: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrFileNotFound, rel)
	}
	return f, nil
}

// abs returns the host path of the relative path rel. Only for handing a
// file that Open accepted to ffmpeg or ffprobe, which take a path.
func (l *Library) abs(rel string) string {
	return filepath.Join(l.root, filepath.FromSlash(rel))
}

// isEscape reports the error of os.OpenInRoot for a symbolic link that
// points outside the root. To the caller such a file does not exist.
func isEscape(err error) bool {
	var pe *fs.PathError
	return errors.As(err, &pe) && pe.Err.Error() == "path escapes from parent"
}

// contentTypes maps the stored formats (Format* constants) to media types.
var contentTypes = map[string]string{
	FormatMP3:  "audio/mpeg",
	FormatAAC:  "audio/aac",
	FormatM4A:  "audio/mp4",
	FormatOgg:  "audio/ogg",
	FormatFLAC: "audio/flac",
	FormatWAV:  "audio/wav",
}

// ContentType returns the media type of a track file: from its stored
// format, else from the file extension, else application/octet-stream.
func ContentType(format, rel string) string {
	if ct, ok := contentTypes[format]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(path.Ext(rel)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}
