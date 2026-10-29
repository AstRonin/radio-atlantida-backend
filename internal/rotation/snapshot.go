package rotation

import (
	"context"
	"fmt"
	"slices"

	"github.com/AstRonin/radio-atlantida-backend/internal/clocks"
)

// libraryFor reads the snapshot of scope and records the scope in it.
func libraryFor(ctx context.Context, st Store, scope LibraryScope) (Library, error) {
	scope = normalize(scope)
	lib, err := st.Library(ctx, scope)
	if err != nil {
		return Library{}, fmt.Errorf("read library: %w", err)
	}
	lib.Scope = scope
	return lib, nil
}

// scopeOf returns the scope a build of hours of clocks with inserts reads: the
// playlists of the clocks and of their template inserts, and the tracks of
// every insert. A build of an hour reads nothing else of the library.
func scopeOf(cs []clocks.Clock, inserts []Insert) LibraryScope {
	var playlists, tracks []uint64
	for _, c := range cs {
		playlists = append(playlists, clockPlaylists(c)...)
		for _, ci := range c.Inserts {
			if ci.TrackID != nil {
				tracks = append(tracks, *ci.TrackID)
			}
		}
	}
	for _, ins := range inserts {
		tracks = append(tracks, ins.TrackID)
	}
	return normalize(LibraryScope{Playlists: playlists, Tracks: tracks})
}

// normalize sorts and dedups the IDs of a scope.
func normalize(s LibraryScope) LibraryScope {
	s.Playlists = sortedUnique(s.Playlists)
	s.Tracks = sortedUnique(s.Tracks)
	return s
}

func sortedUnique(ids []uint64) []uint64 {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}

// ensureLibrary makes sure lib holds what a build with the scope want reads,
// and reads what it lacks, so a change made during a run (a clock given a new
// playlist, a one-time insert of a track outside the snapshot) gives the same
// plan as a fresh snapshot. A missing playlist widens the snapshot by one
// read; a missing track is fetched by ID. The common case reads nothing.
func ensureLibrary(ctx context.Context, st Store, lib *Library, want LibraryScope) error {
	if lib.Scope.All {
		return nil
	}
	want = normalize(want)
	if !containsAll(lib.Scope.Playlists, want.Playlists) {
		widened := normalize(LibraryScope{
			Playlists: append(slices.Clone(lib.Scope.Playlists), want.Playlists...),
			Tracks:    append(slices.Clone(lib.Scope.Tracks), want.Tracks...),
		})
		fresh, err := libraryFor(ctx, st, widened)
		if err != nil {
			return err
		}
		*lib = fresh
		return nil
	}
	var missing []uint64
	for _, id := range want.Tracks {
		if _, ok := lib.track(id); ok || slices.Contains(lib.Scope.Tracks, id) {
			continue
		}
		missing = append(missing, id)
	}
	if len(missing) == 0 {
		return nil
	}
	got, err := st.TracksOf(ctx, missing)
	if err != nil {
		return fmt.Errorf("read tracks: %w", err)
	}
	lib.Tracks = append(lib.Tracks, got...)
	slices.SortFunc(lib.Tracks, func(a, b Track) int { return compareIDs(a.ID, b.ID) })
	lib.Scope.Tracks = sortedUnique(append(slices.Clone(lib.Scope.Tracks), missing...))
	lib.digests = nil
	return nil
}

// containsAll reports whether every ID of want is in have (both sorted).
func containsAll(have, want []uint64) bool {
	for _, id := range want {
		if _, ok := slices.BinarySearch(have, id); !ok {
			return false
		}
	}
	return true
}

func compareIDs(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// titlesOf reads the titles and artists of the tracks, for the output of a
// simulation or a sample. The snapshot does not hold them.
func titlesOf(ctx context.Context, st Store, ids []uint64) (map[uint64]Track, error) {
	out := map[uint64]Track{}
	if len(ids) == 0 {
		return out, nil
	}
	got, err := st.TracksOf(ctx, sortedUnique(ids))
	if err != nil {
		return nil, fmt.Errorf("read track titles: %w", err)
	}
	for _, t := range got {
		out[t.ID] = t
	}
	return out, nil
}
