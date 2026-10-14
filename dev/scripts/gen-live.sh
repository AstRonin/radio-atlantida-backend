#!/usr/bin/env bash
# Generates the Ogg/Opus test file of `make live-check` (docs/phase-12-plan.md):
# 20 s of a tone that beeps twice a second, one Opus packet per Ogg page
# (20 ms), as a browser sends the microphone over WebRTC.
#
# ffmpeg runs from a container, so no local install is needed.
# Usage: dev/scripts/gen-live.sh [file]   (default: ./.dev/live-check.ogg)
set -euo pipefail

FFMPEG_IMAGE="linuxserver/ffmpeg:9.0-cli-ls84"
FILE="${1:-./.dev/live-check.ogg}"

if [[ -f "$FILE" ]]; then
  exit 0
fi
mkdir -p "$(dirname "$FILE")"
OUT_DIR="$(cd "$(dirname "$FILE")" && pwd)"

docker run --rm --user "$(id -u):$(id -g)" -v "$OUT_DIR:/out" "$FFMPEG_IMAGE" \
  -hide_banner -loglevel error \
  -f lavfi -i "sine=frequency=660:duration=20:sample_rate=48000" \
  -af "volume='if(lt(mod(t,0.5),0.25),1,0)':eval=frame" \
  -ac 1 -c:a libopus -b:a 64k -frame_duration 20 -page_duration 20000 \
  "/out/$(basename "$FILE")"
echo "create $FILE"
