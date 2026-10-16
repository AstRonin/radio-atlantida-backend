package clocks

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/validation"
)

// Playlists of the tests: 1 and 2 fill, 3 spread 2, 4 start.
var testOwn = map[uint64]PlaylistRule{
	1: {Rule: RuleFill},
	2: {Rule: RuleFill},
	3: {Rule: RuleSpread, Count: new(2)},
	4: {Rule: RuleStart},
}

func rule(r Rule) *Rule { return &r }

func TestCheck(t *testing.T) {
	t.Parallel()
	music := PlaylistInput{PlaylistID: 1}
	tests := []struct {
		name  string
		in    Input
		field string // "-" when valid
		rule  validation.Rule
	}{
		{"one fill playlist", Input{Name: "Music", Playlists: []PlaylistInput{music}}, "-", ""},
		{"fill and spread", Input{Name: "M", Playlists: []PlaylistInput{music, {PlaylistID: 3}}}, "-", ""},
		{"all weighted", Input{Name: "M", Playlists: []PlaylistInput{
			{PlaylistID: 1, FillWeight: new(30)}, {PlaylistID: 2, FillWeight: new(70)},
		}}, "-", ""},
		{"spread override with count", Input{Name: "M", Playlists: []PlaylistInput{
			music, {PlaylistID: 2, Rule: rule(RuleSpread), Count: new(4)},
		}}, "-", ""},
		{"fill override of a spread playlist", Input{Name: "M", Playlists: []PlaylistInput{{PlaylistID: 3, Rule: rule(RuleFill)}}}, "-", ""},
		{"blank name", Input{Name: " ", Playlists: []PlaylistInput{music}}, "name", validation.Required},
		{"no fill", Input{Name: "M", Playlists: []PlaylistInput{{PlaylistID: 3}}}, "playlists", validation.FillPlaylistRequired},
		{"no playlists", Input{Name: "M"}, "playlists", validation.FillPlaylistRequired},
		{
			"fill overridden away",
			Input{Name: "M", Playlists: []PlaylistInput{{PlaylistID: 1, Rule: rule(RuleEnd)}}},
			"playlists", validation.FillPlaylistRequired,
		},
		{
			"unknown playlist",
			Input{Name: "M", Playlists: []PlaylistInput{music, {PlaylistID: 99}}},
			"playlists[1].playlist_id", validation.UnknownID,
		},
		{"duplicate", Input{Name: "M", Playlists: []PlaylistInput{music, music}}, "playlists[1].playlist_id", validation.Invalid},
		{
			"partial weights",
			Input{Name: "M", Playlists: []PlaylistInput{{PlaylistID: 1, FillWeight: new(30)}, {PlaylistID: 2}}},
			"playlists[1].fill_weight", validation.FillWeightsPartial,
		},
		{
			"weight on a rule playlist",
			Input{Name: "M", Playlists: []PlaylistInput{music, {PlaylistID: 3, FillWeight: new(5)}}},
			"playlists[1].fill_weight", validation.FillWeightNotAllowed,
		},
		{
			"weight 0",
			Input{Name: "M", Playlists: []PlaylistInput{{PlaylistID: 1, FillWeight: new(0)}}},
			"playlists[0].fill_weight", validation.Range,
		},
		{
			"weight 101",
			Input{Name: "M", Playlists: []PlaylistInput{{PlaylistID: 1, FillWeight: new(101)}}},
			"playlists[0].fill_weight", validation.Range,
		},
		{
			"spread override without count",
			Input{Name: "M", Playlists: []PlaylistInput{music, {PlaylistID: 2, Rule: rule(RuleSpread)}}},
			"playlists[1].clock_count", validation.ClockCountRequired,
		},
		{
			"count without spread override",
			Input{Name: "M", Playlists: []PlaylistInput{music, {PlaylistID: 3, Count: new(3)}}},
			"playlists[1].clock_count", validation.ClockCountNotAllowed,
		},
		{
			"count 13",
			Input{Name: "M", Playlists: []PlaylistInput{music, {PlaylistID: 2, Rule: rule(RuleSpread), Count: new(13)}}},
			"playlists[1].clock_count", validation.Range,
		},
		{
			"unknown rule",
			Input{Name: "M", Playlists: []PlaylistInput{music, {PlaylistID: 2, Rule: rule("often")}}},
			"playlists[1].clock_rule", validation.Invalid,
		},
		{"insert with a track", Input{
			Name: "M", Playlists: []PlaylistInput{music},
			Inserts: []InsertInput{{OffsetMS: 1_780_000, TrackID: new(uint64(5))}},
		}, "-", ""},
		{"insert at 59:59", Input{
			Name: "M", Playlists: []PlaylistInput{music},
			Inserts: []InsertInput{{OffsetMS: MaxOffsetMS, PlaylistID: new(uint64(4))}},
		}, "-", ""},
		{"insert past the hour", Input{
			Name: "M", Playlists: []PlaylistInput{music},
			Inserts: []InsertInput{{OffsetMS: HourMS, TrackID: new(uint64(5))}},
		}, "inserts[0].offset_ms", validation.Range},
		{"insert not whole seconds", Input{
			Name: "M", Playlists: []PlaylistInput{music},
			Inserts: []InsertInput{{OffsetMS: 1500, TrackID: new(uint64(5))}},
		}, "inserts[0].offset_ms", validation.Format},
		{"insert offset twice", Input{Name: "M", Playlists: []PlaylistInput{music}, Inserts: []InsertInput{
			{OffsetMS: 60_000, TrackID: new(uint64(5))}, {OffsetMS: 60_000, PlaylistID: new(uint64(4))},
		}}, "inserts[1].offset_ms", validation.InsertOffsetTaken},
		{
			"insert tolerance",
			Input{
				Name: "M", Playlists: []PlaylistInput{music},
				Inserts: []InsertInput{{OffsetMS: 0, ToleranceMS: new(MaxToleranceMS + 1), TrackID: new(uint64(5))}},
			},
			"inserts[0].tolerance_ms", validation.Range,
		},
		{
			"insert without source",
			Input{Name: "M", Playlists: []PlaylistInput{music}, Inserts: []InsertInput{{OffsetMS: 0}}},
			"inserts[0].track_id", validation.InsertSourceRequired,
		},
		{
			"insert with both sources",
			Input{
				Name: "M", Playlists: []PlaylistInput{music},
				Inserts: []InsertInput{{OffsetMS: 0, TrackID: new(uint64(5)), PlaylistID: new(uint64(4))}},
			},
			"inserts[0].track_id", validation.InsertSourceRequired,
		},
		{"insert unknown track", Input{
			Name: "M", Playlists: []PlaylistInput{music},
			Inserts: []InsertInput{{OffsetMS: 0, TrackID: new(uint64(6))}},
		}, "inserts[0].track_id", validation.UnknownID},
		{"insert unknown playlist", Input{
			Name: "M", Playlists: []PlaylistInput{music},
			Inserts: []InsertInput{{OffsetMS: 0, PlaylistID: new(uint64(99))}},
		}, "inserts[0].playlist_id", validation.UnknownID},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := Check(tt.in, testOwn, map[uint64]bool{6: true}, 60_000)
			if tt.field == "-" {
				require.NoError(t, err)
				return
			}
			var ve *validation.Error
			require.ErrorAs(t, err, &ve)
			require.Len(t, ve.Fields, 1, ve.Error())
			require.Equal(t, tt.field, ve.Fields[0].Field, ve.Error())
			require.Equal(t, tt.rule, ve.Fields[0].Rule, ve.Error())
		})
	}
}

