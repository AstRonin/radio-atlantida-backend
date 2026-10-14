package clocks

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// Create saves a new clock and returns it with the dry check. A short
// clock is saved incomplete; the panel then asks for the filler (§6.0).
func (s *Service) Create(ctx context.Context, in Input) (Result, error) {
	c, err := s.prepare(ctx, in)
	if err != nil {
		return Result{}, err
	}
	arr, err := Arrange(c.Elements, nil)
	if err != nil {
		return Result{}, err
	}
	c.Elements = arr.Elements
	c.Status = arr.Layout.Status(c.Elements)

	wctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	var id uint64
	err = s.tx.WithTx(wctx, func(ctx context.Context, tx db.Tx) (txErr error) {
		id, txErr = s.store.WithTx(tx).Create(ctx, c)
		return txErr
	})
	if err != nil {
		return Result{}, asFieldError(err)
	}
	return s.result(ctx, id, nil)
}

// Update replaces a clock and returns it with the dry check. Elements are
// matched by ID; a new fixed element that overlaps an existing one replaces
// it (§7.2). A refused save changes nothing.
func (s *Service) Update(ctx context.Context, id uint64, in Input) (Result, error) {
	c, err := s.prepare(ctx, in)
	if err != nil {
		return Result{}, err
	}
	c.ID = id
	replaced, err := s.update(ctx, c)
	if err != nil {
		return Result{}, asFieldError(err)
	}
	return s.result(ctx, id, replaced)
}

// update arranges the clock against its stored elements and saves it, in
// one transaction under the clock's lock.
func (s *Service) update(ctx context.Context, c Clock) ([]Element, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	var replaced []Element
	err := s.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		st := s.store.WithTx(tx)
		if err := st.Lock(ctx, c.ID); err != nil {
			return err
		}
		stored, err := st.Get(ctx, c.ID)
		if err != nil {
			return err
		}
		existing := map[uint64]bool{}
		for _, e := range stored.Elements {
			existing[e.ID] = true
		}
		arr, err := Arrange(c.Elements, existing)
		if err != nil {
			return err
		}
		replaced = arr.Replaced
		c.Elements = arr.Elements
		c.Status = arr.Layout.Status(c.Elements)
		return st.Update(ctx, c)
	})
	return replaced, err
}

// SaveWithFiller stores the filler chosen in the dialog for each short
// segment, on the segment's holder element, and the pool if given. Every
// short segment must have a filler afterwards, so the clock is complete.
func (s *Service) SaveWithFiller(ctx context.Context, id uint64, choice FillerChoice) (Result, error) {
	errs := &validation.Error{}
	for i, ch := range choice.Segments {
		path := fmt.Sprintf("segments[%d]", i)
		if ch.Filler.Mode == FillerNone {
			errs.Add(path+".filler_mode", validation.Invalid, "choose a filler mode, or save the clock as incomplete")
			continue
		}
		checkFiller(errs, path, ch.Filler, true)
	}
	if err := errs.Err(); err != nil {
		return Result{}, err
	}
	if err := s.checkChoiceSources(ctx, choice.Segments); err != nil {
		return Result{}, err
	}

	wctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	err := s.tx.WithTx(wctx, func(ctx context.Context, tx db.Tx) error {
		st := s.store.WithTx(tx)
		if err := st.Lock(ctx, id); err != nil {
			return err
		}
		c, err := st.Get(ctx, id)
		if err != nil {
			return err
		}
		l, err := LayoutOf(c.Elements)
		if err != nil {
			return fmt.Errorf("layout of clock %d: %w", id, err)
		}
		if err := applyChoices(c.Elements, l, choice.Segments); err != nil {
			return err
		}
		if uncovered := l.Uncovered(c.Elements); len(uncovered) > 0 {
			return validation.Field("segments", validation.Required, "%s", shortSegments(uncovered))
		}
		if choice.PoolTrackIDs != nil {
			if err := st.SetPool(ctx, id, choice.PoolTrackIDs); err != nil {
				return err
			}
		}
		if err := s.checkPool(ctx, st, c); err != nil {
			return err
		}
		c.Status = l.Status(c.Elements)
		return st.Update(ctx, c)
	})
	if err != nil {
		return Result{}, asFieldError(err)
	}
	return s.result(ctx, id, nil)
}

