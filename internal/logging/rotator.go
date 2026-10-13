package logging

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// FileNamePattern matches daily log file names: app-YYYY-MM-DD.log (UTC date).
// The logs API accepts only names that match it (§10.1).
var FileNamePattern = regexp.MustCompile(`^app-(\d{4}-\d{2}-\d{2})\.log$`)

const dateLayout = "2006-01-02"

// DailyRotator is an io.Writer that writes to dir/app-YYYY-MM-DD.log, where
// the date is the UTC date of the write. It switches files at UTC midnight
// and, on every switch, deletes log files older than the retention period.
type DailyRotator struct {
	dir string
	now func() time.Time

	mu            sync.Mutex // guards file, date, and retentionDays
	file          *os.File
	date          string
	retentionDays int
}

// NewDailyRotator creates dir if needed. now is the clock (time.Now in
// production).
func NewDailyRotator(dir string, retentionDays int, now func() time.Time) (*DailyRotator, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create log directory %s: %w", dir, err)
	}
	return &DailyRotator{dir: dir, retentionDays: retentionDays, now: now}, nil
}

// Write appends p to the file of the current UTC date.
func (r *DailyRotator) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	today := r.now().UTC().Format(dateLayout)
	if r.file == nil || today != r.date {
		if err := r.rotate(today); err != nil {
			return 0, err
		}
	}
	n, err := r.file.Write(p)
	if err != nil {
		return n, fmt.Errorf("write log file: %w", err)
	}
	return n, nil
}

// SetRetentionDays changes the retention period (admin setting
// retention.log_days) and deletes the files it expires at once, instead of
// at the next switch.
func (r *DailyRotator) SetRetentionDays(days int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retentionDays = days
	if r.date == "" {
		// Nothing written yet: the first switch cleans up.
		return nil
	}
	return r.cleanup(r.date)
}

// Close closes the current file.
func (r *DailyRotator) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}

func (r *DailyRotator) rotate(today string) error {
	if r.file != nil {
		// The old file is complete; a close error cannot be reported anywhere
		// more useful than the new file.
		_ = r.file.Close()
		r.file = nil
	}
	path := filepath.Join(r.dir, "app-"+today+".log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640) //nolint:gosec // path is built from a date, not from input
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	r.file = f
	r.date = today
	// Cleanup failures must not stop logging.
	_ = r.cleanup(today)
	return nil
}

// cleanup deletes log files whose date is more than retentionDays before
// today.
func (r *DailyRotator) cleanup(today string) error {
	day, err := time.Parse(dateLayout, today)
	if err != nil {
		return fmt.Errorf("parse date: %w", err)
	}
	cutoff := day.AddDate(0, 0, -r.retentionDays)

	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return fmt.Errorf("read log directory: %w", err)
	}
	var errs []error
	for _, e := range entries {
		m := FileNamePattern.FindStringSubmatch(e.Name())
		if m == nil || e.IsDir() {
			continue
		}
		fileDay, err := time.Parse(dateLayout, m[1])
		if err != nil || !fileDay.Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(r.dir, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
