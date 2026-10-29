package rotation

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"slices"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
	"github.com/AstRonin/radio-atlantida-backend/internal/playrules"
)

// digestKey identifies a clock version for the digest cache.
type digestKey struct {
	clock   uint64
	updated int64
}

// clockPlaylists returns the playlists a build of an hour of c reads: the
// playlists of c and of its template inserts, sorted and unique.
func clockPlaylists(c clocks.Clock) []uint64 {
	var out []uint64
	for _, p := range c.Playlists {
		out = append(out, p.PlaylistID)
	}
	for _, ins := range c.Inserts {
		if ins.PlaylistID != nil {
			out = append(out, *ins.PlaylistID)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// clockDigest fingerprints the library as an hour of c sees it: every track
// of the playlists of c (with its length on air, cue, artist, playlists and
// rules) and the play rules those tracks use. Every snapshot that holds the
// playlists of c holds the same tracks, so the digest does not depend on the
// scope of the snapshot. It is cached per clock version until the snapshot
// gains tracks.
func (l *Library) clockDigest(c clocks.Clock) string {
	key := digestKey{clock: c.ID, updated: c.UpdatedAt.UnixNano()}
	if d, ok := l.digests[key]; ok {
		return d
	}
	want := clockPlaylists(c)
	h := sha256.New()
	rules := map[uint64]bool{}
	for _, t := range l.Tracks {
		if !intersects(t.PlaylistIDs, want) {
			continue
		}
		writeTrack(h, t)
		for _, id := range t.RuleIDs {
			rules[id] = true
		}
	}
	ids := make([]uint64, 0, len(rules))
	for id := range rules {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		fmt.Fprintf(h, "%s\n", ruleText(id, l.Rules[id]))
	}
	d := hex.EncodeToString(h.Sum(nil))
	if l.digests == nil {
		l.digests = map[digestKey]string{}
	}
	l.digests[key] = d
	return d
}

// inputsHash fingerprints what an hour is built from, besides its history:
// the clock version, the library as the clock sees it (clockDigest), the
// one-time inserts that stay (ID order, with the content of their tracks),
// the build settings and the station timezone. An hour whose stored
// fingerprint is equal to this one needs no rebuild (the full pass).
func inputsHash(c clocks.Clock, inserts []Insert, set Settings, loc string, lib *Library) string {
	h := sha256.New()
	fmt.Fprintf(h, "clock %d %d\n", c.ID, c.UpdatedAt.UnixNano())
	fmt.Fprintf(h, "library %s\n", lib.clockDigest(c))
	for _, ins := range inserts {
		fmt.Fprintf(h, "insert %d track %d missed %s placement %s offset %s tolerance %d\n",
			ins.ID, ins.TrackID, optUint(ins.MissedID), ins.Placement, optInt(ins.OffsetMS), ins.ToleranceMS)
		if t, ok := lib.track(ins.TrackID); ok {
			writeTrack(h, t)
		} else {
			fmt.Fprintf(h, "track %d missing\n", ins.TrackID)
		}
	}
	fmt.Fprintf(h, "settings min_gap %d timezone %s\n", set.MinGapMS, loc)
	return hex.EncodeToString(h.Sum(nil))
}

// writeTrack writes the fields of a track that a build reads (not its title).
func writeTrack(h hash.Hash, t Track) {
	rules := slices.Sorted(slices.Values(t.RuleIDs))
	fmt.Fprintf(h, "track %d artist %s air %d cue %d playlists %v rules %v\n",
		t.ID, optUint(t.ArtistID), t.DurationMS, t.CueInMS, t.PlaylistIDs, rules)
}

// ruleText is the text of a play rule as a build checks it.
func ruleText(id uint64, r playrules.RuleInput) string {
	var from, to string
	if r.WindowFrom != nil && r.WindowTo != nil {
		from, to = fmt.Sprint(int(*r.WindowFrom)), fmt.Sprint(int(*r.WindowTo))
	}
	return fmt.Sprintf("rule %d window %s-%s weekdays %v interval %s artist %s max %s",
		id, from, to, r.Weekdays, optInt(r.MinIntervalMinutes), optInt(r.ArtistMinIntervalMinutes), optInt(r.MaxPerHour))
}

// intersects reports whether two sorted ID lists share an ID.
func intersects(a, b []uint64) bool {
	for _, id := range a {
		if _, ok := slices.BinarySearch(b, id); ok {
			return true
		}
	}
	return false
}

// optInt formats an optional number; "-" when absent.
func optInt(v *int) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprint(*v)
}

// optUint formats an optional ID; "-" when absent.
func optUint(v *uint64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprint(*v)
}
