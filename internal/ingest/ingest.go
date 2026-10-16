// Package ingest brings audio files into the library and keeps them at the
// broadcast standard (docs/phase-11-plan.md): the import of a folder of the
// media root, which registers its files where they lie
// (docs/phase-18-plan.md), the quality analysis of track files, and the
// transcodes that fix what the analysis found ("Fix loudness", "Optimize
// track").
//
// The three run as asynchronous operations (§6.5) one at a time, on the
// worker goroutine of the service (Run): they touch the same files, and
// ffmpeg must not take the CPU that playout needs on the same machine.
//
// Persistence goes through internal/repository/ingest; files and ffmpeg
// through internal/media.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/media"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
)

// Errors returned by the service.
var (
	// ErrRunning: an import, analysis, or transcode is running; one runs
	// at a time (decision 9).
	ErrRunning = errors.New("another media operation is running")
	// ErrToolsUnavailable: ffmpeg or ffprobe is not installed.
	ErrToolsUnavailable = errors.New("ffmpeg or ffprobe is not available")
)

// Track is a library track as the analysis sees it.
type Track struct {
	ID       uint64
	FilePath string
	// Checksum is the SHA-256 of the file when it was last analysed.
	Checksum     *string
	Measurements Measurements
	Issues       []library.QualityIssue
	// AnalyzedAt is nil until the file is analysed.
	AnalyzedAt *time.Time
}

// TrackFile is a track and the path of its file.
type TrackFile struct {
	ID       uint64
	FilePath string
}

// NewTrack is a track created by an import.
type NewTrack struct {
	Title string
	// ArtistID is nil for a track without an artist.
	ArtistID *uint64
	TypeID   uint64
	// PlaylistIDs are the playlists of the import request.
	PlaylistIDs []uint64
	Analysis    Analysis
}

// Analysis is the stored result of analysing a track file.
type Analysis struct {
	FilePath     string
	Checksum     string
	Measurements Measurements
	Issues       []library.QualityIssue
	AnalyzedAt   time.Time
}

// Store persists what the service changes (internal/repository/ingest).
type Store interface {
	// WithTx returns the store bound to tx.
	WithTx(tx db.Tx) Store
	// TypeExists reports whether the playlist type exists.
	TypeExists(ctx context.Context, id uint64) (bool, error)
	// MissingPlaylists returns the IDs of ids that are not playlists.
	MissingPlaylists(ctx context.Context, ids []uint64) ([]uint64, error)
	// ArtistExists reports whether the artist exists.
	ArtistExists(ctx context.Context, id uint64) (bool, error)
	// TrackByChecksum returns a track whose file has the checksum.
	TrackByChecksum(ctx context.Context, sum string) (t TrackFile, ok bool, err error)
	// TrackPaths maps the file path of every track under folder ("" for
	// every track) to the track.
	TrackPaths(ctx context.Context, folder string) (map[string]uint64, error)
	// TrackByPath returns the track of the file path.
	TrackByPath(ctx context.Context, path string) (id uint64, ok bool, err error)
	// EnsureArtist returns the artist with the name, ignoring case, and
	// creates it when there is none.
	EnsureArtist(ctx context.Context, name string) (uint64, error)
	// CreateTrack adds a track with its playlists and analysis.
	// library.ErrFilePathTaken, ErrUnknownType, ErrUnknownPlaylist.
	CreateTrack(ctx context.Context, t NewTrack) (uint64, error)
	// Tracks returns the tracks of ids that exist, in ID order.
	Tracks(ctx context.Context, ids []uint64) ([]Track, error)
	// Unanalysed returns at most limit tracks never analysed, in ID order.
	Unanalysed(ctx context.Context, limit int) ([]Track, error)
	// SaveAnalysis stores the analysis of a track and its file path, and
	// sets quality_ok. A new checksum resets a cue that does not fit the
	// new duration; cueReset tells. library.ErrTrackNotFound,
	// ErrFilePathTaken.
	SaveAnalysis(ctx context.Context, id uint64, a Analysis) (cueReset bool, err error)
	// MarkMissing records that the file of a track is missing: the
	// file_missing issue alone, quality_ok false. The measurements stay.
	MarkMissing(ctx context.Context, id uint64, at time.Time) error
	// Busy returns the tracks of ids that are queued in Liquidsoap, on air
	// at now, or planned to start before until.
	Busy(ctx context.Context, ids []uint64, now, until time.Time) ([]uint64, error)
	// Waveform returns the stored waveform of a track; false when there is
	// none.
	Waveform(ctx context.Context, trackID uint64) (Waveform, bool, error)
	// SaveWaveform stores the waveform of a track, replacing the one
	// before. library.ErrTrackNotFound.
	SaveWaveform(ctx context.Context, trackID uint64, w Waveform) error
}

