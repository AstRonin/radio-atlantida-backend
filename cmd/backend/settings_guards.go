package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/AstRonin/radio-atlantida-backend/internal/settings"
	"github.com/AstRonin/radio-atlantida-backend/internal/streamformat"
)

// streamFormatKeys are the settings that make the stream format.
var streamFormatKeys = []string{
	settings.StreamCodec.Name(), settings.StreamBitrateKbps.Name(),
	settings.StreamSampleRateHz.Name(), settings.StreamChannels.Name(),
}

// checkEncoderLimits refuses a stream format the encoder cannot start
// (internal/streamformat, design §6.3, §8.8), judged on the combination as
// it will be saved: the batch over the saved values. Each changed format
// key is refused, so the panel shows the message on the fields the
// manager changed.
func checkEncoderLimits(svc *settings.Service) {
	settings.Check(svc, streamFormatKeys, encoderLimitCheck)
}

func encoderLimitCheck(_ context.Context, v settings.View) error {
	codec, err := settings.ViewValue(v, settings.StreamCodec)
	if err != nil {
		return err
	}
	channels, err := settings.ViewValue(v, settings.StreamChannels)
	if err != nil {
		return err
	}
	rate, err := settings.ViewValue(v, settings.StreamSampleRateHz)
	if err != nil {
		return err
	}
	kbps, err := settings.ViewValue(v, settings.StreamBitrateKbps)
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
			refused = append(refused, settings.Refuse(key, settings.RuleEncoderLimit, msg))
		}
	}
	return errors.Join(refused...)
}
