package ingest

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/media"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
	"github.com/AstRonin/radio-atlantida-backend/internal/workerpool"
)

const (
	// importWorkers: probing is quick, measuring decodes the whole file
	// on one core. Two files at a time use two cores and leave the rest to
	// playout.
	importWorkers = 2
	// maxImportFiles bounds one import run; the rest wait for the next
	// run (decision 4).
	maxImportFiles = 5000
	// maxInboxListing bounds the inbox listing (decision 12).
	maxInboxListing = 1000
	// Limits of the track and artist names taken from tags (the API limits
	// of the track and artist forms).
	maxTitleLength  = 500
	maxArtistLength = 200
)

// Outcomes of one inbox file.
const (
	outcomeImported   = "imported"
	outcomeDuplicate  = "duplicates"
	outcomeUnreadable = "unreadable"
	outcomeSkipped    = "skipped"
	outcomeFailed     = "failed"
)

// audioExtensions are the extensions an import takes (lower case).
var audioExtensions = map[string]bool{
	".mp3": true, ".m4a": true, ".aac": true, ".ogg": true, ".oga": true, ".flac": true, ".wav": true,
}

// importable reports whether an inbox file is an audio file and not a
// temporary or system file that is still being copied or belongs to an
// editor.
func importable(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, ".") || strings.HasPrefix(lower, "~$") ||
		strings.HasSuffix(lower, ".part") || strings.HasSuffix(lower, ".tmp") {
		return false
	}
	return audioExtensions[path.Ext(lower)]
}

// ImportRequest is what the manager chooses for a whole import.
type ImportRequest struct {
	TypeID      uint64
	PlaylistIDs []uint64
}

// Inbox is the content of the inbox folder.
type Inbox struct {
	Dir   string
	Files []media.File
	// More is true when the folder holds more files than listed.
	More bool
}

// Inbox lists the files waiting in the inbox folder.
func (s *Service) Inbox(ctx context.Context) (Inbox, error) {
	set, err := s.readSettings(ctx)
	if err != nil {
		return Inbox{}, err
	}
	files, more, err := s.files.Walk(ctx, set.InboxDir, maxInboxListing)
	if err != nil {
		return Inbox{}, fmt.Errorf("list inbox: %w", err)
	}
	if files == nil {
		files = []media.File{}
	}
	return Inbox{Dir: set.InboxDir, Files: files, More: more}, nil
}

// Import starts an import operation of the inbox folder and returns its
// ID. The type and the playlists apply to every imported file.
func (s *Service) Import(ctx context.Context, req ImportRequest) (uint64, error) {
	if err := s.toolsAvailable(); err != nil {
		return 0, err
	}
	if err := s.checkImport(ctx, req); err != nil {
		return 0, err
	}
	return s.start(ctx, operations.KindImport, func(ctx context.Context, id uint64) (string, error) {
		return s.runImport(ctx, id, req)
	})
}

func (s *Service) checkImport(ctx context.Context, req ImportRequest) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	errs := &validation.Error{}
	ok, err := s.store.TypeExists(ctx, req.TypeID)
	if err != nil {
		return fmt.Errorf("check playlist type: %w", err)
	}
	if !ok {
		errs.Add("type_id", validation.UnknownID, "no playlist type has this ID")
	}
	missing, err := s.store.MissingPlaylists(ctx, req.PlaylistIDs)
	if err != nil {
		return fmt.Errorf("check playlists: %w", err)
	}
	if len(missing) > 0 {
		errs.Add("playlist_ids", validation.UnknownID, "no playlist has the IDs %v", missing)
	}
	return errs.Err()
}

func (s *Service) runImport(ctx context.Context, id uint64, req ImportRequest) (string, error) {
	set, err := s.readSettings(ctx)
	if err != nil {
		return "", err
	}
	if overlap(set.InboxDir, set.LibraryDir) {
		return "", fmt.Errorf("the inbox folder %q and the library folder %q must not contain each other (settings import.inbox_dir, import.library_dir)",
			set.InboxDir, set.LibraryDir)
	}
	files, more, err := s.files.Walk(ctx, set.InboxDir, maxImportFiles)
	if err != nil {
		return "", fmt.Errorf("scan inbox: %w", err)
	}
	t := newTally()
	im := &importer{s: s, req: req, set: set, seen: map[string]bool{}}
	err = workerpool.ForEach(ctx, importWorkers, files, func(ctx context.Context, f media.File) error {
		outcome := im.file(ctx, f)
		done := t.add(outcome, f.Path, outcome == outcomeFailed)
		s.ops.Progress(id, operations.KindImport, done, len(files), f.Path)
		// Only a cancelled context stops the batch; a file that fails is
		// counted and logged.
		return ctx.Err()
	})
	if err != nil {
		return "", fmt.Errorf("import stopped after %d of %d files: %w", t.done, len(files), err)
	}
	summary := t.summary(outcomeImported, outcomeDuplicate, outcomeUnreadable, outcomeSkipped, outcomeFailed)
	if more {
		summary += fmt.Sprintf("; more than %d files: run the import again for the rest", maxImportFiles)
	}
	return summary, nil
}

