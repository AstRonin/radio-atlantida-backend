// Package library stores the content library: playlist types, playlists,
// artists, tracks, and their memberships (§4 of the design). It implements
// the stores of the domain package with the same name; import it as
// libraryrepo next to the domain. GORM: admin CRUD (§5).
//
// Each aggregate has its own store. The stores that take part in a
// transaction have WithTx, which returns the store bound to the transaction
// as the domain's interface type (Go has no covariant return types).
package library

import (
	"errors"
	"fmt"
	"strings"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

// writeErrors map the constraints an insert or an update can violate to the
// domain's errors.
var writeErrors = map[string]error{
	"uq_playlist_types_code":         library.ErrTypeCodeTaken,
	"uq_playlist_types_name":         library.ErrTypeNameTaken,
	"uq_playlists_name":              library.ErrPlaylistNameTaken,
	"uq_artists_name":                library.ErrArtistNameTaken,
	"uq_tracks_file_path":            library.ErrFilePathTaken,
	"fk_playlists_type_id":           library.ErrUnknownType,
	"fk_tracks_type_id":              library.ErrUnknownType,
	"fk_tracks_artist_id":            library.ErrUnknownArtist,
	"fk_track_playlists_playlist_id": library.ErrUnknownPlaylist,
	"fk_track_playlists_track_id":    library.ErrUnknownTrack,
}

// historyConstraints are the foreign keys from the play log and the track
// plan. A delete they refuse is refused for good (§4), not until the
// manager edits something.
var historyConstraints = map[string]error{
	"fk_play_log_track_id":        library.ErrTrackHasHistory,
	"fk_scheduled_items_track_id": library.ErrTrackHasHistory,
	"fk_play_log_artist_id":       library.ErrArtistHasHistory,
}

// writeError maps a constraint violation of an insert or update; any other
// error is wrapped with op.
func writeError(op string, err error) error {
	if ce, ok := db.AsConstraintError(err); ok {
		if mapped, ok := writeErrors[ce.Constraint]; ok {
			return mapped
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}

// deleteError maps the foreign key violation of a delete to
// library.ErrInUse or to a history error.
func deleteError(op string, err error) error {
	if ce, ok := db.AsConstraintError(err); ok && ce.Kind == db.ForeignKey {
		if mapped, ok := historyConstraints[ce.Constraint]; ok {
			return mapped
		}
		return errors.Join(library.ErrInUse, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// refRow is an entity named in a refusal.
type refRow struct {
	Kind string
	ID   int64
	Name string
}

func refs(rows []refRow) []library.Ref {
	out := make([]library.Ref, len(rows))
	for i, r := range rows {
		out[i] = library.Ref{Kind: r.Kind, ID: r.ID, Name: r.Name}
	}
	return out
}

// linkRow is a row of a link table, read to fill ID lists without N+1.
type linkRow struct {
	OwnerID int64
	OtherID int64
}

// group collects the IDs of each owner, in the order of rows.
func group(rows []linkRow) map[int64][]int64 {
	out := make(map[int64][]int64)
	for _, r := range rows {
		out[r.OwnerID] = append(out[r.OwnerID], r.OtherID)
	}
	return out
}

// containsPattern is an ILIKE pattern that matches q anywhere, with the
// wildcards of q escaped (backslash is PostgreSQL's default escape).
func containsPattern(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}
