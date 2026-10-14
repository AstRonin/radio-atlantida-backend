package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/media"
	"github.com/AstRonin/radio-atlantida-backend/internal/operations"
)

var testNow = time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)

// fakeFiles is a media root in memory: path → content.
type fakeFiles struct {
	mu      sync.Mutex
	files   map[string]string
	failOps map[string]error // "move:<src>", "commit:<dst>"
}

func newFakeFiles() *fakeFiles {
	return &fakeFiles{files: map[string]string{}, failOps: map[string]error{}}
}

func (f *fakeFiles) put(p, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[p] = content
}

func (f *fakeFiles) get(p string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.files[p]
	return c, ok
}

func (f *fakeFiles) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.files))
}

func (f *fakeFiles) Walk(_ context.Context, dir string, limit int) ([]media.File, bool, error) {
	var out []media.File
	for _, p := range f.paths() {
		if !strings.HasPrefix(p, dir+"/") || strings.HasPrefix(path.Base(p), ".") {
			continue
		}
		if len(out) == limit {
			return out, true, nil
		}
		c, _ := f.get(p)
		out = append(out, media.File{Path: p, Size: int64(len(c)), ModTime: testNow})
	}
	return out, false, nil
}

func (f *fakeFiles) Checksum(_ context.Context, rel string) (string, error) {
	c, ok := f.get(rel)
	if !ok {
		return "", media.ErrFileNotFound
	}
	sum := sha256.Sum256([]byte(c))
	return hex.EncodeToString(sum[:]), nil
}

func (f *fakeFiles) FreeName(rel string) (string, error) {
	ext := path.Ext(rel)
	candidate := rel
	for n := 2; ; n++ {
		if _, ok := f.get(candidate); !ok {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(rel, ext), n, ext)
	}
}

func (f *fakeFiles) Move(src, dst string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOps["move:"+src]; err != nil {
		return err
	}
	if _, ok := f.files[dst]; ok {
		return media.ErrExists
	}
	c, ok := f.files[src]
	if !ok {
		return media.ErrFileNotFound
	}
	delete(f.files, src)
	f.files[dst] = c
	return nil
}

func (f *fakeFiles) Commit(tmp, dst string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failOps["commit:"+dst]; err != nil {
		return err
	}
	c, ok := f.files[tmp]
	if !ok {
		return media.ErrFileNotFound
	}
	delete(f.files, tmp)
	f.files[dst] = c
	return nil
}

func (f *fakeFiles) Remove(rel string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, rel)
	return nil
}

// fakeTools probe, measure, and encode by file content: what a file is
// depends only on its bytes, so it survives moves.
type fakeTools struct {
	files     *fakeFiles
	mu        sync.Mutex
	infos     map[string]media.Info     // content → info
	loudness  map[string]media.Loudness // content → loudness; missing: silent
	encodes   []encodeCall
	encodeErr error
	missing   bool // Available reports false
}

type encodeCall struct {
	src, dst string
	enc      media.Encoding
	norm     *media.Normalize
}

func newFakeTools(files *fakeFiles) *fakeTools {
	return &fakeTools{files: files, infos: map[string]media.Info{}, loudness: map[string]media.Loudness{}}
}

// audio puts a file at p whose probe returns info and whose loudness is l
// (nil: silent).
func (ft *fakeTools) audio(p, content string, info media.Info, l *media.Loudness) {
	ft.files.put(p, content)
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.infos[content] = info
	if l != nil {
		ft.loudness[content] = *l
	}
}

func (ft *fakeTools) Available() bool { return !ft.missing }

