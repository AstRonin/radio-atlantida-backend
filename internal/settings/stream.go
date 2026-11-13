package settings

import (
	"context"
	"errors"
	"fmt"

	"github.com/AstRonin/radio-atlantida-backend/internal/streamformat"
)

// streamFormatKeys are the settings that make the stream format.
var streamFormatKeys = []string{
	StreamCodec.Name(), StreamBitrateKbps.Name(),
	StreamSampleRateHz.Name(), StreamChannels.Name(),
}

// CheckEncoderLimits registers the cross-key check of the stream format
// against the encoder limits of the pinned Liquidsoap (design §6.3).
//
// A stream format the encoder cannot start (internal/streamformat, design
// §8.8) is refused, judged on the combination as it will be saved: the
// batch over the saved values. Each changed format key is refused, so the
// panel shows the message on the fields the manager changed.
func CheckEncoderLimits(svc *Service) {
	Check(svc, streamFormatKeys, encoderLimitCheck)
}

func encoderLimitCheck(_ context.Context, v View) error {
	codec, err := ViewValue(v, StreamCodec)
	if err != nil {
		return err
	}
	channels, err := ViewValue(v, StreamChannels)
	if err != nil {
		return err
	}
	rate, err := ViewValue(v, StreamSampleRateHz)
	if err != nil {
		return err
	}
	kbps, err := ViewValue(v, StreamBitrateKbps)
	if err != nil {
		return err
	}
	l := streamformat.Check(codec, channels, rate, kbps)
	if l == nil {
		return nil
	}
	msg := fmt.Sprintf("%s; the bitrate is %d kbps.", l, kbps)
	var refused []error
	for _, key := range streamFormatKeys {
		if v.Changed(key) {
			refused = append(refused, Refuse(key, RuleEncoderLimit, msg))
		}
	}
	return errors.Join(refused...)
}
