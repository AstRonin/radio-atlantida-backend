package logging

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"slices"
	"time"
)

// ErrFileNotFound is returned for a log file name that is invalid or does
// not exist.
var ErrFileNotFound = errors.New("log file not found")

// FileInfo describes one daily log file.
type FileInfo struct {
	Name string
	// Date is the UTC date of the file.
	Date time.Time
	Size int64
}

// Entry is one parsed log line. Attrs holds every field other than the
// standard ones.
type Entry struct {
	Time      time.Time
	Level     string
	Message   string
	Component string
	RequestID string
	Error     string
	Attrs     map[string]any
}

// Filter selects log entries.
type Filter struct {
	// MinLevel keeps entries at this level or above. Nil keeps all.
	MinLevel *slog.Level
	// Component keeps entries of one component. Empty keeps all.
	Component string
	// Offset skips the newest matching entries; Limit caps the result.
	Offset, Limit int
}

// Page is a page of entries, newest first.
type Page struct {
	Entries []Entry
	// Total is the number of entries that match the filter.
	Total int
}

// checkEvery is how many lines are read between cancellation checks.
const checkEvery = 1024

// FileReader reads the daily log files in the log root for the panel
// (§10.1). Only names matching [FileNamePattern] are opened, and only inside
// the root, so a request cannot reach other files.
type FileReader struct {
	root string
}

// NewFileReader reads files in logRoot.
func NewFileReader(logRoot string) *FileReader { return &FileReader{root: logRoot} }

// List returns the log files, newest first.
func (r *FileReader) List() ([]FileInfo, error) {
	dirEntries, err := os.ReadDir(r.root)
	if err != nil {
		return nil, fmt.Errorf("list log files: %w", err)
	}
	var files []FileInfo
	for _, e := range dirEntries {
		m := FileNamePattern.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		date, err := time.Parse(dateLayout, m[1])
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // deleted by rotation meanwhile
		}
		files = append(files, FileInfo{Name: e.Name(), Date: date, Size: info.Size()})
	}
	slices.SortFunc(files, func(a, b FileInfo) int { return b.Date.Compare(a.Date) })
	return files, nil
}

// Read returns the entries of one file that match f, newest first. Lines
// that are not JSON (for example a line cut by a crash) are skipped.
func (r *FileReader) Read(ctx context.Context, name string, f Filter) (Page, error) {
	if !FileNamePattern.MatchString(name) {
		return Page{}, ErrFileNotFound
	}
	root, err := os.OpenRoot(r.root)
	if err != nil {
		return Page{}, fmt.Errorf("open log root: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return Page{}, ErrFileNotFound
	}
	if err != nil {
		return Page{}, fmt.Errorf("open log file %s: %w", name, err)
	}
	defer func() { _ = file.Close() }()

	// Keep only the newest Offset+Limit matches: the file is oldest first.
	keep := newRing(f.Offset + f.Limit)
	total := 0
	br := bufio.NewReader(file)
	for n := 0; ; n++ {
		if n%checkEvery == 0 {
			if err := ctx.Err(); err != nil {
				return Page{}, err
			}
		}
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if e, ok := parseEntry(line); ok && f.matches(e) {
				total++
				keep.push(e)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Page{}, fmt.Errorf("read log file %s: %w", name, err)
		}
	}

	newest := keep.newestFirst()
	if f.Offset >= len(newest) {
		return Page{Entries: []Entry{}, Total: total}, nil
	}
	return Page{Entries: newest[f.Offset:], Total: total}, nil
}

func (f Filter) matches(e Entry) bool {
	if f.Component != "" && e.Component != f.Component {
		return false
	}
	if f.MinLevel != nil {
		var l slog.Level
		if l.UnmarshalText([]byte(e.Level)) != nil || l < *f.MinLevel {
			return false
		}
	}
	return true
}

func parseEntry(line []byte) (Entry, bool) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return Entry{}, false
	}
	str := func(key string) string {
		v, _ := raw[key].(string)
		delete(raw, key)
		return v
	}
	e := Entry{
		Level:     str(slog.LevelKey),
		Message:   str(slog.MessageKey),
		Component: str(KeyComponent),
		RequestID: str(KeyRequestID),
		Error:     str(KeyError),
	}
	t, err := time.Parse(time.RFC3339Nano, str(slog.TimeKey))
	if err != nil {
		return Entry{}, false
	}
	e.Time = t.UTC()
	if len(raw) > 0 {
		e.Attrs = raw
	}
	return e, true
}

// ring keeps the last n pushed entries.
type ring struct {
	buf  []Entry
	next int
	full bool
}

func newRing(n int) *ring { return &ring{buf: make([]Entry, max(n, 0))} }

func (r *ring) push(e Entry) {
	if len(r.buf) == 0 {
		return
	}
	r.buf[r.next] = e
	r.next = (r.next + 1) % len(r.buf)
	if r.next == 0 {
		r.full = true
	}
}

func (r *ring) newestFirst() []Entry {
	var ordered []Entry
	if r.full {
		ordered = append(slices.Clone(r.buf[r.next:]), r.buf[:r.next]...)
	} else {
		ordered = slices.Clone(r.buf[:r.next])
	}
	slices.Reverse(ordered)
	return ordered
}

// LevelNames are the level names written by the logger, lowest first.
func LevelNames() []string {
	return []string{
		slog.LevelDebug.String(), slog.LevelInfo.String(), slog.LevelWarn.String(), slog.LevelError.String(),
	}
}

// Components lists the allowed components (§10.1).
func Components() []Component {
	return []Component{
		ComponentAPI, ComponentDB, ComponentRotation, ComponentLiquidsoap,
		ComponentIcecast, ComponentWebRTC, ComponentRetention, ComponentOutage, ComponentReconcile, ComponentMedia,
	}
}
