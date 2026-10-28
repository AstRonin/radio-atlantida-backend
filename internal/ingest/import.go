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
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/media"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
	"github.com/AstRonin/radio-atlantida-backend/internal/workerpool"
)

const (
	// importWorkers: an import only probes and hashes the files it registers;
	// the loudness measurement, which decodes the whole file, is the
	// analysis (analyseWorkers).
	importWorkers = 10
	// maxImportFiles bounds the files walked in one import run, those
	// already in the library included; the rest wait for the next run
	// (docs/phase-18-plan.md, decision 12).
	maxImportFiles = 5000
	// maxArtistLength is the limit of an artist name (the API limit of the
	// artist form); the title has library.MaxTitleLength.
	maxArtistLength = 200
)

// Outcomes of one file of an import.
const (
	outcomeImported   = "imported"
	outcomeInLibrary  = "in_library"
	outcomeDuplicate  = "duplicates"
	outcomeUnreadable = "unreadable"
	outcomeSkipped    = "skipped"
	outcomeFailed     = "failed"
)

// audioExtensions are the extensions an import takes (lower case).
var audioExtensions = map[string]bool{
	".mp3": true, ".m4a": true, ".aac": true, ".ogg": true, ".oga": true, ".flac": true, ".wav": true,
}

// importable reports whether a file is an audio file and not a temporary
// or system file that is still being copied or belongs to an editor.
func importable(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, ".") || strings.HasPrefix(lower, "~$") ||
		strings.HasSuffix(lower, ".part") || strings.HasSuffix(lower, ".tmp") {
		return false
	}
	return audioExtensions[path.Ext(lower)]
}

// ImportRequest is what the manager chooses for an import. The type, the
// playlists, and the artist apply to the new tracks of the run only.
type ImportRequest struct {
	// Folder is relative to the media root; "" is the media root.
	Folder      string
	TypeID      uint64
	PlaylistIDs []uint64
	// ArtistID, when set, is the artist of every new track; it wins over
	// the artist tag and the file name.
	ArtistID *uint64
}

// Import starts an import operation of a folder and returns its ID. Every
// audio file of the folder and its subfolders that is not a track yet
// becomes one where it lies; nothing is moved.
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
	if err := s.checkFolder(req.Folder, errs); err != nil {
		return err
	}
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
	if req.ArtistID != nil {
		ok, err := s.store.ArtistExists(ctx, *req.ArtistID)
		if err != nil {
			return fmt.Errorf("check artist: %w", err)
		}
		if !ok {
			errs.Add("artist_id", validation.UnknownID, "no artist has this ID")
		}
	}
	return errs.Err()
}

func (s *Service) runImport(ctx context.Context, id uint64, req ImportRequest) (string, error) {
	set, err := s.readSettings(ctx)
	if err != nil {
		return "", err
	}
	files, more, err := s.files.Walk(ctx, req.Folder, maxImportFiles)
	if err != nil {
		return "", fmt.Errorf("scan %s: %w", folderLabel(req.Folder), err)
	}
	idx, err := s.libraryIndex(ctx, req.Folder)
	if err != nil {
		return "", err
	}
	t := newTally()
	// The analysis of each registered track runs beside the import, on
	// analyseWorkers goroutines; the operation ends when both are done.
	registered := make(chan Track)
	im := &importer{
		s: s, req: req, idx: idx, seen: map[string]string{}, registered: registered,
		opID: id, total: newTrackTotal(0),
	}
	var analysing sync.WaitGroup
	ta := newTally()
	for range analyseWorkers {
		analysing.Go(func() {
			for tr := range registered {
				res := s.analyseTrack(ctx, tr, set.Standard, false, ta)
				s.publishAnalysed(im.opID, im.total, ta, tr, res)
			}
		})
	}

	err = workerpool.ForEach(ctx, importWorkers, files, func(ctx context.Context, f media.File) error {
		outcome := im.file(ctx, f)
		done := t.add(outcome, f.Path, outcome == outcomeFailed)
		s.ops.Progress(id, operations.KindImport, done, len(files), f.Path)
		// Only a cancelled context stops the batch; a file that fails is
		// counted and logged.
		return ctx.Err()
	})
	// No file is registered after the walk: the analysis drains what is
	// left, whether the walk finished or was cancelled.
	close(registered)
	analysing.Wait()
	if err != nil {
		return "", fmt.Errorf("import stopped after %d of %d files: %w", t.done, len(files), err)
	}
	summary := folderLabel(req.Folder) + ": " +
		t.summary(outcomeImported, outcomeInLibrary, outcomeDuplicate, outcomeUnreadable, outcomeSkipped, outcomeFailed) +
		"; analysis: " + ta.summary(outcomeAnalysed, outcomeRechecked, outcomeMissing, outcomeDeleted, outcomeUnreadable, outcomeFailed)
	if more {
		summary += fmt.Sprintf("; more than %d files: run the import again for the rest", maxImportFiles)
	}
	return summary, nil
}

// folderLabel names a folder of an import in its summary.
func folderLabel(folder string) string {
	if folder == "" {
		return "whole media folder"
	}
	return "folder " + folder
}

