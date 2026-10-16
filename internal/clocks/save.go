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

// Create saves a new clock and returns it with its dry check.
func (s *Service) Create(ctx context.Context, in Input) (Result, error) {
	c, err := s.prepare(ctx, in)
	if err != nil {
		return Result{}, err
	}
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
	return s.result(ctx, id)
}

// Update replaces a clock (its playlists and inserts) and returns it with
// its dry check. A refused save changes nothing.
func (s *Service) Update(ctx context.Context, id uint64, in Input) (Result, error) {
	c, err := s.prepare(ctx, in)
	if err != nil {
		return Result{}, err
	}
	c.ID = id
	wctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	err = s.tx.WithTx(wctx, func(ctx context.Context, tx db.Tx) error {
		st := s.store.WithTx(tx)
		if lockErr := st.Lock(ctx, id); lockErr != nil {
			return lockErr
		}
		return st.Update(ctx, c)
	})
	if err != nil {
		return Result{}, asFieldError(err)
	}
	return s.result(ctx, id)
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

// prepare reads what the checks need and returns the clock of a save.
func (s *Service) prepare(ctx context.Context, in Input) (Clock, error) {
	rctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	set, err := s.settings(rctx)
	if err != nil {
		return Clock{}, fmt.Errorf("read clock settings: %w", err)
	}
	var playlists, tracks []uint64
	for _, p := range in.Playlists {
		playlists = append(playlists, p.PlaylistID)
	}
	for _, ins := range in.Inserts {
		if ins.PlaylistID != nil {
			playlists = append(playlists, *ins.PlaylistID)
		}
		if ins.TrackID != nil {
			tracks = append(tracks, *ins.TrackID)
		}
	}
	own := map[uint64]PlaylistRule{}
	if len(playlists) > 0 {
		if own, err = s.store.PlaylistRules(rctx, uniq(playlists)); err != nil {
			return Clock{}, fmt.Errorf("read playlist rules: %w", err)
		}
	}
	missing := map[uint64]bool{}
	if len(tracks) > 0 {
		ids, err := s.store.MissingTracks(rctx, uniq(tracks))
		if err != nil {
			return Clock{}, fmt.Errorf("check tracks: %w", err)
		}
		for _, id := range ids {
			missing[id] = true
		}
	}
	return Check(in, own, missing, set.FixedToleranceMS)
}

// Check validates a save and returns the clock to store (§7.2). own holds
// the rule of every playlist that exists; missingTracks the track IDs that
// do not. defaultToleranceMS applies to an insert without a tolerance.
// Refusals name the request field (playlists[2].fill_weight,
// inserts[0].offset_ms).
func Check(in Input, own map[uint64]PlaylistRule, missingTracks map[uint64]bool, defaultToleranceMS int) (Clock, error) {
	errs := &validation.Error{}
	c := Clock{Name: strings.TrimSpace(in.Name)}
	if c.Name == "" {
		errs.Add("name", validation.Required, "must not be blank")
	}

	seen := map[uint64]bool{}
	fill, weighted, unweighted := 0, 0, -1
	for i, p := range in.Playlists {
		field := func(name string) string { return fmt.Sprintf("playlists[%d].%s", i, name) }
		cp := ClockPlaylist{PlaylistID: p.PlaylistID, Rule: p.Rule, Count: p.Count, FillWeight: p.FillWeight}
		rule, known := own[p.PlaylistID]
		switch {
		case !known:
			errs.Add(field("playlist_id"), validation.UnknownID, "no playlist has ID %d", p.PlaylistID)
		case seen[p.PlaylistID]:
			errs.Add(field("playlist_id"), validation.Invalid, "the playlist is listed twice")
		}
		seen[p.PlaylistID] = true
		cp.Own = rule
		if p.Rule != nil && !p.Rule.Valid() {
			errs.Add(field("clock_rule"), validation.Invalid, "must be one of fill, spread, start, end, start_end, middle")
			continue
		}
		spreadOverride := p.Rule != nil && *p.Rule == RuleSpread
		switch {
		case spreadOverride && p.Count == nil:
			errs.Add(field("clock_count"), validation.ClockCountRequired, "the spread rule needs the number of tracks")
		case !spreadOverride && p.Count != nil:
			errs.Add(field("clock_count"), validation.ClockCountNotAllowed, "only a spread override of the rule has a count")
		case p.Count != nil && (*p.Count < 1 || *p.Count > MaxSpreadCount):
			errs.Add(field("clock_count"), validation.Range, "must be between 1 and %d", MaxSpreadCount)
		}
		if !known {
			c.Playlists = append(c.Playlists, cp)
			continue
		}
		isFill := cp.EffectiveRule() == RuleFill
		switch {
		case p.FillWeight != nil && !isFill:
			errs.Add(field("fill_weight"), validation.FillWeightNotAllowed, "only a fill playlist has a share")
		case p.FillWeight != nil && (*p.FillWeight < 1 || *p.FillWeight > MaxFillWeight):
			errs.Add(field("fill_weight"), validation.Range, "must be between 1 and %d", MaxFillWeight)
		}
		if isFill {
			fill++
			switch {
			case p.FillWeight != nil:
				weighted++
			case unweighted < 0:
				unweighted = i
			}
		}
		c.Playlists = append(c.Playlists, cp)
	}
	switch {
	case fill == 0:
		errs.Add("playlists", validation.FillPlaylistRequired, "a clock needs at least one playlist whose placement rule is fill")
	case weighted > 0 && unweighted >= 0:
		errs.Add(fmt.Sprintf("playlists[%d].fill_weight", unweighted), validation.FillWeightsPartial,
			"either every fill playlist has a share or none")
	}

	offsets := map[int]bool{}
	for i, ins := range in.Inserts {
		field := func(name string) string { return fmt.Sprintf("inserts[%d].%s", i, name) }
		out := Insert{OffsetMS: ins.OffsetMS, ToleranceMS: defaultToleranceMS, TrackID: ins.TrackID, PlaylistID: ins.PlaylistID}
		if ins.ToleranceMS != nil {
			out.ToleranceMS = *ins.ToleranceMS
		}
		switch {
		case ins.OffsetMS < 0 || ins.OffsetMS > MaxOffsetMS:
			errs.Add(field("offset_ms"), validation.Range, "must be between 0 (00:00) and %d (59:59)", MaxOffsetMS)
		case ins.OffsetMS%1000 != 0:
			errs.Add(field("offset_ms"), validation.Format, "must be whole seconds")
		case offsets[ins.OffsetMS]:
			errs.Add(field("offset_ms"), validation.InsertOffsetTaken, "another insert starts at %s", clockTime(ins.OffsetMS))
		}
		offsets[ins.OffsetMS] = true
		if out.ToleranceMS < 0 || out.ToleranceMS > MaxToleranceMS {
			errs.Add(field("tolerance_ms"), validation.Range, "must be between 0 and %d", MaxToleranceMS)
		}
		switch {
		case (ins.TrackID == nil) == (ins.PlaylistID == nil):
			errs.Add(field("track_id"), validation.InsertSourceRequired, "set exactly one of track_id and playlist_id")
		case ins.TrackID != nil && missingTracks[*ins.TrackID]:
			errs.Add(field("track_id"), validation.UnknownID, "no track has ID %d", *ins.TrackID)
		case ins.PlaylistID != nil:
			if _, ok := own[*ins.PlaylistID]; !ok {
				errs.Add(field("playlist_id"), validation.UnknownID, "no playlist has ID %d", *ins.PlaylistID)
			}
		}
		c.Inserts = append(c.Inserts, out)
	}
	slices.SortStableFunc(c.Inserts, func(a, b Insert) int { return a.OffsetMS - b.OffsetMS })
	if err := errs.Err(); err != nil {
		return Clock{}, err
	}
	return c, nil
}

func uniq(ids []uint64) []uint64 {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}

// result reads the saved clock and runs the dry check on it.
func (s *Service) result(ctx context.Context, id uint64) (Result, error) {
	c, err := s.Get(ctx, id)
	if err != nil {
		return Result{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	warnings, err := s.warnings(cctx, c)
	if err != nil {
		return Result{}, fmt.Errorf("dry check of clock %d: %w", id, err)
	}
	return Result{Clock: c, Warnings: warnings}, nil
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

// clockTime formats an offset into the hour as MM:SS.
func clockTime(ms int) string { return fmt.Sprintf("%02d:%02d", ms/60_000, ms/1000%60) }

// fieldErrors maps store errors to the request field they concern. FK
// errors from a race after the checks cannot name the row.
var fieldErrors = []struct {
	err   error
	field string
	rule  validation.Rule
	msg   string
}{
	{ErrNameTaken, "name", validation.Taken, "another clock has this name"},
	{ErrUnknownPlaylist, "playlists", validation.UnknownID, "a playlist was deleted while the clock was saved"},
	{ErrUnknownTrack, "inserts", validation.UnknownID, "a track was deleted while the clock was saved"},
}

func asFieldError(err error) error {
	for _, fe := range fieldErrors {
		if errors.Is(err, fe.err) {
			return validation.Field(fe.field, fe.rule, "%s", fe.msg)
		}
	}
	return err
}
