package library

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// MaxTitleLength is the longest title, in characters, after cleaning
// (docs/phase-21-plan.md §2, decision 5).
const MaxTitleLength = 500

// Bulk rename limits (decision 9).
const (
	// MaxRenameTracks bounds one bulk rename, like a bulk delete.
	MaxRenameTracks = 1000
	// MaxRenameText bounds the find and the replace text.
	MaxRenameText = 200
)

// Refusals of a bulk rename, besides ErrTrackNotFound.
var (
	// ErrTitleEmpty: the new title would be empty.
	ErrTitleEmpty = errors.New("the new title would be empty")
	// ErrTitleTooLong: the new title would be longer than MaxTitleLength;
	// it is refused, not cut.
	ErrTitleTooLong = errors.New("the new title would be too long")
)

// CleanTitle applies the title rule the way an import does: control
// characters are removed (one that is white space, such as a tab, separates
// words like a space), runs of white space become one space, and the ends
// are trimmed. Invalid UTF-8 is dropped.
func CleanTitle(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && !unicode.IsSpace(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	return strings.Join(strings.Fields(s), " ")
}

// hasControl reports whether s contains a control character (a line break,
// a tab, ...), which the API refuses in a title instead of removing it.
func hasControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}

// checkTitle applies the title rule to a title entered on the API: control
// characters are refused, the rest is cleaned. It adds the broken rule of
// field to errs and returns the clean title.
func checkTitle(errs *validation.Error, field, title string) string {
	if hasControl(title) {
		errs.Add(field, validation.InvalidCharacters, "must not contain control characters (line breaks, tabs)")
		return title
	}
	title = CleanTitle(title)
	switch {
	case title == "":
		errs.Add(field, validation.Required, "must not be blank")
	case utf8.RuneCountInString(title) > MaxTitleLength:
		errs.Add(field, validation.Length, "must be at most %d characters", MaxTitleLength)
	}
	return title
}

// RenameRequest is a bulk find-and-replace on titles. The match is literal:
// no pattern characters.
type RenameRequest struct {
	// TrackIDs lists 1 to MaxRenameTracks tracks, each once.
	TrackIDs []uint64
	// Find is 1 to MaxRenameText characters, not only white space.
	Find string
	// Replace is 0 to MaxRenameText characters, without control
	// characters.
	Replace   string
	MatchCase bool
	// DryRun computes the result and writes nothing.
	DryRun bool
}

// TitleChange is a track whose title a rename changes.
type TitleChange struct {
	TrackID  uint64
	OldTitle string
	NewTitle string
}

// RenameRefusal is a track a rename leaves alone. Err is ErrTrackNotFound,
// ErrTitleEmpty, or ErrTitleTooLong.
type RenameRefusal struct {
	TrackID uint64
	Err     error
}

// RenameResult is the outcome of a rename, each list in request order.
type RenameResult struct {
	Changed []TitleChange
	// Unchanged: no occurrence of the text, or the same title after the
	// replacement.
	Unchanged []uint64
	Refused   []RenameRefusal
}

// RenameTracks replaces every occurrence of Find in the titles of the
// tracks, then applies the title rule, so removing a word leaves no double
// space. A dry run reads the titles and writes nothing; otherwise the
// titles are read again under a row lock in one transaction with the
// write, so the result is what was written even when a title changed
// since the preview. A changed title clears the reviewed mark.
func (s *Service) RenameTracks(ctx context.Context, req RenameRequest) (RenameResult, error) {
	match, err := checkRename(req)
	if err != nil {
		return RenameResult{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	if req.DryRun {
		titles, terr := s.tracks.Titles(ctx, req.TrackIDs, false)
		if terr != nil {
			return RenameResult{}, fmt.Errorf("read titles: %w", terr)
		}
		return rename(req.TrackIDs, titles, match), nil
	}
	var res RenameResult
	err = s.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		ts := s.tracks.WithTx(tx)
		titles, terr := ts.Titles(ctx, req.TrackIDs, true)
		if terr != nil {
			return fmt.Errorf("read titles: %w", terr)
		}
		res = rename(req.TrackIDs, titles, match)
		if len(res.Changed) == 0 {
			return nil
		}
		changed := make(map[uint64]string, len(res.Changed))
		for _, c := range res.Changed {
			changed[c.TrackID] = c.NewTitle
		}
		if serr := ts.SetTitles(ctx, changed); serr != nil {
			return fmt.Errorf("write titles: %w", serr)
		}
		return nil
	})
	if err != nil {
		return RenameResult{}, err
	}
	s.log.InfoContext(ctx, "tracks renamed",
		"requested", len(req.TrackIDs), "changed", len(res.Changed), "unchanged", len(res.Unchanged), "refused", len(res.Refused))
	return res, nil
}