// SaveIncomplete acknowledges a short clock without a filler choice: the
// clock stays incomplete and shows its warning. Reminders for its hours in
// the planning window are created by rotation (internal/rotation, §7).
func (s *Service) SaveIncomplete(ctx context.Context, id uint64) (Result, error) {
	return s.result(ctx, id, nil)
}

// Delete deletes a clock that no grid slot uses and that has no history.
func (s *Service) Delete(ctx context.Context, id uint64) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	err := s.store.Delete(ctx, id)
	if !errors.Is(err, ErrInUse) {
		return err
	}
	slots, err := s.store.Users(ctx, id, MaxUsers)
	if err != nil {
		return fmt.Errorf("list grid slots of clock %d: %w", id, err)
	}
	refs := make([]Ref, len(slots))
	for i, sl := range slots {
		refs[i] = Ref{Kind: KindGridSlot, ID: sl.ID, Name: slotName(sl)}
	}
	return &InUseError{UsedBy: refs}
}

// Pool returns the filler pool of a clock.
func (s *Service) Pool(ctx context.Context, id uint64) ([]PoolEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	if _, err := s.store.Get(ctx, id); err != nil {
		return nil, err
	}
	return s.store.Pool(ctx, id)
}

// SetPool replaces the unused rows of the filler pool with trackIDs, in
// order. Used rows stay as history (§4).
func (s *Service) SetPool(ctx context.Context, id uint64, trackIDs []uint64) ([]PoolEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	err := s.tx.WithTx(ctx, func(ctx context.Context, tx db.Tx) error {
		st := s.store.WithTx(tx)
		if err := st.Lock(ctx, id); err != nil {
			return err
		}
		return st.SetPool(ctx, id, trackIDs)
	})
	if err != nil {
		return nil, asFieldError(err)
	}
	return s.store.Pool(ctx, id)
}

// prepare checks the clock fields and the references of a save and returns
// the clock with the tolerance defaults applied. The layout is checked by
// Arrange.
func (s *Service) prepare(ctx context.Context, in Input) (Clock, error) {
	errs := &validation.Error{}
	c := Clock{Name: strings.TrimSpace(in.Name), DefaultFiller: in.DefaultFiller}
	if c.Name == "" {
		errs.Add("name", validation.Required, "must not be blank")
	}
	if c.DefaultFiller != nil {
		checkFiller(errs, "", *c.DefaultFiller, false)
	}
	if err := errs.Err(); err != nil {
		return Clock{}, err
	}

	rctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	set, err := s.settings(rctx)
	if err != nil {
		return Clock{}, fmt.Errorf("read clock settings: %w", err)
	}
	c.Elements = make([]Element, len(in.Elements))
	for i, e := range in.Elements {
		c.Elements[i] = Element{
			ID: e.ID, Placement: e.Placement, FixedMinute: e.FixedMinute,
			FixedToleranceMS:    orDefault(e.FixedToleranceMS, set.FixedToleranceMS),
			NominalDurationMS:   e.NominalDurationMS,
			DurationToleranceMS: orDefault(e.DurationToleranceMS, set.DurationToleranceMS),
			Filler:              e.Filler,
			PlaylistIDs:         e.PlaylistIDs,
		}
		if c.Elements[i].Filler.Mode == "" {
			c.Elements[i].Filler.Mode = FillerNone
		}
	}
	if err := s.checkRefs(rctx, c); err != nil {
		return Clock{}, err
	}
	return c, nil
}

