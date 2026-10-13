#!/usr/bin/env bash
# Generates short MP3 test tracks (sine tones) for the dev stack.
#
# ffmpeg runs from a container, so no local install is needed.
# Usage: dev/scripts/gen-media.sh [output-dir]   (default: ./.dev/media)
set -euo pipefail

FFMPEG_IMAGE="linuxserver/ffmpeg:9.0-cli-ls84"
OUT_DIR="${1:-./.dev/media}"

mkdir -p "$OUT_DIR"
OUT_DIR="$(cd "$OUT_DIR" && pwd)"

# name:duration-seconds:frequency-hz
TRACKS=(
  "jingle-20s-a:20:880"
  "jingle-20s-b:20:988"
  "news-45s:45:330"
  "segment-45s:45:392"
  "music-3m-a:180:440"
  "music-3m-b:180:494"
  "music-3m-c:180:523"
  "music-4m-a:240:587"
  "music-4m-b:240:659"
  "music-4m-c:240:698"
)

for entry in "${TRACKS[@]}"; do
  IFS=: read -r name seconds freq <<<"$entry"
  file="$name.mp3"
  if [[ -f "$OUT_DIR/$file" ]]; then
    echo "skip   $file (exists)"
    continue
  fi
  docker run --rm --user "$(id -u):$(id -g)" -v "$OUT_DIR:/out" "$FFMPEG_IMAGE" \
    -hide_banner -loglevel error \
    -f lavfi -i "sine=frequency=$freq:duration=$seconds:sample_rate=44100" \
    -ac 2 -ar 44100 -c:a libmp3lame -b:a 128k \
    -metadata title="$name" -metadata artist="Test Tone" \
    "/out/$file"
  echo "create $file (${seconds}s, ${freq} Hz)"
done
