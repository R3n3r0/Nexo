#!/usr/bin/env bash
# Generates the demo video of the screenshots with ffmpeg: 20 seconds of
# animated "aurora" gradients with a title and a timecode, 1280x720 at 30
# fps, H.264 High with a keyframe every 2 seconds, AAC stereo, the index at
# the start (+faststart). No input file: everything comes from lavfi.
#
# Usage: gen_video.sh OUTPUT.mp4 [SECONDS]
set -euo pipefail

out="${1:?usage: gen_video.sh OUTPUT.mp4 [SECONDS]}"
seconds="${2:-20}"

sources=(
  -f lavfi -i "gradients=s=1280x720:r=30:n=3:c0=0x020617:c1=0x0b1d51:c2=0x1e1b4b:speed=0.01:type=linear:seed=11"
  -f lavfi -i "gradients=s=1280x720:r=30:n=6:c0=0x02140f:c1=0x00e68a:c2=0x05b2c9:c3=0x02101f:c4=0x7c3aed:c5=0x34d399:speed=0.02:type=linear:seed=5"
  -f lavfi -i "gradients=s=1280x720:r=30:n=3:c0=0x000000:c1=0xdb2777:c2=0x000000:speed=0.015:type=radial:seed=9"
  -f lavfi -i "sine=f=196:beep_factor=3:sample_rate=48000"
  -f lavfi -i "sine=f=294:sample_rate=48000"
)
# The gradients blend in planar RGB (packed RGBA loses the green).
background="[0:v]format=gbrp[g0];[1:v]format=gbrp[g1];[2:v]format=gbrp[g2];[g0][g1]blend=all_mode=screen:all_opacity=0.9[a];[a][g2]blend=all_mode=screen:all_opacity=0.3"
text=",drawtext=font=Inter Display:text=AURORA:fontsize=150:fontcolor=white@0.92:x=(w-tw)/2:y=(h-th)/2-40:alpha='min(1,t/2)'"
text+=",drawtext=font=Inter:text=A Nexo video streaming demo:fontsize=34:fontcolor=white@0.8:x=(w-tw)/2:y=h/2+90:alpha='min(1,max(0,(t-1)/2))'"
text+=",drawtext=font=monospace:text='%{pts\:hms}':fontsize=24:fontcolor=white@0.7:x=w-tw-40:y=36"
audio="[3:a][4:a]amix=inputs=2,volume=0.4[au]"
encode=(-map "[v]" -map "[au]" -t "$seconds"
  -c:v libx264 -preset medium -profile:v high -pix_fmt yuv420p -crf 21 -g 60 -keyint_min 60 -sc_threshold 0
  -c:a aac -b:a 128k -ac 2 -movflags +faststart)

# drawtext needs an ffmpeg built with libfreetype: without it, no title.
if ! ffmpeg -hide_banner -loglevel error -y "${sources[@]}" \
  -filter_complex "$background$text,format=yuv420p[v];$audio" "${encode[@]}" "$out"; then
  echo "gen_video: drawtext failed, the video has no title" >&2
  ffmpeg -hide_banner -loglevel error -y "${sources[@]}" \
    -filter_complex "$background,format=yuv420p[v];$audio" "${encode[@]}" "$out"
fi