// replacer replaces every occurrence of the find text in a title; false
// when there is none.
type replacer func(title string) (string, bool)

// checkRename refuses a request the handler's schema would refuse too (it
// guards other callers) and the texts the schema cannot check, and returns
// the replacement.
func checkRename(req RenameRequest) (replacer, error) {
	errs := &validation.Error{}
	if err := checkTrackIDs(req.TrackIDs, MaxRenameTracks); err != nil {
		return nil, err
	}
	switch n := utf8.RuneCountInString(req.Find); {
	case strings.TrimSpace(req.Find) == "":
		errs.Add("find", validation.Required, "must not be blank")
	case n > MaxRenameText:
		errs.Add("find", validation.Length, "must be at most %d characters", MaxRenameText)
	case hasControl(req.Find):
		errs.Add("find", validation.InvalidCharacters, "must not contain control characters (line breaks, tabs)")
	}
	switch {
	case utf8.RuneCountInString(req.Replace) > MaxRenameText:
		errs.Add("replace", validation.Length, "must be at most %d characters", MaxRenameText)
	case hasControl(req.Replace):
		errs.Add("replace", validation.InvalidCharacters, "must not contain control characters (line breaks, tabs)")
	}
	if err := errs.Err(); err != nil {
		return nil, err
	}
	if req.MatchCase {
		return func(title string) (string, bool) {
			return strings.ReplaceAll(title, req.Find, req.Replace), strings.Contains(title, req.Find)
		}, nil
	}
	// A literal pattern: QuoteMeta escapes every pattern character, and
	// (?i) folds case by Unicode rules (Cyrillic and Armenian too).
	re := regexp.MustCompile("(?i)" + regexp.QuoteMeta(req.Find))
	return func(title string) (string, bool) {
		return re.ReplaceAllLiteralString(title, req.Replace), re.MatchString(title)
	}, nil
}

// rename computes the result for the titles of the tracks that exist.
func rename(ids []uint64, titles map[uint64]string, replace replacer) RenameResult {
	res := RenameResult{Changed: []TitleChange{}, Unchanged: []uint64{}, Refused: []RenameRefusal{}}
	for _, id := range ids {
		old, ok := titles[id]
		if !ok {
			res.Refused = append(res.Refused, RenameRefusal{TrackID: id, Err: ErrTrackNotFound})
			continue
		}
		replaced, found := replace(old)
		if !found {
			res.Unchanged = append(res.Unchanged, id)
			continue
		}
		title := CleanTitle(replaced)
		switch {
		case title == "":
			res.Refused = append(res.Refused, RenameRefusal{TrackID: id, Err: ErrTitleEmpty})
		case utf8.RuneCountInString(title) > MaxTitleLength:
			res.Refused = append(res.Refused, RenameRefusal{TrackID: id, Err: ErrTitleTooLong})
		case title == old:
			res.Unchanged = append(res.Unchanged, id)
		default:
			res.Changed = append(res.Changed, TitleChange{TrackID: id, OldTitle: old, NewTitle: title})
		}
	}
	return res
}
