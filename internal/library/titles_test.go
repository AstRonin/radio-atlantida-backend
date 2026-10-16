package library

import (
	"bytes"
	"context"
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/db"
	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

func TestCleanTitle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, in, want string
	}{
		{"trimmed", "  Song  ", "Song"},
		{"runs of spaces collapsed", "Song   [Live]  Mix", "Song [Live] Mix"},
		{"tab and line break separate words", "One\tTwo\r\nThree", "One Two Three"},
		{"other control characters removed", "So\x00ng\x7f\u0085", "Song"},
		{"non-breaking space is white space", "A  B", "A B"},
		{"invalid UTF-8 dropped", "A\xffB", "AB"},
		{"Cyrillic and emoji kept", " Песня  🎵 ", "Песня 🎵"},
		{"only white space", " \t ", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, CleanTitle(tt.in))
		})
	}
}

func TestCreateTrackTitleRule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		title string
		want  string
		rule  validation.Rule
	}{
		{"cleaned", "  Song   [Live] ", "Song [Live]", ""},
		{"line break refused", "Song\nB-side", "", validation.InvalidCharacters},
		{"tab refused", "Song\tB", "", validation.InvalidCharacters},
		{"blank after cleaning", "   ", "", validation.Required},
		{"500 characters", strings.Repeat("я", 500), strings.Repeat("я", 500), ""},
		{"over 500 characters", strings.Repeat("я", 501), "", validation.Length},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tracks := &fakeTracks{}
			_, err := newTrackService(tracks, fakeProber{}, &bytes.Buffer{}).
				CreateTrack(t.Context(), TrackInput{Title: tt.title, FilePath: "a.mp3", TypeID: 1})
			if tt.rule == "" {
				require.NoError(t, err)
				require.Equal(t, tt.want, tracks.created.Title)
				return
			}
			var verr *validation.Error
			require.ErrorAs(t, err, &verr)
			require.Equal(t, []validation.FieldError{{Field: "title", Rule: tt.rule, Message: verr.Fields[0].Message}}, verr.Fields)
		})
	}
}

// fakeTitleTracks keeps the titles and reviewed marks of tracks.
type fakeTitleTracks struct {
	TrackStore // unused methods panic
	titles     map[uint64]string
	reviewed   map[uint64]bool
	// locked records forUpdate of every Titles call.
	locked  []bool
	written map[uint64]string
	calls   int
}

func newFakeTitleTracks(titles map[uint64]string) *fakeTitleTracks {
	return &fakeTitleTracks{titles: titles, reviewed: map[uint64]bool{}}
}

func (f *fakeTitleTracks) WithTx(db.Tx) TrackStore { return f }

