package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/config"
	"github.com/AstRonin/radio-atlantida-backend/internal/playout"
	"github.com/AstRonin/radio-atlantida-backend/internal/settings"
)

// A save with stream.* settings wakes the pusher once, which sends one
// output.set; a save without one does not.
func TestWakeOnStreamChange(t *testing.T) {
	t.Parallel()
	woken := 0
	fn := wakeOnStreamChange(func() { woken++ })
	fn([]settings.Change{{Key: settings.StreamCodec.Name()}})
	require.Equal(t, 1, woken)
	fn([]settings.Change{{Key: settings.StreamIcecastTLS.Name()}})
	require.Equal(t, 2, woken)

	fn([]settings.Change{
		{Key: settings.StreamCodec.Name()},
		{Key: settings.StreamBitrateKbps.Name()},
		{Key: settings.StreamSampleRateHz.Name()},
		{Key: settings.LiquidsoapMusicGain.Name()},
	})
	require.Equal(t, 3, woken, "one wake for a batch of several stream keys")

	fn([]settings.Change{{Key: settings.LiquidsoapMusicGain.Name()}, {Key: "streamer.other"}})
	fn(nil)
	require.Equal(t, 3, woken, "no wake without a stream key")
}

// The stream output resolves to the settings defaults, which equal the dev
// stack and the script's own fallback.
func TestStreamOutputDefaults(t *testing.T) {
	t.Parallel()
	svc, err := settings.NewService(settings.NewRegistry(), memStore{}, config.Secrets{}.IsSet(),
		slog.New(slog.DiscardHandler), time.Now)
	require.NoError(t, err)
	o, err := streamOutput(t.Context(), svc)
	require.NoError(t, err)
	require.Equal(t, playout.StreamOutput{
		Host: "icecast", Port: 8000, Mount: "/radio", Codec: "mp3", BitrateKbps: 128, SampleRateHz: 44100, Channels: 2,
		Name: "Radio Atlantida",
	}, o)

	// A saved value is read.
	_, err = svc.Set(t.Context(), settings.StreamIcecastTLS.Name(), json.RawMessage(`true`))
	require.NoError(t, err)
	o, err = streamOutput(t.Context(), svc)
	require.NoError(t, err)
	require.True(t, o.TLS)
}

// memStore keeps saved settings in memory.
type memStore map[string]settings.Stored

func (m memStore) List(context.Context) ([]settings.Stored, error) {
	out := make([]settings.Stored, 0, len(m))
	for _, s := range m {
		out = append(out, s)
	}
	return out, nil
}

func (m memStore) Get(_ context.Context, key string) (settings.Stored, error) {
	s, ok := m[key]
	if !ok {
		return settings.Stored{}, settings.ErrNotFound
	}
	return s, nil
}

func (m memStore) UpsertMany(_ context.Context, sts []settings.Stored) ([]settings.Stored, error) {
	for _, s := range sts {
		m[s.Key] = s
	}
	return sts, nil
}
