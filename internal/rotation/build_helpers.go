package rotation

import (
	"fmt"
	"slices"
	"time"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
)

// burnDraw uses up the draw of a slot a pin took over, so the draws after it
// (the targets of later playlists) stay the same as without the pin.
func (b *builder) burnDraw() { b.rng.Uint64() }

// at returns the instant of an offset into the hour.
func (b *builder) at(offsetMS int) time.Time {
	return b.in.Start.Add(time.Duration(offsetMS) * time.Millisecond)
}

// allowedOf returns the tracks of ts that may start at offset.
func (b *builder) allowedOf(ts []Track, offset int) []Track {
	at := b.at(offset)
	var out []Track
	for _, t := range ts {
		if b.chk.allowed(t, at) {
			out = append(out, t)
		}
	}
	return out
}

// fillAllowed returns the tracks of ts that a fill draw may start at offset:
// the allowed ones, without the tracks pinned in this hour.
func (b *builder) fillAllowed(ts []Track, offset int) []Track {
	return slices.DeleteFunc(b.allowedOf(ts, offset), func(t Track) bool { return b.pinned[t.ID] })
}

func clip(ms int) int { return min(max(ms, 0), clocks.HourMS) }

func (b *builder) warn(w Warning) { b.plan.Warnings = append(b.plan.Warnings, w) }

func (b *builder) warnRule(code WarningCode, playlist int, format string, args ...any) {
	b.warn(Warning{Code: code, PlaylistIndex: new(playlist), Message: fmt.Sprintf(format, args...)})
}

// clock formats an offset into the hour as station local HH:MM:SS.
func (b *builder) clock(offset int) string {
	return b.at(offset).In(b.in.Location).Format(time.TimeOnly)
}

// clockTime formats an offset into the hour as MM:SS.
func clockTime(ms int) string { return fmt.Sprintf("%02d:%02d", ms/minuteMS, ms/1000%60) }

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
