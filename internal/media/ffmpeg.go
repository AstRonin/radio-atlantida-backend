package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ErrFFmpegUnavailable is returned when ffmpeg is not installed. The
// runtime image has it; a development host may not.
var ErrFFmpegUnavailable = errors.New("ffmpeg is not available")

// ErrSilent is returned by Measure for a file without measurable loudness
// (silence, or shorter than the 400 ms gating block of EBU R128).
var ErrSilent = errors.New("no measurable loudness")

// Deadlines of one ffmpeg run. Both decode the whole file; an hour-long
// programme takes about a minute on one core.
const (
	measureTimeout = 5 * time.Minute
	encodeTimeout  = 10 * time.Minute
)

// Loudness is the EBU R128 measurement of a file (ffmpeg loudnorm, first
// pass).
type Loudness struct {
	IntegratedLUFS float64
	TruePeakDBTP   float64
	RangeLU        float64
	// ThresholdLUFS and Offset are inputs of the second pass.
	ThresholdLUFS float64
	Offset        float64
}

// Output is how ffmpeg writes a file of one format.
type Output struct {
	// Encoder and Muxer are ffmpeg names (libmp3lame, mp3).
	Encoder string
	Muxer   string
	// Ext is the file extension, with the dot.
	Ext string
	// Lossless outputs take no bitrate.
	Lossless bool
}

// OutputFor returns how to write a file of format (a Format* constant)
// with codec (as ffprobe names it). ok is false when the backend cannot
// write that combination.
func OutputFor(format, codec string) (out Output, ok bool) {
	switch {
	case format == FormatMP3:
		return Output{Encoder: "libmp3lame", Muxer: "mp3", Ext: ".mp3"}, true
	case format == FormatM4A && codec == "alac":
		return Output{Encoder: "alac", Muxer: "ipod", Ext: ".m4a", Lossless: true}, true
	case format == FormatM4A:
		return Output{Encoder: "aac", Muxer: "ipod", Ext: ".m4a"}, true
	case format == FormatAAC:
		return Output{Encoder: "aac", Muxer: "adts", Ext: ".aac"}, true
	case format == FormatOgg && codec == "vorbis":
		return Output{Encoder: "libvorbis", Muxer: "ogg", Ext: ".ogg"}, true
	case format == FormatOgg && codec == "opus":
		return Output{Encoder: "libopus", Muxer: "ogg", Ext: ".ogg"}, true
	case format == FormatOgg && codec == "flac":
		return Output{Encoder: "flac", Muxer: "ogg", Ext: ".ogg", Lossless: true}, true
	case format == FormatFLAC:
		return Output{Encoder: "flac", Muxer: "flac", Ext: ".flac", Lossless: true}, true
	case format == FormatWAV && strings.HasPrefix(codec, "pcm_"):
		// PCM encoders are named like their codecs.
		return Output{Encoder: codec, Muxer: "wav", Ext: ".wav", Lossless: true}, true
	}
	return Output{}, false
}

// Encoding is the audio of an encoded file.
type Encoding struct {
	Output Output
	// BitrateKbps is ignored for a lossless output.
	BitrateKbps int
	SampleRate  int
	Channels    int
}

// Normalize brings the loudness of a file to a target in the second pass of
// loudnorm, from the first-pass Measured values of the same file.
type Normalize struct {
	TargetLUFS   float64
	TruePeakDBTP float64
	Measured     Loudness
}

// FFmpeg measures and encodes media files.
type FFmpeg struct {
	lib *Library
	bin string
}

// NewFFmpeg returns the tools for the files of lib. It looks for ffmpeg on
// PATH once; without it, every call returns ErrFFmpegUnavailable.
func NewFFmpeg(lib *Library) *FFmpeg {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		bin = ""
	}
	return &FFmpeg{lib: lib, bin: bin}
}

// Available reports whether ffmpeg was found.
func (f *FFmpeg) Available() bool { return f.bin != "" }

// Measure reads the loudness of the first audio stream of the file at rel.
// It decodes the whole file on one thread, so it never takes more than one
// core from playout.
func (f *FFmpeg) Measure(ctx context.Context, rel string) (Loudness, error) {
	if f.bin == "" {
		return Loudness{}, ErrFFmpegUnavailable
	}
	if err := f.check(rel); err != nil {
		return Loudness{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, measureTimeout)
	defer cancel()
	stderr, err := f.run(ctx,
		"-hide_banner", "-nostdin", "-nostats", "-threads", "1", "-filter_threads", "1",
		"-i", "file:"+f.lib.abs(rel),
		"-map", "0:a:0", "-af", "loudnorm=print_format=json", "-f", "null", "-")
	if err != nil {
		return Loudness{}, fmt.Errorf("measure %s: %w", rel, err)
	}
	l, err := parseLoudnorm(stderr)
	if err != nil {
		return Loudness{}, fmt.Errorf("measure %s: %w", rel, err)
	}
	return l, nil
}

// Encode writes the first audio stream of src to dst (both relative to the
// root) with enc, normalized when norm is set. Tags are copied; pictures
// and other streams are not. dst is overwritten; the caller writes to a
// temporary name and commits it (Library.Commit).
func (f *FFmpeg) Encode(ctx context.Context, src, dst string, enc Encoding, norm *Normalize) error {
	if f.bin == "" {
		return ErrFFmpegUnavailable
	}
	if err := f.check(src); err != nil {
		return err
	}
	if err := ValidatePath(dst); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, encodeTimeout)
	defer cancel()
	if _, err := f.run(ctx, encodeArgs(f.lib.abs(src), f.lib.abs(dst), enc, norm)...); err != nil {
		return fmt.Errorf("encode %s: %w", src, err)
	}
	return nil
}