// overlap reports whether one folder contains the other.
func overlap(a, b string) bool {
	return a == b || strings.HasPrefix(b, a+"/") || strings.HasPrefix(a, b+"/")
}

// importer imports the files of one run.
type importer struct {
	s   *Service
	req ImportRequest
	set Settings

	// seen holds the checksums of this run, so two copies of one file in
	// the inbox import once.
	mu   sync.Mutex
	seen map[string]bool
}

// file imports one inbox file (decision 4) and returns its outcome.
func (im *importer) file(ctx context.Context, f media.File) string {
	s := im.s
	if !importable(path.Base(f.Path)) {
		return outcomeSkipped
	}
	sum, err := s.files.Checksum(ctx, f.Path)
	if err != nil {
		return im.fail(ctx, f.Path, "read file", err)
	}
	if im.seenBefore(sum) {
		s.log.InfoContext(ctx, "import: duplicate in the inbox left there", "file_path", f.Path)
		return outcomeDuplicate
	}
	dctx, cancel := context.WithTimeout(ctx, dbTimeout)
	_, dup, err := s.store.TrackByChecksum(dctx, sum)
	cancel()
	if err != nil {
		return im.fail(ctx, f.Path, "check duplicate", err)
	}
	if dup {
		s.log.InfoContext(ctx, "import: file already in the library left in the inbox", "file_path", f.Path)
		return outcomeDuplicate
	}

	info, loud, err := s.analyse(ctx, f.Path)
	if err != nil {
		if ctx.Err() != nil {
			return outcomeFailed
		}
		s.log.WarnContext(ctx, "import: file not readable as audio", "file_path", f.Path, logging.KeyError, err)
		return outcomeUnreadable
	}
	m := measurementsOf(info, loud, nil)

	rel := strings.TrimPrefix(f.Path, im.set.InboxDir+"/")
	dst, err := s.files.FreeName(im.set.LibraryDir + "/" + rel)
	if err != nil {
		return im.fail(ctx, f.Path, "choose library name", err)
	}
	if err = s.files.Move(f.Path, dst); err != nil {
		return im.fail(ctx, f.Path, "move to library", err)
	}

	err = im.register(ctx, info, path.Base(f.Path), Analysis{
		FilePath: dst, Checksum: sum, Measurements: m, Issues: Check(m, im.set.Standard), AnalyzedAt: s.now().UTC(),
	})
	if err != nil {
		if merr := s.files.Move(dst, f.Path); merr != nil {
			// The file stays in the library folder without a track
			// (known limit); the log names both places.
			s.log.ErrorContext(ctx, "import: moving the file back to the inbox failed",
				"file_path", f.Path, "library_path", dst, logging.KeyError, merr)
		}
		return im.fail(ctx, f.Path, "register track", err)
	}
	return outcomeImported
}

// register creates the track and, when the file names one, its artist, in
// one transaction. Without a title tag, the title is the file's name in
// the inbox.
func (im *importer) register(ctx context.Context, info media.Info, name string, a Analysis) error {
	s := im.s
	title := cmp.Or(info.Title, strings.TrimSuffix(name, path.Ext(name)))
	nt := NewTrack{
		Title: truncate(title, maxTitleLength), TypeID: im.req.TypeID, PlaylistIDs: im.req.PlaylistIDs, Analysis: a,
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	return s.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		st := s.store.WithTx(tx)
		if info.Artist != "" {
			aid, err := st.EnsureArtist(ctx, truncate(info.Artist, maxArtistLength))
			if err != nil {
				return fmt.Errorf("artist %q: %w", info.Artist, err)
			}
			nt.ArtistID = &aid
		}
		_, err := st.CreateTrack(ctx, nt)
		return err
	})
}

func (im *importer) seenBefore(sum string) bool {
	im.mu.Lock()
	defer im.mu.Unlock()
	if im.seen[sum] {
		return true
	}
	im.seen[sum] = true
	return false
}

func (im *importer) fail(ctx context.Context, rel, step string, err error) string {
	if ctx.Err() == nil {
		im.s.log.ErrorContext(ctx, "import: "+step+" failed", "file_path", rel, logging.KeyError, err)
	}
	return outcomeFailed
}

// analyse probes the file at rel and measures its loudness. A silent file
// has no loudness (nil); a file without a duration is not audio the
// planner can use.
func (s *Service) analyse(ctx context.Context, rel string) (media.Info, *media.Loudness, error) {
	info, err := s.prober.Probe(ctx, rel)
	if err != nil {
		return media.Info{}, nil, err
	}
	if info.DurationMS == nil {
		return media.Info{}, nil, errors.New("no duration")
	}
	l, err := s.ff.Measure(ctx, rel)
	switch {
	case errors.Is(err, media.ErrSilent):
		return info, nil, nil
	case err != nil:
		return media.Info{}, nil, err
	}
	return info, &l, nil
}

// truncate shortens s to at most n characters.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strings.TrimSpace(string([]rune(s)[:n]))
}
