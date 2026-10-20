#!/usr/bin/env bash
# Records demo/sheaf.gif; run by `make demo` inside its container (see
# demo/Dockerfile), with the repository mounted at the working directory and
# sheaf built for the container in bin/linux/. The demo bundle lives in the
# container's /tmp and goes away with it.
#
# vhs writes a video (bin/demo.mp4, see demo/sheaf.tape), turned into the
# GIF in two passes, palette then frames: a GIF written by vhs itself is
# made in one ffmpeg pass that keeps every frame in memory until the
# palette is known (several GB for this demo).
set -euo pipefail

dir=/tmp/sheaf-demo
export PATH="$PWD/bin/linux:$PATH"
demo/setup.sh "$dir" >/dev/null
SHEAF_DEMO_DIR=$dir vhs demo/sheaf.tape

palette=$(mktemp --suffix=.png)
trap 'rm -f "$palette" bin/demo.mp4' EXIT
ffmpeg -v error -y -i bin/demo.mp4 -vf palettegen=stats_mode=diff "$palette"
ffmpeg -v error -y -i bin/demo.mp4 -i "$palette" \
  -lavfi 'paletteuse=dither=none:diff_mode=rectangle' demo/sheaf.gif
