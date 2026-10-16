package ingest

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/library"
)

func TestWaveformCachedByChecksum(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.tools.audio("library/a.mp3", "AB", mp3Info("", ""), nil)
	f.store.add(Track{ID: 1, FilePath: "library/a.mp3", Checksum: new("sum1")})

	w, err := f.svc.Waveform(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, Waveform{DurationMS: 100, BucketMS: WaveformBucketMS, Peaks: []int8{-65, 65, -66, 66}, Checksum: "sum1"}, w)
	_, err = f.svc.Waveform(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, 1, f.tools.peaks, "the stored waveform is reused")

	// A new file (its analysis stored a new checksum) is decoded again.
	f.store.tracks[1].Checksum = new("sum2")
	w, err = f.svc.Waveform(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, "sum2", w.Checksum)
	require.Equal(t, 2, f.tools.peaks)
}

func TestWaveformWithoutChecksumIsNotStored(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.tools.audio("library/a.mp3", "A", mp3Info("", ""), nil)
	f.store.add(Track{ID: 1, FilePath: "library/a.mp3"})
	for range 2 {
		_, err := f.svc.Waveform(t.Context(), 1)
		require.NoError(t, err)
	}
	require.Equal(t, 2, f.tools.peaks)
	require.Empty(t, f.store.waveforms)
}

func TestWaveformRefusals(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.store.add(Track{ID: 1, FilePath: "library/gone.mp3", Checksum: new("s")})
	f.files.put("library/text.mp3", "not audio")
	f.store.add(Track{ID: 2, FilePath: "library/text.mp3", Checksum: new("t")})

	_, err := f.svc.Waveform(t.Context(), 9)
	require.ErrorIs(t, err, library.ErrTrackNotFound)
	_, err = f.svc.Waveform(t.Context(), 1)
	require.ErrorIs(t, err, ErrWaveformUnavailable)
	_, err = f.svc.Waveform(t.Context(), 2)
	require.ErrorIs(t, err, ErrWaveformUnavailable)

	f.tools.missing = true
	_, err = f.svc.Waveform(t.Context(), 1)
	require.ErrorIs(t, err, ErrToolsUnavailable)
}