func (ft *fakeTools) Probe(_ context.Context, rel string) (media.Info, error) {
	c, ok := ft.files.get(rel)
	if !ok {
		return media.Info{}, media.ErrFileNotFound
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	info, ok := ft.infos[c]
	if !ok {
		return media.Info{}, errors.New("no audio stream")
	}
	return info, nil
}

func (ft *fakeTools) Measure(_ context.Context, rel string) (media.Loudness, error) {
	c, ok := ft.files.get(rel)
	if !ok {
		return media.Loudness{}, media.ErrFileNotFound
	}
	ft.mu.Lock()
	defer ft.mu.Unlock()
	l, ok := ft.loudness[c]
	if !ok {
		return media.Loudness{}, media.ErrSilent
	}
	return l, nil
}

// Encode writes a file whose probe reports the encoding and whose loudness
// is the target when normalized.
func (ft *fakeTools) Encode(_ context.Context, src, dst string, enc media.Encoding, norm *media.Normalize) error {
	ft.mu.Lock()
	ft.encodes = append(ft.encodes, encodeCall{src: src, dst: dst, enc: enc, norm: norm})
	err := ft.encodeErr
	ft.mu.Unlock()
	if err != nil {
		return err
	}
	c, ok := ft.files.get(src)
	if !ok {
		return media.ErrFileNotFound
	}
	out := c + "|" + enc.Output.Encoder
	ft.mu.Lock()
	info := ft.infos[c]
	info.Format = new(formatOfMuxer(enc.Output.Muxer))
	info.Codec = new(strings.TrimPrefix(enc.Output.Encoder, "lib"))
	if *info.Codec == "mp3lame" {
		info.Codec = new("mp3")
	}
	info.Bitrate = new(enc.BitrateKbps * 1000)
	info.SampleRate, info.Channels = new(enc.SampleRate), new(enc.Channels)
	ft.infos[out] = info
	if l, ok := ft.loudness[c]; ok {
		if norm != nil {
			l.IntegratedLUFS, l.TruePeakDBTP = norm.TargetLUFS, norm.TruePeakDBTP-0.5
		}
		ft.loudness[out] = l
	}
	ft.mu.Unlock()
	ft.files.put(dst, out)
	return nil
}

func formatOfMuxer(m string) string {
	switch m {
	case "ipod":
		return media.FormatM4A
	case "ogg":
		return media.FormatOgg
	default:
		return m
	}
}

// fakeStore keeps tracks, artists, types, and playlists in memory.
type fakeStore struct {
	mu        sync.Mutex
	types     map[uint64]bool
	playlists map[uint64]bool
	artists   map[uint64]string
	tracks    map[uint64]*storedTrack
	busy      map[uint64]bool
	nextID    uint64
	createErr error
}

type storedTrack struct {
	Track
	NewTrack
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		types: map[uint64]bool{1: true}, playlists: map[uint64]bool{10: true, 11: true},
		artists: map[uint64]string{}, tracks: map[uint64]*storedTrack{}, busy: map[uint64]bool{}, nextID: 100,
	}
}

func (s *fakeStore) WithTx(db.Tx) Store { return s }

func (s *fakeStore) TypeExists(_ context.Context, id uint64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.types[id], nil
}

func (s *fakeStore) MissingPlaylists(_ context.Context, ids []uint64) ([]uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []uint64
	for _, id := range ids {
		if !s.playlists[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

func (s *fakeStore) TrackByChecksum(_ context.Context, sum string) (uint64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range slices.Sorted(maps.Keys(s.tracks)) {
		if c := s.tracks[id].Checksum; c != nil && *c == sum {
			return id, true, nil
		}
	}
	return 0, false, nil
}

func (s *fakeStore) TrackByPath(_ context.Context, p string) (uint64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, t := range s.tracks {
		if t.FilePath == p {
			return id, true, nil
		}
	}
	return 0, false, nil
}

func (s *fakeStore) EnsureArtist(_ context.Context, name string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, n := range s.artists {
		if strings.EqualFold(n, name) {
			return id, nil
		}
	}
	s.nextID++
	s.artists[s.nextID] = name
	return s.nextID, nil
}

func (s *fakeStore) CreateTrack(_ context.Context, t NewTrack) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return 0, s.createErr
	}
	s.nextID++
	s.tracks[s.nextID] = &storedTrack{
		Track: Track{
			ID: s.nextID, FilePath: t.Analysis.FilePath, Checksum: new(t.Analysis.Checksum),
			Measurements: t.Analysis.Measurements, Issues: t.Analysis.Issues, AnalyzedAt: new(t.Analysis.AnalyzedAt),
		},
		NewTrack: t,
	}
	return s.nextID, nil
}

// add stores a track as the library has it.
func (s *fakeStore) add(t Track) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tracks[t.ID] = &storedTrack{Track: t}
}

func (s *fakeStore) track(id uint64) Track {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tracks[id].Track
}

func (s *fakeStore) created() []storedTrack {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []storedTrack
	for _, id := range slices.Sorted(maps.Keys(s.tracks)) {
		if s.tracks[id].Title != "" {
			out = append(out, *s.tracks[id])
		}
	}
	return out
}

