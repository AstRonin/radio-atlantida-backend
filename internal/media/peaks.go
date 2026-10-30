package media

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"
)

const (
	// peaksRate is the sample rate the waveform is computed at: a mono mix
	// at half the CD rate keeps the peaks of the audible band and halves
	// the samples to read.
	peaksRate = 22050
	// peaksTimeout bounds one waveform decode; a five-minute MP3 takes well
	// under a second.
	peaksTimeout = 30 * time.Second
)

// Peaks decodes the first audio stream of the file at rel to a mono mix and
// returns its waveform: the min and the max sample of every bucket of
// bucketMS, scaled to -127..127, as pairs (min0, max0, min1, max1, ...),
// and the decoded duration. The PCM is read as a stream, never held whole.
func (f *FFmpeg) Peaks(ctx context.Context, rel string, bucketMS int) ([]int8, int, error) {
	if f.bin == "" {
		return nil, 0, ErrFFmpegUnavailable
	}
	if err := f.check(rel); err != nil {
		return nil, 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, peaksTimeout)
	defer cancel()
	cmd := commandContext(ctx, f.bin,
		"-hide_banner", "-nostdin", "-nostats", "-v", "error", "-threads", "1",
		"-i", "file:"+f.lib.abs(rel), "-map", "0:a:0", "-ac", "1", "-ar", strconv.Itoa(peaksRate),
		"-c:a", "pcm_s16le", "-f", "s16le", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, 0, fmt.Errorf("peaks of %s: %w", rel, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, 0, fmt.Errorf("peaks of %s: %w", rel, err)
	}
	peaks, durationMS, rerr := PeaksOf(stdout, peaksRate, bucketMS)
	if rerr != nil {
		// Stop ffmpeg before waiting: it may block on a full pipe.
		cancel()
	}
	werr := cmd.Wait()
	if rerr != nil {
		return nil, 0, fmt.Errorf("peaks of %s: %w", rel, rerr)
	}
	if werr != nil {
		return nil, 0, fmt.Errorf("peaks of %s: %w: %s", rel, werr, lastLine(stderr.String()))
	}
	return peaks, durationMS, nil
}

// PeaksOf reads signed 16-bit little-endian mono PCM at rate and returns
// the min and max of every bucket of bucketMS (the last one may be
// partial), scaled to -127..127, and the duration of the PCM. A trailing
// odd byte is ignored.
func PeaksOf(r io.Reader, rate, bucketMS int) ([]int8, int, error) {
	if rate <= 0 || bucketMS <= 0 {
		return nil, 0, errors.New("rate and bucket must be positive")
	}
	br := bufio.NewReaderSize(r, 64<<10)
	perBucket := int64(rate) * int64(bucketMS) // samples × 1000 per bucket
	var out []int8
	var n int64
	var lo, hi int16
	buf := make([]byte, 2)
	for {
		if _, err := io.ReadFull(br, buf); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, 0, fmt.Errorf("read pcm: %w", err)
		}
		v := int16(binary.LittleEndian.Uint16(buf)) //nolint:gosec // G115: the bits are a signed sample
		// A sample starts a new bucket when its time crosses a bucket edge:
		// bucket(n) = n × 1000 / (rate × bucketMS).
		if n == 0 || n*1000/perBucket != (n-1)*1000/perBucket {
			if n > 0 {
				out = append(out, scale(lo), scale(hi))
			}
			lo, hi = v, v
		}
		lo, hi = min(lo, v), max(hi, v)
		n++
	}
	if n > 0 {
		out = append(out, scale(lo), scale(hi))
	}
	return out, int(n * 1000 / int64(rate)), nil
}

// scale maps a 16-bit sample to -127..127, so min and max are symmetric.
func scale(v int16) int8 {
	return int8(max(int32(v)*127/32767, -127)) //nolint:gosec // G115: within -127..127
}
