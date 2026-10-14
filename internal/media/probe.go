package media

import (
	"bytes"
	"cmp"
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

// Stored formats of a track (tracks.format).
const (
	FormatMP3  = "mp3"
	FormatAAC  = "aac"
	FormatM4A  = "m4a"
	FormatOgg  = "ogg"
	FormatFLAC = "flac"
	FormatWAV  = "wav"
)

// Formats lists the stored formats (for the API enum).
func Formats() []string {
	return []string{FormatMP3, FormatAAC, FormatM4A, FormatOgg, FormatFLAC, FormatWAV}
}

// ErrProbeUnavailable is returned when ffprobe is not installed. The runtime image has it;
// a backend started on a development host may not.
var ErrProbeUnavailable = errors.New("ffprobe is not available")

// probeTimeout bounds one ffprobe run. Reading the headers of a local file
// takes milliseconds; a hung network share must not hold the request.
const probeTimeout = 10 * time.Second

// Info is what ffprobe reports about a file. A field is nil when ffprobe
// does not report it.
type Info struct {
	DurationMS *int
	// Format is one of the Format* constants.
	Format *string
	// Codec is the codec of the audio stream as ffprobe names it (mp3, aac,
	// vorbis, flac, pcm_s16le, ...).
	Codec *string
	// Bitrate is in bits per second.
	Bitrate    *int
	SampleRate *int
	Channels   *int
	// Title and Artist are the file's tags; empty when it has none.
	Title  string
	Artist string
}

// LosslessCodec reports whether codec (as ffprobe names it) stores audio
// without loss (FLAC, ALAC, PCM): the bitrate of such a file says nothing
// about its quality.
func LosslessCodec(codec string) bool {
	return codec == "flac" || codec == "alac" || strings.HasPrefix(codec, "pcm_")
}

// Prober reads the format of media files with ffprobe.
type Prober struct {
	lib *Library
	bin string
}

// NewProber returns a prober for the files of lib. It looks for ffprobe on
// PATH once; without it, Probe returns ErrProbeUnavailable.
func NewProber(lib *Library) *Prober {
	bin, err := exec.LookPath("ffprobe")
	if err != nil {
		bin = ""
	}
	return &Prober{lib: lib, bin: bin}
}

// Available reports whether ffprobe was found.
func (p *Prober) Available() bool { return p.bin != "" }

// Probe reads the duration, format, codec, bitrate, sample rate, channels,
// and tags of the file at the relative path rel. A missing file returns
// ErrFileNotFound.
func (p *Prober) Probe(ctx context.Context, rel string) (Info, error) {
	if p.bin == "" {
		return Info{}, ErrProbeUnavailable
	}
	// Opening first resolves the path inside the root, so ffprobe only ever
	// reads a file that Open accepts.
	f, err := p.lib.Open(rel)
	if err != nil {
		return Info{}, err
	}
	_ = f.Close()

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	abs := p.lib.abs(rel)
	//nolint:gosec // G204: bin is ffprobe from PATH, and abs is a validated path inside MEDIA_ROOT passed as one argument (no shell).
	cmd := exec.CommandContext(ctx, p.bin,
		"-v", "error",
		"-show_entries", "format=format_name,duration,bit_rate:format_tags:stream=codec_type,codec_name,sample_rate,channels:stream_tags",
		"-of", "json",
		"--", abs)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return Info{}, fmt.Errorf("ffprobe %s: %w: %s", rel, err, strings.TrimSpace(stderr.String()))
	}
	info, err := parseProbe(out)
	if err != nil {
		return Info{}, fmt.Errorf("ffprobe %s: %w", rel, err)
	}
	return info, nil
}

type probeOutput struct {
	Format struct {
		FormatName string            `json:"format_name"`
		Duration   string            `json:"duration"`
		BitRate    string            `json:"bit_rate"`
		Tags       map[string]string `json:"tags"`
	} `json:"format"`
	Streams []struct {
		CodecType  string            `json:"codec_type"`
		CodecName  string            `json:"codec_name"`
		SampleRate string            `json:"sample_rate"`
		Channels   int               `json:"channels"`
		Tags       map[string]string `json:"tags"`
	} `json:"streams"`
}

// formatNames maps ffprobe format names (the first of its comma list) to
// stored formats.
var formatNames = map[string]string{
	"mp3":  FormatMP3,
	"aac":  FormatAAC,
	"mov":  FormatM4A, // "mov,mp4,m4a,3gp,3g2,mj2"
	"ogg":  FormatOgg,
	"flac": FormatFLAC,
	"wav":  FormatWAV,
}

func parseProbe(out []byte) (Info, error) {
	var po probeOutput
	if err := json.Unmarshal(out, &po); err != nil {
		return Info{}, fmt.Errorf("decode output: %w", err)
	}
	var info Info
	hasAudio := false
	var streamTags map[string]string
	for _, s := range po.Streams {
		if s.CodecType != "audio" {
			continue
		}
		hasAudio = true
		info.SampleRate = positiveInt(s.SampleRate)
		if s.CodecName != "" {
			info.Codec = new(s.CodecName)
		}
		if s.Channels > 0 {
			info.Channels = new(s.Channels)
		}
		streamTags = s.Tags
		break
	}
	if !hasAudio {
		return Info{}, errors.New("no audio stream")
	}
	// MP3 and MP4 keep tags in the container, Ogg in the stream.
	info.Title = cmp.Or(tag(po.Format.Tags, "title"), tag(streamTags, "title"))
	info.Artist = cmp.Or(tag(po.Format.Tags, "artist"), tag(streamTags, "artist"))
	name, _, _ := strings.Cut(po.Format.FormatName, ",")
	if f, ok := formatNames[name]; ok {
		info.Format = &f
	}
	if d, err := strconv.ParseFloat(po.Format.Duration, 64); err == nil && d > 0 && d*1000 < math.MaxInt32 {
		ms := int(math.Round(d * 1000))
		info.DurationMS = &ms
	}
	info.Bitrate = positiveInt(po.Format.BitRate)
	return info, nil
}

// tag returns the tag key of tags, whatever its case (Vorbis comments are
// upper case, ID3 lower case), trimmed.
func tag(tags map[string]string, key string) string {
	for k, v := range tags {
		if strings.EqualFold(k, key) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func positiveInt(s string) *int {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return nil
	}
	return &n
}