func (s *fakeStore) Tracks(_ context.Context, ids []uint64) ([]Track, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Track
	for _, id := range slices.Sorted(slices.Values(ids)) {
		if t, ok := s.tracks[id]; ok {
			out = append(out, t.Track)
		}
	}
	return out, nil
}

func (s *fakeStore) Unanalysed(_ context.Context, limit int) ([]Track, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Track
	for _, id := range slices.Sorted(maps.Keys(s.tracks)) {
		if s.tracks[id].AnalyzedAt == nil && len(out) < limit {
			out = append(out, s.tracks[id].Track)
		}
	}
	return out, nil
}

func (s *fakeStore) SaveAnalysis(_ context.Context, id uint64, a Analysis) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tracks[id]
	if !ok {
		return library.ErrTrackNotFound
	}
	t.FilePath, t.Checksum, t.Measurements, t.Issues = a.FilePath, new(a.Checksum), a.Measurements, a.Issues
	t.AnalyzedAt = new(a.AnalyzedAt)
	return nil
}

func (s *fakeStore) MarkMissing(_ context.Context, id uint64, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.tracks[id]
	t.Issues, t.AnalyzedAt = []library.QualityIssue{library.IssueFileMissing}, new(at)
	return nil
}

func (s *fakeStore) Busy(_ context.Context, ids []uint64, _, _ time.Time) ([]uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []uint64
	for _, id := range ids {
		if s.busy[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

type fakeTx struct{}

func (fakeTx) WithTx(ctx context.Context, fn func(ctx context.Context, tx db.Tx) error) error {
	return fn(ctx, db.Tx{})
}

// fakeOps runs nothing itself: Execute calls fn and keeps the outcome.
type fakeOps struct {
	mu       sync.Mutex
	nextID   uint64
	started  []operations.Kind
	summary  map[uint64]string
	failure  map[uint64]error
	progress int
}

func newFakeOps() *fakeOps {
	return &fakeOps{summary: map[uint64]string{}, failure: map[uint64]error{}}
}

func (o *fakeOps) Start(_ context.Context, kind operations.Kind) (uint64, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.nextID++
	o.started = append(o.started, kind)
	return o.nextID, nil
}

func (o *fakeOps) Execute(ctx context.Context, id uint64, _ logging.Component, fn operations.Func) error {
	summary, err := fn(ctx)
	o.mu.Lock()
	defer o.mu.Unlock()
	o.summary[id], o.failure[id] = summary, err
	return nil
}

func (o *fakeOps) Progress(uint64, operations.Kind, int, int, string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.progress++
}

type fixture struct {
	svc   *Service
	files *fakeFiles
	tools *fakeTools
	store *fakeStore
	ops   *fakeOps
	set   Settings
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{files: newFakeFiles(), store: newFakeStore(), ops: newFakeOps()}
	f.tools = newFakeTools(f.files)
	f.set = Settings{InboxDir: "incoming", LibraryDir: "library", Standard: testStandard(), BusyAhead: 10 * time.Minute}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.svc = NewService(fakeTx{}, f.store, f.files, f.tools, f.tools, f.ops,
		func(context.Context) (Settings, error) { return f.set, nil }, log, func() time.Time { return testNow })
	return f
}

// runQueued runs the operation the service accepted, as Run would, and
// returns its outcome.
func (f *fixture) runQueued(t *testing.T) (string, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = f.svc.Run(ctx)
	}()
	require.Eventually(t, func() bool { return !f.svc.running.Load() }, 5*time.Second, time.Millisecond)
	cancel()
	<-done
	f.ops.mu.Lock()
	defer f.ops.mu.Unlock()
	return f.ops.summary[f.ops.nextID], f.ops.failure[f.ops.nextID]
}

// mp3Info is the probe of a file that meets testStandard.
func mp3Info(title, artist string) media.Info {
	return media.Info{
		DurationMS: new(180000), Format: new(media.FormatMP3), Codec: new("mp3"),
		Bitrate: new(320000), SampleRate: new(44100), Channels: new(2), Title: title, Artist: artist,
	}
}

func loud(lufs, tp float64) *media.Loudness {
	return &media.Loudness{IntegratedLUFS: lufs, TruePeakDBTP: tp, RangeLU: 6, ThresholdLUFS: lufs - 10}
}

func sha(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// contextCancelled returns a context that is already cancelled.
func contextCancelled(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx, cancel
}