func (f *fakeTitleTracks) Titles(_ context.Context, ids []uint64, forUpdate bool) (map[uint64]string, error) {
	f.locked = append(f.locked, forUpdate)
	out := map[uint64]string{}
	for _, id := range ids {
		if v, ok := f.titles[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

func (f *fakeTitleTracks) SetTitles(_ context.Context, titles map[uint64]string) error {
	f.written = maps.Clone(titles)
	maps.Copy(f.titles, titles)
	for id := range titles {
		delete(f.reviewed, id)
	}
	return nil
}

func (f *fakeTitleTracks) SetReviewed(_ context.Context, ids []uint64, reviewed bool) ([]uint64, error) {
	f.calls++
	var out []uint64
	for _, id := range ids {
		if _, ok := f.titles[id]; ok {
			f.reviewed[id] = reviewed
			out = append(out, id)
		}
	}
	return out, nil
}

func TestRenameComputation(t *testing.T) {
	t.Parallel()
	titles := map[uint64]string{
		1: "Song [www.site.com] Mix",
		2: "Other [WWW.SITE.COM]",
		3: "No site here",
		4: "[www.site.com]",
		5: "Live live LIVE",
		6: "Song  [www.site.com]",
	}
	tests := []struct {
		name string
		req  RenameRequest
		want RenameResult
	}{
		{
			name: "case-insensitive by default, spaces collapsed",
			req:  RenameRequest{TrackIDs: []uint64{3, 1, 2, 9, 4}, Find: "[www.site.com]"},
			want: RenameResult{
				Changed: []TitleChange{
					{TrackID: 1, OldTitle: "Song [www.site.com] Mix", NewTitle: "Song Mix"},
					{TrackID: 2, OldTitle: "Other [WWW.SITE.COM]", NewTitle: "Other"},
				},
				Unchanged: []uint64{3},
				Refused:   []RenameRefusal{{TrackID: 9, Err: ErrTrackNotFound}, {TrackID: 4, Err: ErrTitleEmpty}},
			},
		},
		{
			name: "match case",
			req:  RenameRequest{TrackIDs: []uint64{1, 2}, Find: "[www.site.com]", Replace: "", MatchCase: true},
			want: RenameResult{
				Changed:   []TitleChange{{TrackID: 1, OldTitle: "Song [www.site.com] Mix", NewTitle: "Song Mix"}},
				Unchanged: []uint64{2},
				Refused:   []RenameRefusal{},
			},
		},
		{
			name: "every occurrence",
			req:  RenameRequest{TrackIDs: []uint64{5}, Find: "live", Replace: "Studio"},
			want: RenameResult{
				Changed:   []TitleChange{{TrackID: 5, OldTitle: "Live live LIVE", NewTitle: "Studio Studio Studio"}},
				Unchanged: []uint64{}, Refused: []RenameRefusal{},
			},
		},
		{
			name: "pattern characters are literal",
			req:  RenameRequest{TrackIDs: []uint64{1, 3}, Find: "site.com]", Replace: "x"},
			want: RenameResult{
				Changed:   []TitleChange{{TrackID: 1, OldTitle: "Song [www.site.com] Mix", NewTitle: "Song [www.x Mix"}},
				Unchanged: []uint64{3}, Refused: []RenameRefusal{},
			},
		},
		{
			name: "same title after cleaning is unchanged",
			req:  RenameRequest{TrackIDs: []uint64{3}, Find: "here", Replace: "here"},
			want: RenameResult{Changed: []TitleChange{}, Unchanged: []uint64{3}, Refused: []RenameRefusal{}},
		},
		{
			name: "a stored double space goes with a change",
			req:  RenameRequest{TrackIDs: []uint64{6}, Find: " [www.site.com]"},
			want: RenameResult{
				Changed:   []TitleChange{{TrackID: 6, OldTitle: "Song  [www.site.com]", NewTitle: "Song"}},
				Unchanged: []uint64{}, Refused: []RenameRefusal{},
			},
		},
		{
			name: "too long is refused, not cut",
			req:  RenameRequest{TrackIDs: []uint64{5}, Find: "live", Replace: strings.Repeat("я", 200)},
			want: RenameResult{
				Changed: []TitleChange{}, Unchanged: []uint64{},
				Refused: []RenameRefusal{{TrackID: 5, Err: ErrTitleTooLong}},
			},
		},
		{
			name: "Cyrillic case folding",
			req:  RenameRequest{TrackIDs: []uint64{7}, Find: "ПЕСНЯ", Replace: "Song"},
			want: RenameResult{
				Changed:   []TitleChange{{TrackID: 7, OldTitle: "Моя песня", NewTitle: "Моя Song"}},
				Unchanged: []uint64{}, Refused: []RenameRefusal{},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			all := maps.Clone(titles)
			all[7] = "Моя песня"
			tracks := newFakeTitleTracks(all)
			dry := tt.req
			dry.DryRun = true
			res, err := newTrackServiceOn(tracks, &bytes.Buffer{}).RenameTracks(t.Context(), dry)
			require.NoError(t, err)
			require.Equal(t, tt.want, res)
			require.Nil(t, tracks.written, "a dry run writes nothing")
			require.Equal(t, []bool{false}, tracks.locked)
		})
	}
}

func TestRenameAppliesInOneTransaction(t *testing.T) {
	t.Parallel()
	tracks := newFakeTitleTracks(map[uint64]string{1: "A [x]", 2: "B", 3: "C [x]"})
	tracks.reviewed = map[uint64]bool{1: true, 2: true}
	res, err := newTrackServiceOn(tracks, &bytes.Buffer{}).
		RenameTracks(t.Context(), RenameRequest{TrackIDs: []uint64{1, 2, 3}, Find: "[x]"})
	require.NoError(t, err)
	require.Len(t, res.Changed, 2)
	require.Equal(t, []bool{true}, tracks.locked, "titles read under a row lock")
	require.Equal(t, map[uint64]string{1: "A", 3: "C"}, tracks.written)
	require.Equal(t, map[uint64]bool{2: true}, tracks.reviewed, "the mark is cleared only on changed tracks")
}

func TestRenameRefusals(t *testing.T) {
	t.Parallel()
	ids := []uint64{1}
	many := make([]uint64, MaxRenameTracks+1)
	for i := range many {
		many[i] = uint64(i + 1)
	}
	tests := []struct {
		name  string
		req   RenameRequest
		field string
		rule  validation.Rule
	}{
		{"no tracks", RenameRequest{Find: "a"}, "track_ids", validation.Range},
		{"too many tracks", RenameRequest{TrackIDs: many, Find: "a"}, "track_ids", validation.Range},
		{"repeated track", RenameRequest{TrackIDs: []uint64{1, 1}, Find: "a"}, "track_ids", validation.Invalid},
		{"empty find", RenameRequest{TrackIDs: ids}, "find", validation.Required},
		{"blank find", RenameRequest{TrackIDs: ids, Find: "  "}, "find", validation.Required},
		{"long find", RenameRequest{TrackIDs: ids, Find: strings.Repeat("a", 201)}, "find", validation.Length},
		{"control in find", RenameRequest{TrackIDs: ids, Find: "a\tb"}, "find", validation.InvalidCharacters},
		{"long replace", RenameRequest{TrackIDs: ids, Find: "a", Replace: strings.Repeat("b", 201)}, "replace", validation.Length},
		{"control in replace", RenameRequest{TrackIDs: ids, Find: "a", Replace: "b\n"}, "replace", validation.InvalidCharacters},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tracks := newFakeTitleTracks(map[uint64]string{1: "a"})
			_, err := newTrackServiceOn(tracks, &bytes.Buffer{}).RenameTracks(t.Context(), tt.req)
			var verr *validation.Error
			require.ErrorAs(t, err, &verr)
			require.Equal(t, tt.field, verr.Fields[0].Field)
			require.Equal(t, tt.rule, verr.Fields[0].Rule)
			require.Empty(t, tracks.locked, "nothing read")
		})
	}
}

func TestReviewTracks(t *testing.T) {
	t.Parallel()
	tracks := newFakeTitleTracks(map[uint64]string{1: "a", 2: "b", 3: "c"})
	svc := newTrackServiceOn(tracks, &bytes.Buffer{})
	res, err := svc.ReviewTracks(t.Context(), []uint64{3, 9, 1}, true)
	require.NoError(t, err)
	require.Equal(t, ReviewTracksResult{Updated: []uint64{3, 1}, NotFound: []uint64{9}}, res, "request order")
	require.Equal(t, map[uint64]bool{1: true, 3: true}, tracks.reviewed)

	many := make([]uint64, MaxReviewTracks+1)
	for i := range many {
		many[i] = uint64(i + 1)
	}
	for _, ids := range [][]uint64{nil, many, {2, 2}} {
		_, err = svc.ReviewTracks(t.Context(), ids, false)
		var verr *validation.Error
		require.ErrorAs(t, err, &verr)
		require.Equal(t, "track_ids", verr.Fields[0].Field)
	}
	require.Equal(t, 1, tracks.calls, "a refused request writes nothing")
}