func encodeArgs(src, dst string, enc Encoding, norm *Normalize) []string {
	args := []string{
		"-hide_banner", "-nostdin", "-nostats", "-v", "error", "-y", "-threads", "1", "-filter_threads", "1",
		"-i", "file:" + src, "-map", "0:a:0", "-map_metadata", "0",
	}
	if norm != nil {
		args = append(args, "-af", loudnormFilter(*norm))
	}
	args = append(args, "-c:a", enc.Output.Encoder)
	if !enc.Output.Lossless && enc.BitrateKbps > 0 {
		args = append(args, "-b:a", strconv.Itoa(enc.BitrateKbps)+"k")
	}
	// loudnorm resamples to 192 kHz, so the rate is always given.
	args = append(args, "-ar", strconv.Itoa(enc.SampleRate), "-ac", strconv.Itoa(enc.Channels))
	if enc.Output.Muxer == "mp3" {
		// ID3v2.3 is what most players read.
		args = append(args, "-id3v2_version", "3")
	}
	return append(args, "-f", enc.Output.Muxer, "file:"+dst)
}

// loudnormFilter is the second pass of loudnorm. A loudness range target at
// least as wide as the measured one keeps the filter linear: one gain for
// the whole file, so the dynamics stay as they are.
func loudnormFilter(n Normalize) string {
	lra := math.Min(math.Max(7, math.Ceil(n.Measured.RangeLU)), 50)
	return fmt.Sprintf("loudnorm=I=%s:TP=%s:LRA=%s:measured_I=%s:measured_TP=%s:measured_LRA=%s:measured_thresh=%s:offset=%s:linear=true",
		num(n.TargetLUFS), num(n.TruePeakDBTP), num(lra),
		num(n.Measured.IntegratedLUFS), num(n.Measured.TruePeakDBTP), num(n.Measured.RangeLU),
		num(n.Measured.ThresholdLUFS), num(n.Measured.Offset))
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

// check opens rel first, so ffmpeg only ever reads a file that Open
// accepts (inside the root, no escaping link).
func (f *FFmpeg) check(rel string) error {
	file, err := f.lib.Open(rel)
	if err != nil {
		return err
	}
	return file.Close()
}

// run runs ffmpeg with args and returns what it wrote to stderr.
func (f *FFmpeg) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := commandContext(ctx, f.bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, lastLine(stderr.String()))
	}
	return stderr.Bytes(), nil
}

// lastLine keeps an ffmpeg error short: its last line names the problem.
func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	const maxLen = 300
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	return s
}

type loudnormOutput struct {
	InputI      string `json:"input_i"`
	InputTP     string `json:"input_tp"`
	InputLRA    string `json:"input_lra"`
	InputThresh string `json:"input_thresh"`
	Offset      string `json:"target_offset"`
}

// parseLoudnorm reads the JSON block that loudnorm prints at the end of
// ffmpeg's stderr.
func parseLoudnorm(stderr []byte) (Loudness, error) {
	end := bytes.LastIndexByte(stderr, '}')
	start := bytes.LastIndexByte(stderr[:max(end, 0)], '{')
	if start < 0 || end < start {
		return Loudness{}, errors.New("no loudnorm output")
	}
	var lo loudnormOutput
	if err := json.Unmarshal(stderr[start:end+1], &lo); err != nil {
		return Loudness{}, fmt.Errorf("decode loudnorm output: %w", err)
	}
	var l Loudness
	for _, v := range []struct {
		s   string
		dst *float64
	}{
		{lo.InputI, &l.IntegratedLUFS},
		{lo.InputTP, &l.TruePeakDBTP},
		{lo.InputLRA, &l.RangeLU},
		{lo.InputThresh, &l.ThresholdLUFS},
		{lo.Offset, &l.Offset},
	} {
		n, err := strconv.ParseFloat(strings.TrimSpace(v.s), 64)
		if err != nil || math.IsInf(n, 0) || math.IsNaN(n) {
			return Loudness{}, ErrSilent
		}
		*v.dst = n
	}
	return l, nil
}
