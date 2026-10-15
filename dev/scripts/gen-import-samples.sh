#!/usr/bin/env bash
# Generates sample files for the import (docs/phase-11-plan.md,
# docs/phase-18-plan.md) into the folder import-samples of the media root:
# one that meets the broadcast standard and one per kind of quality issue.
# Import them by choosing that folder.
#
# ffmpeg runs from a container, so no local install is needed.
# Usage: dev/scripts/gen-import-samples.sh [media-root]   (default: ./.dev/media)
set -euo pipefail

FFMPEG_IMAGE="linuxserver/ffmpeg:9.0-cli-ls84"
ROOT="${1:-./.dev/media}"

mkdir -p "$ROOT/import-samples"
OUT_DIR="$(cd "$ROOT/import-samples" && pwd)"

# The lavfi sine is at -18 dBFS; mono to stereo takes 3 dB more, so +5.8 dB
# gives about -16 LUFS in stereo.
# name|ffmpeg output options
FILES=(
  "good.mp3|-ac 2 -ar 44100 -c:a libmp3lame -b:a 320k -af volume=5.8dB"
  "quiet.mp3|-ac 2 -ar 44100 -c:a libmp3lame -b:a 320k -af volume=-6dB"
  "hot.mp3|-ac 2 -ar 44100 -c:a libmp3lame -b:a 320k -af volume=20dB"
  "mono-22k.mp3|-ac 1 -ar 22050 -c:a libmp3lame -b:a 64k -af volume=8.8dB"
  "speech.wav|-ac 1 -ar 48000 -c:a pcm_s16le -af volume=8.8dB"
  "jingle.ogg|-ac 2 -ar 44100 -c:a libvorbis -q:a 5 -af volume=5.8dB"
)

for entry in "${FILES[@]}"; do
  IFS='|' read -r file opts <<<"$entry"
  if [[ -f "$OUT_DIR/$file" ]]; then
    echo "skip   import-samples/$file (exists)"
    continue
  fi
  # $opts is split into words on purpose.
  # shellcheck disable=SC2086
  docker run --rm --user "$(id -u):$(id -g)" -v "$OUT_DIR:/out" "$FFMPEG_IMAGE" \
    -hide_banner -loglevel error \
    -f lavfi -i "sine=frequency=523:duration=20:sample_rate=48000" \
    $opts -metadata title="${file%.*}" -metadata artist="Import Test" \
    "/out/$file"
  echo "create import-samples/$file"
done
