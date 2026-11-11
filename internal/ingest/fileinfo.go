package ingest

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/AstRonin/radio-atlantida-backend/internal/library"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/media"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// fileInfoProbes bounds the ffprobe runs of FileInfo at a time. A probe
// reads only the headers, but the manager may expand many rows of the
// import listing at once, on the machine that plays out; the probes run
// beside a media operation (they only read the file).
const fileInfoProbes = 2

// FileInfo is what one probe of a file tells before it is imported.
type FileInfo struct {
	media.File
	// Probe is what ffprobe read: the duration (always set), the technical
	// facts, and the title and artist tags (empty when the file has none).
	Probe media.Info
	// Names are the title and the artist the import would give the file.
	Names Names
	// Issues are the quality issues that the probe alone shows: format,
	// sample rate, channels, low bitrate. The loudness and the true peak
	// need a decode; the analysis after the import measures them.
	Issues []library.QualityIssue
}

// FileInfo probes the file at rel, relative to the media root, as an
// import would (GET /api/import/file-info). It is not a media operation: it
// neither waits for nor blocks an import. At most fileInfoProbes run at a
// time; a request waits for a slot until ctx ends. Nothing is cached or
// stored.
//
// A path that is not clean and relative, is hidden, or names no audio
// file by its name is refused per field (path). ErrFileNotFound,
// ErrToolsUnavailable, ErrProbeFailed.
func (s *Service) FileInfo(ctx context.Context, rel string) (FileInfo, error) {
	if err := checkFilePath(rel); err != nil {
		return FileInfo{}, err
	}
	if !s.prober.Available() {
		return FileInfo{}, ErrToolsUnavailable
	}
	f, err := s.files.Stat(rel)
	switch {
	case errors.Is(err, media.ErrFileNotFound):
		return FileInfo{}, fmt.Errorf("%w: %s", ErrFileNotFound, rel)
	case err != nil:
		return FileInfo{}, fmt.Errorf("stat %s: %w", rel, err)
	}
	set, err := s.readSettings(ctx)
	if err != nil {
		return FileInfo{}, err
	}

	select {
	case s.probes <- struct{}{}:
	case <-ctx.Done():
		return FileInfo{}, fmt.Errorf("wait for a probe slot: %w", ctx.Err())
	}
	info, err := s.probe(ctx, rel)
	<-s.probes
	switch {
	case errors.Is(err, media.ErrFileNotFound):
		return FileInfo{}, fmt.Errorf("%w: %s", ErrFileNotFound, rel)
	case errors.Is(err, media.ErrProbeUnavailable):
		return FileInfo{}, ErrToolsUnavailable
	case err != nil && ctx.Err() != nil:
		return FileInfo{}, fmt.Errorf("probe %s: %w", rel, ctx.Err())
	case err != nil:
		// The manager sees "not readable as audio"; the log keeps why. A
		// file that the import would skip as unreadable is not an alert.
		s.log.WarnContext(ctx, "file info: file not readable as audio", "file_path", rel, logging.KeyError, err)
		return FileInfo{}, fmt.Errorf("%w: %s: %w", ErrProbeFailed, rel, err)
	}
	return FileInfo{
		File:  f,
		Probe: info,
		Names: importNames(rel, info),
		// No loudness: Check leaves out what it was not given.
		Issues: Check(measurementsOf(info, nil, nil), set.Standard),
	}, nil
}

// checkFilePath refuses a path that the import listing would not show as
// an audio file: not a clean relative path, a hidden element, or not
// importable by its name.
func checkFilePath(rel string) error {
	if err := media.ValidatePath(rel); err != nil {
		return validation.Field("path", validation.Invalid, "%s", strings.TrimPrefix(err.Error(), media.ErrInvalidPath.Error()+": "))
	}
	for el := range strings.SplitSeq(rel, "/") {
		if strings.HasPrefix(el, ".") {
			return validation.Field("path", validation.Invalid, "hidden files and folders (a name starting with \".\") are not imported")
		}
	}
	if !importable(path.Base(rel)) {
		return validation.Field("path", validation.NotAudio, "an import skips this file by its name: not an audio file (mp3, m4a, aac, ogg, oga, flac, wav) or a temporary file")
	}
	return nil
}