// importer imports the files of one run.
type importer struct {
	s   *Service
	req ImportRequest
	// idx holds the tracks of the folder when the run started.
	idx libraryIndex

	// seen maps the checksums of this run to their files, so two copies
	// of one file import once.
	mu   sync.Mutex
	seen map[string]string
	// registered receives every track the run registers, with the checksum
	// the import read; its analysis runs on the other side of the channel.
	registered chan<- Track
	// opID is the import operation; total counts the tracks it has
	// registered, for the track.analysed events.
	opID  uint64
	total *trackTotal
}

// file imports one file (decision 7) and returns its outcome. The file
// stays where it is in every case. The track is registered with what the
// probe reads; its loudness and issues come from the analysis.
func (im *importer) file(ctx context.Context, f media.File) string {
	s := im.s
	switch st, _ := im.idx.status(f.Path); st {
	case FileNotAudio:
		return outcomeSkipped
	case FileInLibrary, FileOriginal:
		// Decided by the path alone, so a second run over a large folder
		// reads no file.
		return outcomeInLibrary
	}
	sum, err := s.files.Checksum(ctx, f.Path)
	if err != nil {
		return im.fail(ctx, f.Path, "read file", err)
	}
	if first, dup := im.seenBefore(sum, f.Path); dup {
		s.log.InfoContext(ctx, "import: file is a copy of another file of this run; left alone",
			"file_path", f.Path, "copy_of", first)
		return outcomeDuplicate
	}
	dctx, cancel := context.WithTimeout(ctx, dbTimeout)
	existing, dup, err := s.store.TrackByChecksum(dctx, sum)
	cancel()
	if err != nil {
		return im.fail(ctx, f.Path, "check duplicate", err)
	}
	if dup {
		// The file was copied or moved on disk (decision 10).
		s.log.InfoContext(ctx, "import: file has the content of a track at another path; left alone",
			"file_path", f.Path, "track_id", existing.ID, "track_file_path", existing.FilePath)
		return outcomeDuplicate
	}

	info, err := s.probe(ctx, f.Path)
	if err != nil {
		if ctx.Err() != nil {
			return outcomeFailed
		}
		s.log.WarnContext(ctx, "import: file not readable as audio", "file_path", f.Path, logging.KeyError, err)
		return outcomeUnreadable
	}
	id, err := im.register(ctx, info, Analysis{
		FilePath: f.Path, Checksum: sum, Measurements: measurementsOf(info, nil, nil),
	})
	if err != nil {
		return im.fail(ctx, f.Path, "register track", err)
	}
	// Counted before the analysis can see the track, so that the total
	// is never below the tracks analysed.
	im.total.register()
	im.registered <- Track{ID: id, FilePath: f.Path, Hashed: sum}
	return outcomeImported
}

// register creates the track at a.FilePath and, when needed, its artist,
// in one transaction, and returns its ID. The title and the artist are
// decided each on its own (decision 6a): the title from the tag, else from
// the file name; the artist from the request, else the tag, else the file
// name, else none.
func (im *importer) register(ctx context.Context, info media.Info, a Analysis) (uint64, error) {
	s := im.s
	base := path.Base(a.FilePath)
	nameTitle, nameArtist := nameParts(strings.TrimSuffix(base, path.Ext(base)))
	// The title rule (docs/phase-21-plan.md §2, decision 5) removes what the
	// API refuses: control characters and runs of white space.
	title := cmp.Or(library.CleanTitle(info.Title), library.CleanTitle(nameTitle))
	artist := cmp.Or(strings.TrimSpace(info.Artist), nameArtist)
	nt := NewTrack{
		Title: truncate(title, library.MaxTitleLength), ArtistID: im.req.ArtistID,
		TypeID: im.req.TypeID, PlaylistIDs: im.req.PlaylistIDs, Analysis: a,
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	var id uint64
	err := s.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		st := s.store.WithTx(tx)
		if nt.ArtistID == nil && artist != "" {
			aid, err := st.EnsureArtist(ctx, truncate(artist, maxArtistLength))
			if err != nil {
				return fmt.Errorf("artist %q: %w", artist, err)
			}
			nt.ArtistID = &aid
		}
		var err error
		id, err = st.CreateTrack(ctx, nt)
		return err
	})
	return id, err
}

// seenBefore records the checksum of the file at p and returns the file of
// this run that had it first.
func (im *importer) seenBefore(sum, p string) (string, bool) {
	im.mu.Lock()
	defer im.mu.Unlock()
	if first, ok := im.seen[sum]; ok {
		return first, true
	}
	im.seen[sum] = p
	return "", false
}

func (im *importer) fail(ctx context.Context, rel, step string, err error) string {
	if ctx.Err() == nil {
		im.s.log.ErrorContext(ctx, "import: "+step+" failed", "file_path", rel, logging.KeyError, err)
	}
	return outcomeFailed
}

// probe reads the format and tags of the file at rel. A file without a
// duration is not audio the planner can use.
func (s *Service) probe(ctx context.Context, rel string) (media.Info, error) {
	info, err := s.prober.Probe(ctx, rel)
	if err != nil {
		return media.Info{}, err
	}
	if info.DurationMS == nil {
		return media.Info{}, errors.New("no duration")
	}
	return info, nil
}

// analyse probes the file at rel and measures its loudness. A silent file
// has no loudness (nil).
func (s *Service) analyse(ctx context.Context, rel string) (media.Info, *media.Loudness, error) {
	info, err := s.probe(ctx, rel)
	if err != nil {
		return media.Info{}, nil, err
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
