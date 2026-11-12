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

// A saved stream.* setting wakes the pusher, which sends output.set; other
// groups do not.
func TestWakeOnStreamChange(t *testing.T) {
	t.Parallel()
	woken := 0
	fn := wakeOnStreamChange(func() { woken++ })
	fn(settings.Change{Key: settings.StreamCodec.Name()})
	fn(settings.Change{Key: settings.StreamIcecastTLS.Name()})
	fn(settings.Change{Key: settings.LiquidsoapMusicGain.Name()})
	fn(settings.Change{Key: "streamer.other"})
	require.Equal(t, 2, woken)
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

func (m memStore) Upsert(_ context.Context, s settings.Stored) (settings.Stored, error) {
	m[s.Key] = s
	return s, nil
}
