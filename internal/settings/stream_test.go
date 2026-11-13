package settings_test

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/AstRonin/radio-atlantida-backend/internal/settings"
)

// encoderService is a settings service with the encoder check and the
// given saved values.
func encoderService(t *testing.T, saved map[string]string) *settings.Service {
	t.Helper()
	store := newFakeStore()
	for k, v := range saved {
		store.rows[k] = settings.Stored{Key: k, Value: json.RawMessage(v), UpdatedAt: time.Now().UTC()}
	}
	svc, err := settings.NewService(settings.NewRegistry(), store, allSecretsSet(),
		slog.New(slog.DiscardHandler), time.Now)
	require.NoError(t, err)
	settings.CheckEncoderLimits(svc)
	return svc
}

func refusals(t *testing.T, err error) map[string]*settings.InvalidValueError {
	t.Helper()
	var ive *settings.InvalidValuesError
	require.ErrorAs(t, err, &ive)
	out := map[string]*settings.InvalidValueError{}
	for _, e := range ive.Errors {
		out[e.Key] = e
	}
	return out
}

var (
	codecKey   = settings.StreamCodec.Name()
	bitrateKey = settings.StreamBitrateKbps.Name()
	rateKey    = settings.StreamSampleRateHz.Name()
	chanKey    = settings.StreamChannels.Name()
)

// The stream format is checked against the encoder limits as it will be
// saved: the batch over the saved values.
func TestEncoderLimitCheck(t *testing.T) {
	t.Parallel()
	vorbisMono22050 := map[string]string{codecKey: `"vorbis"`, chanKey: `1`, rateKey: `22050`, bitrateKey: `64`}

	t.Run("bitrate over the limit is refused", func(t *testing.T) {
		t.Parallel()
		svc := encoderService(t, vorbisMono22050)
		_, err := svc.SetMany(t.Context(), map[string]json.RawMessage{bitrateKey: json.RawMessage(`128`)})
		got := refusals(t, err)
		require.Len(t, got, 1)
		require.Equal(t, settings.RuleEncoderLimit, got[bitrateKey].Rule)
		require.Equal(t, "Vorbis mono at 22050 Hz allows at most 64 kbps; the bitrate is 128 kbps.", got[bitrateKey].Message)
	})

	t.Run("the same bitrate at another sample rate is allowed", func(t *testing.T) {
		t.Parallel()
		svc := encoderService(t, vorbisMono22050)
		_, err := svc.SetMany(t.Context(), map[string]json.RawMessage{
			bitrateKey: json.RawMessage(`128`), rateKey: json.RawMessage(`44100`),
		})
		require.NoError(t, err)
	})

	t.Run("every changed format key is refused", func(t *testing.T) {
		t.Parallel()
		svc := encoderService(t, nil) // mp3 stereo 44100 128
		_, err := svc.SetMany(t.Context(), map[string]json.RawMessage{
			codecKey: json.RawMessage(`"vorbis"`), chanKey: json.RawMessage(`1`), rateKey: json.RawMessage(`22050`),
		})
		got := refusals(t, err)
		require.Len(t, got, 3)
		for _, k := range []string{codecKey, chanKey, rateKey} {
			require.Equal(t, settings.RuleEncoderLimit, got[k].Rule, k)
		}
		require.NotContains(t, got, bitrateKey, "not changed")
	})

	// Saved one by one, raising the sample rate and the bitrate fails when
	// the bitrate goes first; as one batch (a map: no order) it passes.
	// The handler test sends the JSON keys in both orders.
	t.Run("sample rate and bitrate together", func(t *testing.T) {
		t.Parallel()
		oneByOne := encoderService(t, vorbisMono22050)
		_, err := oneByOne.Set(t.Context(), bitrateKey, json.RawMessage(`192`))
		var ive *settings.InvalidValueError
		require.ErrorAs(t, err, &ive)
		require.Equal(t, settings.RuleEncoderLimit, ive.Rule)

		svc := encoderService(t, vorbisMono22050)
		saved, err := svc.SetMany(t.Context(), map[string]json.RawMessage{
			bitrateKey: json.RawMessage(`192`), rateKey: json.RawMessage(`44100`),
		})
		require.NoError(t, err)
		require.Len(t, saved, 2)
		kbps, err := settings.Value(t.Context(), svc, settings.StreamBitrateKbps)
		require.NoError(t, err)
		require.Equal(t, 192, kbps)
	})

	t.Run("a single-key save is checked against the saved values", func(t *testing.T) {
		t.Parallel()
		svc := encoderService(t, map[string]string{codecKey: `"vorbis"`, chanKey: `1`, rateKey: `44100`, bitrateKey: `192`})
		_, err := svc.Set(t.Context(), rateKey, json.RawMessage(`22050`))
		var ive *settings.InvalidValueError
		require.ErrorAs(t, err, &ive)
		require.Equal(t, rateKey, ive.Key)
		require.Equal(t, settings.RuleEncoderLimit, ive.Rule)
		require.Equal(t, "Vorbis mono at 22050 Hz allows at most 64 kbps; the bitrate is 192 kbps.", ive.Message)

		_, err = svc.Set(t.Context(), rateKey, json.RawMessage(`48000`))
		require.NoError(t, err)
	})

	t.Run("mp3 and aac allow every combination", func(t *testing.T) {
		t.Parallel()
		for _, codec := range []string{`"mp3"`, `"aac"`} {
			for _, ch := range []string{`1`, `2`} {
				for _, sr := range []string{`22050`, `32000`, `44100`, `48000`} {
					svc := encoderService(t, nil)
					_, err := svc.SetMany(t.Context(), map[string]json.RawMessage{
						codecKey: json.RawMessage(codec), chanKey: json.RawMessage(ch),
						rateKey: json.RawMessage(sr), bitrateKey: json.RawMessage(`320`),
					})
					require.NoError(t, err, "%s %s ch %s Hz", codec, ch, sr)
				}
			}
		}
	})
}