func TestCheckDefaultsAndOrder(t *testing.T) {
	t.Parallel()
	c, err := Check(Input{
		Name:      " Music ",
		Playlists: []PlaylistInput{{PlaylistID: 3}, {PlaylistID: 1}},
		Inserts: []InsertInput{
			{OffsetMS: 1_800_000, ToleranceMS: new(0), PlaylistID: new(uint64(4))},
			{OffsetMS: 1_780_000, TrackID: new(uint64(5))},
		},
	}, testOwn, nil, 60_000)
	require.NoError(t, err)
	require.Equal(t, "Music", c.Name)
	require.Equal(t, []int{1_780_000, 1_800_000}, []int{c.Inserts[0].OffsetMS, c.Inserts[1].OffsetMS}, "by offset")
	require.Equal(t, 60_000, c.Inserts[0].ToleranceMS, "default tolerance")
	require.Equal(t, 0, c.Inserts[1].ToleranceMS)
	require.Equal(t, uint64(3), c.Playlists[0].PlaylistID, "position order kept")
}

func TestEffectiveRule(t *testing.T) {
	t.Parallel()
	own := ClockPlaylist{PlaylistID: 3, Own: testOwn[3]}
	require.Equal(t, RuleSpread, own.EffectiveRule())
	require.Equal(t, 2, *own.EffectiveCount())

	over := ClockPlaylist{PlaylistID: 3, Rule: rule(RuleSpread), Count: new(5), Own: testOwn[3]}
	require.Equal(t, 5, *over.EffectiveCount())

	fill := ClockPlaylist{PlaylistID: 3, Rule: rule(RuleFill), Own: testOwn[3]}
	require.Equal(t, RuleFill, fill.EffectiveRule())
	require.Nil(t, fill.EffectiveCount())
}
