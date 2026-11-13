package rotation

import (
	"cmp"
	"slices"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
)

// index groups the tracks of the clock's playlists and insert playlists,
// and lists the fill playlists with their shares (decision 9: without
// shares, every fill playlist weighs 1).
func (b *builder) index() {
	b.pinned = map[uint64]bool{}
	for _, h := range b.in.HourInserts {
		if h.Placement == PlacementFixed {
			b.pinned[h.TrackID] = true
		}
	}
	b.indexClaims()
	want := map[uint64]bool{}
	for _, p := range b.in.Clock.Playlists {
		want[p.PlaylistID] = true
		if p.EffectiveRule() == clocks.RuleFill {
			w := 1
			if p.FillWeight != nil {
				w = *p.FillWeight
			}
			b.fill = append(b.fill, fillSource{playlistID: p.PlaylistID, weight: w})
		}
	}
	for _, ins := range b.in.Clock.Inserts {
		if ins.PlaylistID != nil {
			want[*ins.PlaylistID] = true
		}
	}
	for _, t := range b.in.Library.Tracks {
		for _, id := range t.PlaylistIDs {
			if want[id] {
				b.tracks[id] = append(b.tracks[id], t)
			}
		}
	}
}

// indexClaims resolves the slot claims of the hour's pins, in insert ID order
// so the build is deterministic. A claim that finds no slot claims nothing,
// and the operator is told why the hour has one track more.
func (b *builder) indexClaims() {
	b.claims = slotClaims{
		start: map[int]bool{}, end: map[int]bool{}, middle: map[int]bool{},
		spread: map[int][]int{}, tracks: map[uint64]bool{}, pins: map[uint64]bool{},
	}
	pins := slices.Clone(b.in.HourInserts)
	slices.SortStableFunc(pins, func(a, b Insert) int { return cmp.Compare(a.ID, b.ID) })
	for _, h := range pins {
		// A deleted playlist leaves the origin behind (ON DELETE SET NULL).
		if h.RulePlaylistID == nil && h.RuleOriginMS == nil {
			continue
		}
		if b.claimSlot(h) {
			b.claims.tracks[h.TrackID], b.claims.pins[h.ID] = true, true
			continue
		}
		b.warn(Warning{
			Code: WarnPinClaimStale, TrackID: new(h.TrackID), HourInsertID: new(h.ID),
			Message: "a pinned rule track no longer takes a slot of its playlist, so the hour has one track more",
		})
	}
}

// claimSlot gives the claim of h a slot of its playlist; false if the
// playlist left the clock, is a fill playlist now, or has no slot left.
func (b *builder) claimSlot(h Insert) bool {
	if h.RulePlaylistID == nil || h.RuleOriginMS == nil || h.Placement != PlacementFixed {
		return false
	}
	i := slices.IndexFunc(b.in.Clock.Playlists, func(p clocks.ClockPlaylist) bool { return p.PlaylistID == *h.RulePlaylistID })
	if i < 0 {
		return false
	}
	take := func(slots map[int]bool) bool {
		if slots[i] {
			return false
		}
		slots[i] = true
		return true
	}
	p := b.in.Clock.Playlists[i]
	switch p.EffectiveRule() {
	case clocks.RuleStart:
		return take(b.claims.start)
	case clocks.RuleEnd:
		return take(b.claims.end)
	case clocks.RuleStartEnd:
		if *h.RuleOriginMS < middleTargetMS {
			return take(b.claims.start)
		}
		return take(b.claims.end)
	case clocks.RuleMiddle:
		return take(b.claims.middle)
	case clocks.RuleSpread:
		if len(b.claims.spread[i]) >= spreadCount(p) {
			return false
		}
		b.claims.spread[i] = append(b.claims.spread[i], *h.RuleOriginMS)
		return true
	default:
		return false
	}
}

// spreadCount is the number of slots of a spread playlist.
func spreadCount(p clocks.ClockPlaylist) int {
	if c := p.EffectiveCount(); c != nil {
		return *c
	}
	return 1
}

// avoidClaimed returns the candidates of a rule draw without the tracks of
// the claiming pins; all of them when nothing else is left.
func (b *builder) avoidClaimed(ts []Track) []Track {
	rest := slices.DeleteFunc(slices.Clone(ts), func(t Track) bool { return b.claims.tracks[t.ID] })
	if len(rest) == 0 {
		return ts
	}
	return rest
}