func orDefault(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

// checkRefs names the request field of every playlist or artist that does
// not exist, so the panel can mark the element.
func (s *Service) checkRefs(ctx context.Context, c Clock) error {
	var playlists, artists []uint64
	addSource := func(f *Filler) {
		if f == nil || f.SourceID == nil {
			return
		}
		switch f.Mode {
		case FillerPlaylist:
			playlists = append(playlists, *f.SourceID)
		case FillerArtist:
			artists = append(artists, *f.SourceID)
		}
	}
	addSource(c.DefaultFiller)
	for _, e := range c.Elements {
		playlists = append(playlists, e.PlaylistIDs...)
		addSource(&e.Filler)
	}
	missingP, missingA, err := s.missing(ctx, playlists, artists)
	if err != nil {
		return err
	}
	if len(missingP) == 0 && len(missingA) == 0 {
		return nil
	}

	errs := &validation.Error{}
	checkSource := func(field string, f *Filler) {
		if f == nil || f.SourceID == nil {
			return
		}
		if (f.Mode == FillerPlaylist && missingP[*f.SourceID]) || (f.Mode == FillerArtist && missingA[*f.SourceID]) {
			errs.Add(field, validation.UnknownID, "no %s has ID %d", f.Mode, *f.SourceID)
		}
	}
	checkSource("default_filler_source_id", c.DefaultFiller)
	for i, e := range c.Elements {
		for _, p := range e.PlaylistIDs {
			if missingP[p] {
				errs.Add(elementField(i, "playlist_ids"), validation.UnknownID, "no playlist has ID %d", p)
				break
			}
		}
		checkSource(elementField(i, "filler_source_id"), &e.Filler)
	}
	return errs.Err()
}

// checkChoiceSources refuses a filler choice whose playlist or artist does
// not exist.
func (s *Service) checkChoiceSources(ctx context.Context, choices []SegmentChoice) error {
	var playlists, artists []uint64
	for _, ch := range choices {
		switch {
		case ch.Filler.SourceID == nil:
		case ch.Filler.Mode == FillerPlaylist:
			playlists = append(playlists, *ch.Filler.SourceID)
		case ch.Filler.Mode == FillerArtist:
			artists = append(artists, *ch.Filler.SourceID)
		}
	}
	rctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	missingP, missingA, err := s.missing(rctx, playlists, artists)
	if err != nil {
		return err
	}
	errs := &validation.Error{}
	for i, ch := range choices {
		f := ch.Filler
		if f.SourceID != nil && ((f.Mode == FillerPlaylist && missingP[*f.SourceID]) || (f.Mode == FillerArtist && missingA[*f.SourceID])) {
			errs.Add(fmt.Sprintf("segments[%d].filler_source_id", i), validation.UnknownID, "no %s has ID %d", f.Mode, *f.SourceID)
		}
	}
	return errs.Err()
}

func (s *Service) missing(ctx context.Context, playlists, artists []uint64) (map[uint64]bool, map[uint64]bool, error) {
	mp, ma := map[uint64]bool{}, map[uint64]bool{}
	if len(playlists) > 0 {
		ids, err := s.store.MissingPlaylists(ctx, uniq(playlists))
		if err != nil {
			return nil, nil, fmt.Errorf("check playlists: %w", err)
		}
		for _, id := range ids {
			mp[id] = true
		}
	}
	if len(artists) > 0 {
		ids, err := s.store.MissingArtists(ctx, uniq(artists))
		if err != nil {
			return nil, nil, fmt.Errorf("check artists: %w", err)
		}
		for _, id := range ids {
			ma[id] = true
		}
	}
	return mp, ma, nil
}

func uniq(ids []uint64) []uint64 {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}

// applyChoices stores each choice on the holder of its segment. Two choices
// for segments with the same holder must agree.
func applyChoices(elements []Element, l Layout, choices []SegmentChoice) error {
	errs := &validation.Error{}
	chosen := map[int]int{} // holder → choice index
	for i, ch := range choices {
		field := fmt.Sprintf("segments[%d].segment", i)
		if ch.Segment < 0 || ch.Segment >= len(l.Segments) {
			errs.Add(field, validation.Range, "the clock has segments 0 to %d", len(l.Segments)-1)
			continue
		}
		h := l.Segments[ch.Segment].Holder
		if j, ok := chosen[h]; ok {
			if !sameFiller(choices[j].Filler, ch.Filler) {
				errs.Add(field, validation.Invalid,
					"segments %d and %d are filled by the same element, so they need the same filler",
					choices[j].Segment, ch.Segment)
			}
			continue
		}
		chosen[h] = i
		elements[h].Filler = ch.Filler
	}
	return errs.Err()
}

func sameFiller(a, b Filler) bool {
	if a.Mode != b.Mode || (a.SourceID == nil) != (b.SourceID == nil) {
		return false
	}
	return a.SourceID == nil || *a.SourceID == *b.SourceID
}

// checkPool refuses the pool filler mode while the pool has no unused track.
func (s *Service) checkPool(ctx context.Context, st Store, c Clock) error {
	usesPool := slices.ContainsFunc(c.Elements, func(e Element) bool { return e.Filler.Mode == FillerPool })
	if !usesPool {
		return nil
	}
	pool, err := st.Pool(ctx, c.ID)
	if err != nil {
		return fmt.Errorf("read filler pool of clock %d: %w", c.ID, err)
	}
	if !slices.ContainsFunc(pool, func(p PoolEntry) bool { return p.UsedAt == nil }) {
		return validation.Field("pool_track_ids", validation.Required, "the pool filler mode needs at least one unused track in the pool")
	}
	return nil
}

func shortSegments(segs []Segment) string {
	parts := make([]string, len(segs))
	for i, s := range segs {
		parts[i] = fmt.Sprintf("segment %d is short by %d s", s.Index, (s.ShortfallMS+999)/1000)
	}
	return strings.Join(parts, "; ") + ": choose a filler for each, or save the clock as incomplete"
}

// result reads the saved clock and runs the dry check on it.
func (s *Service) result(ctx context.Context, id uint64, replaced []Element) (Result, error) {
	v, err := s.Get(ctx, id)
	if err != nil {
		return Result{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	warnings, err := s.warnings(cctx, v.Clock, v.Layout)
	if err != nil {
		return Result{}, fmt.Errorf("dry check of clock %d: %w", id, err)
	}
	return Result{Clock: v.Clock, Layout: v.Layout, Warnings: warnings, Replaced: replaced}, nil
}

var weekdayNames = [...]string{"", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

// slotName describes a grid slot for a refusal: "Mon 08:00, 2026-10-15 to
// 2026-11-01" (station local).
func slotName(sl SlotRef) string {
	to := "open end"
	if sl.ValidTo != nil {
		to = sl.ValidTo.Format(time.DateOnly)
	}
	day := ""
	if sl.Weekday >= 1 && sl.Weekday <= 7 {
		day = weekdayNames[sl.Weekday]
	}
	return fmt.Sprintf("%s %02d:00, %s to %s", day, sl.Hour, sl.ValidFrom.Format(time.DateOnly), to)
}

// fieldErrors maps store errors to the request field they concern. FK
// errors from a race after checkRefs cannot name the element.
var fieldErrors = []struct {
	err   error
	field string
	rule  validation.Rule
	msg   string
}{
	{ErrNameTaken, "name", validation.Taken, "another clock has this name"},
	{ErrUnknownPlaylist, "elements", validation.UnknownID, "a playlist was deleted while the clock was saved"},
	{ErrUnknownArtist, "elements", validation.UnknownID, "an artist was deleted while the clock was saved"},
	{ErrUnknownTrack, "pool_track_ids", validation.UnknownID, "a track ID does not exist"},
}

func asFieldError(err error) error {
	for _, fe := range fieldErrors {
		if errors.Is(err, fe.err) {
			return validation.Field(fe.field, fe.rule, "%s", fe.msg)
		}
	}
	return err
}
