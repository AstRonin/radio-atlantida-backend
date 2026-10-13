package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
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
	// Bitrate is in bits per second.
	Bitrate    *int
	SampleRate *int
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

// Probe reads the duration, format, bitrate, and sample rate of the file at
// the relative path rel. A missing file returns ErrFileNotFound.
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
	abs := filepath.Join(p.lib.root, filepath.FromSlash(rel))
	//nolint:gosec // G204: bin is ffprobe from PATH, and abs is a validated path inside MEDIA_ROOT passed as one argument (no shell).
	cmd := exec.CommandContext(ctx, p.bin,
		"-v", "error",
		"-show_entries", "format=format_name,duration,bit_rate:stream=codec_type,sample_rate",
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
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		BitRate    string `json:"bit_rate"`
	} `json:"format"`
	Streams []struct {
		CodecType  string `json:"codec_type"`
		SampleRate string `json:"sample_rate"`
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
	for _, s := range po.Streams {
		if s.CodecType != "audio" {
			continue
		}
		hasAudio = true
		info.SampleRate = positiveInt(s.SampleRate)
		break
	}
	if !hasAudio {
		return Info{}, errors.New("no audio stream")
	}
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

func positiveInt(s string) *int {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return nil
	}
	return &n
}