// TxRunner runs fn in one transaction (*db.DB).
type TxRunner interface {
	WithTx(ctx context.Context, fn func(ctx context.Context, tx db.Tx) error) error
}

// Files are the files under MEDIA_ROOT (*media.Library).
type Files interface {
	Walk(ctx context.Context, dir string, limit int) ([]media.File, bool, error)
	Tree(ctx context.Context, dir string, limit int) (media.Tree, error)
	IsDir(rel string) (bool, error)
	Checksum(ctx context.Context, rel string) (string, error)
	Commit(tmp, dst string) error
	Remove(rel string) error
}

// Prober reads the format and tags of a file (*media.Prober).
type Prober interface {
	Available() bool
	Probe(ctx context.Context, rel string) (media.Info, error)
}

// Encoder measures, encodes, and draws files (*media.FFmpeg).
type Encoder interface {
	Available() bool
	Measure(ctx context.Context, rel string) (media.Loudness, error)
	Encode(ctx context.Context, src, dst string, enc media.Encoding, norm *media.Normalize) error
	Peaks(ctx context.Context, rel string, bucketMS int) ([]int8, int, error)
}

// Operations stores the operations and publishes their events
// (*operations.Tracker).
type Operations interface {
	Start(ctx context.Context, kind operations.Kind) (uint64, error)
	Execute(ctx context.Context, id uint64, component logging.Component, fn operations.Func) error
	Progress(id uint64, kind operations.Kind, done, total int, message string)
}

// Settings are the settings of the service, read when an operation starts.
type Settings struct {
	Standard Standard
	// BusyAhead: a track planned to start within this time is busy, so
	// its file is not replaced under the pusher (decision 8).
	BusyAhead time.Duration
}

// SettingsFunc reads the settings.
type SettingsFunc func(ctx context.Context) (Settings, error)

const (
	// dbTimeout bounds each statement or transaction.
	dbTimeout = 5 * time.Second
	// maxFailedNames bounds the file names listed in a result summary; the
	// log has all of them.
	maxFailedNames = 5
)

// job is an operation waiting for the worker.
type job struct {
	id   uint64
	kind operations.Kind
	fn   func(ctx context.Context, id uint64) (string, error)
}

// Service imports, analyses, and transcodes track files.
type Service struct {
	tx       TxRunner
	store    Store
	files    Files
	prober   Prober
	ff       Encoder
	ops      Operations
	settings SettingsFunc
	log      *slog.Logger
	now      func() time.Time

	// running is true from the moment an operation is accepted until the
	// worker has finished it; it keeps the queue at one job.
	running atomic.Bool
	queue   chan job
	// decodes holds a token per waveform decode running (waveformDecodes).
	decodes chan struct{}
}

// NewService returns the service. log is the root logger; now is the
// clock.
func NewService(tx TxRunner, store Store, files Files, prober Prober, ff Encoder, ops Operations,
	settings SettingsFunc, log *slog.Logger, now func() time.Time,
) *Service {
	return &Service{
		tx: tx, store: store, files: files, prober: prober, ff: ff, ops: ops, settings: settings,
		log: logging.For(log, logging.ComponentMedia), now: now,
		queue: make(chan job, 1), decodes: make(chan struct{}, waveformDecodes),
	}
}

