package outage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/logging"
	"github.com/AstRonin/radio-atlantida-backend/internal/playout"
)

const (
	// maxJournalLine bounds one journal line; a line holds one event of a
	// few hundred bytes.
	maxJournalLine = 64 << 10
	// maxReplayWindow bounds one replay. The plan covers at most 48 hours,
	// so older unaired items cannot exist; the journal is kept 10 days.
	maxReplayWindow = 7 * 24 * time.Hour
)

// Replayer applies a journal line to the play log (*playout.Service).
type Replayer interface {
	Replay(ctx context.Context, e playout.Event) error
}

// Journal reads Liquidsoap's playout journal (§7.1, §8.3):
// journal-YYYY-MM-DD.jsonl files (UTC dates) of one JSON line per event,
// under PLAYOUT_JOURNAL_ROOT, which the backend only reads.
type Journal struct {
	root     string
	replayer Replayer
	log      *slog.Logger // component reconcile
}

// NewJournal returns the journal under root. log is the root logger.
func NewJournal(root string, replayer Replayer, log *slog.Logger) *Journal {
	return &Journal{root: root, replayer: replayer, log: logging.For(log, logging.ComponentReconcile)}
}

// ReplayResult counts what a replay read.
type ReplayResult struct {
	// Lines are the track lines of the window passed to the play log.
	Lines int
	// Bad are the lines that could not be parsed.
	Bad int
}

// journalLine is one line as the script writes it
// (dev/liquidsoap/radio.liq): annotations are strings, empty when absent.
type journalLine struct {
	Event     string    `json:"event"`
	Time      time.Time `json:"time"`
	Source    string    `json:"source"`
	ItemID    string    `json:"item_id"`
	TrackID   string    `json:"track_id"`
	ElapsedMS int       `json:"elapsed_ms"`
}

// Replay passes the track start and end lines written in [from, to] to the
// play log, in journal order. The play log changes are idempotent, so a
// window may be replayed again. A missing file (a day without playout) is
// skipped; lines that cannot be parsed are counted and logged once.
func (j *Journal) Replay(ctx context.Context, from, to time.Time) (ReplayResult, error) {
	from, to = from.UTC(), to.UTC()
	if to.Sub(from) > maxReplayWindow {
		from = to.Add(-maxReplayWindow)
	}
	var res ReplayResult
	root, err := os.OpenRoot(j.root)
	if err != nil {
		return res, fmt.Errorf("open playout journal %s: %w", j.root, err)
	}
	defer func() { _ = root.Close() }()

	for day := truncateDay(from); !day.After(to); day = day.AddDate(0, 0, 1) {
		name := JournalFile(day)
		if err := j.replayFile(ctx, root, name, from, to, &res); err != nil {
			return res, err
		}
	}
	if res.Bad > 0 {
		j.log.Warn("playout journal lines could not be parsed", "count", res.Bad, "from", from, "to", to)
	}
	return res, nil
}

func (j *Journal) replayFile(ctx context.Context, root *os.Root, name string, from, to time.Time, res *ReplayResult) error {
	f, err := root.Open(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open journal file %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), maxJournalLine)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(sc.Bytes()) == 0 {
			continue
		}
		var l journalLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil || l.Time.IsZero() {
			res.Bad++
			continue
		}
		e, ok := l.event()
		if !ok || e.Time.Before(from) || e.Time.After(to) {
			continue
		}
		if err := j.replayer.Replay(ctx, e); err != nil {
			return fmt.Errorf("replay %s line at %s: %w", name, e.Time.Format(time.RFC3339Nano), err)
		}
		res.Lines++
	}
	if err := sc.Err(); err != nil {
		// A line longer than maxJournalLine stops the file: count it and
		// go on with the next file.
		if errors.Is(err, bufio.ErrTooLong) {
			res.Bad++
			return nil
		}
		return fmt.Errorf("read journal file %s: %w", name, err)
	}
	return nil
}

// event returns the track event of the line; false for other lines
// (output state).
func (l journalLine) event() (playout.Event, bool) {
	kind := playout.EventKind(l.Event)
	if kind != playout.EventStart && kind != playout.EventEnd {
		return playout.Event{}, false
	}
	return playout.Event{
		Kind: kind, Time: l.Time.UTC(), Source: l.Source,
		ItemID: parseID(l.ItemID), TrackID: parseID(l.TrackID), ElapsedMS: max(l.ElapsedMS, 0),
	}, true
}

// JournalFile is the name of the journal file of the UTC day of t.
func JournalFile(t time.Time) string {
	return "journal-" + t.UTC().Format(time.DateOnly) + ".jsonl"
}

func truncateDay(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// parseID reads an annotation: empty when absent; a value that is not a
// positive integer counts as absent.
func parseID(s string) *uint64 {
	id, err := strconv.ParseUint(s, 10, 64)
	if err != nil || id == 0 {
		return nil
	}
	return &id
}