// Run is the worker: it runs the accepted operations one by one until ctx
// is cancelled. It is the only consumer of the queue; serve owns it in its
// errgroup. An operation still queued at shutdown fails.
func (s *Service) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			select {
			case j := <-s.queue:
				_ = s.ops.Execute(ctx, j.id, logging.ComponentMedia, func(context.Context) (string, error) {
					return "", fmt.Errorf("%s cancelled: %w", j.kind, ctx.Err())
				})
			default:
			}
			return nil
		case j := <-s.queue:
			// Execute stores, logs, and publishes the outcome; an error here
			// means the outcome itself could not be stored, and it is logged.
			_ = s.ops.Execute(ctx, j.id, logging.ComponentMedia, func(ctx context.Context) (string, error) {
				return j.fn(ctx, j.id)
			})
			s.running.Store(false)
		}
	}
}

// start accepts an operation for the worker, or refuses it with ErrRunning
// while another one is accepted. ctx is the request context: it only
// bounds storing the operation.
func (s *Service) start(ctx context.Context, kind operations.Kind, fn func(ctx context.Context, id uint64) (string, error)) (uint64, error) {
	if !s.running.CompareAndSwap(false, true) {
		return 0, ErrRunning
	}
	id, err := s.ops.Start(ctx, kind)
	if err != nil {
		s.running.Store(false)
		return 0, err
	}
	// The queue holds one job, and running admits one at a time: the send
	// never waits.
	s.queue <- job{id: id, kind: kind, fn: fn}
	return id, nil
}

// toolsAvailable refuses an operation that needs ffmpeg and ffprobe when
// they are missing (a development host).
func (s *Service) toolsAvailable() error {
	if !s.prober.Available() || !s.ff.Available() {
		return ErrToolsUnavailable
	}
	return nil
}

// readSettings reads the settings with a deadline.
func (s *Service) readSettings(ctx context.Context) (Settings, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	set, err := s.settings(ctx)
	if err != nil {
		return Settings{}, fmt.Errorf("read import settings: %w", err)
	}
	return set, nil
}

// tally counts the outcomes of a batch; the workers of the batch share it.
type tally struct {
	mu     sync.Mutex
	counts map[string]int
	failed []string
	// cueReset are the tracks whose cue did not fit the new file and was
	// reset (docs/phase-22-plan.md decision 7).
	cueReset []string
	done     int
}

func newTally() *tally { return &tally{counts: map[string]int{}} }

// add counts one outcome and returns how many items are done. A failed
// item's name is kept for the summary.
func (t *tally) add(outcome, name string, failed bool) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.counts[outcome]++
	if failed && len(t.failed) < maxFailedNames {
		t.failed = append(t.failed, name)
	}
	t.done++
	return t.done
}

// resetCue records a track whose cue was reset; the summary names it.
func (t *tally) resetCue(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cueReset = append(t.cueReset, name)
}

// summary words the counts in the order of outcomes, skipping zeros, and
// names the first failed items and the tracks whose cue was reset.
func (t *tally) summary(outcomes ...string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := ""
	for _, o := range outcomes {
		n := t.counts[o]
		if n == 0 {
			continue
		}
		if out != "" {
			out += ", "
		}
		out += fmt.Sprintf("%s %d", o, n)
	}
	if out == "" {
		out = "nothing to do"
	}
	if len(t.failed) > 0 {
		out += fmt.Sprintf("; failed: %v (see the log)", t.failed)
	}
	if len(t.cueReset) > 0 {
		names := t.cueReset[:min(len(t.cueReset), maxFailedNames)]
		out += fmt.Sprintf("; cue_reset %d: %v", len(t.cueReset), names)
	}
	return out
}
