#!/usr/bin/env bash
# Turns any video file into a source the stitcher accepts: H.264 + AAC, a fixed frame rate, a keyframe every
# second, HLS with fMP4 segments of EXACTLY one second.
#
#   tools/normalize.sh input.mov media/movies/film1 [fps] [WxH]
#
# A file is rarely a whole number of seconds long. When the last second is not complete, the picture and the sound
# fade out over the last half second of the content (at most the whole content), and the rest of that second is
# filled with black and silence, so that the last segment is as long as all the others.
# Frame rates that cannot give a segment of exactly one second (29.97, 59.94) are refused.
#
# Then register it:  uri = http://origin:8080/vod/movies/film1/index.m3u8, duration_s = printed below.
# All your sources should share fps, resolution and codecs; they must match VARIANT_* in .env.
set -euo pipefail

IN=${1:?usage: normalize.sh input out_dir [fps] [WxH]}
OUT=${2:?usage: normalize.sh input out_dir [fps] [WxH]}
FPS=${3:-25}
SIZE=${4:-1280x720}
FADE=${FADE:-0.5}          # seconds of fade-out when the last second is incomplete
W=${SIZE%x*}
H=${SIZE#*x}

case "$FPS" in
  *.*|29|59) echo "fps $FPS cannot give segments of exactly 1.000 s; use a whole frame rate (24, 25, 30, 50, 60)" >&2; exit 2 ;;
esac

# real duration of the media: the longer of the video and the audio stream, not the container's guess
D=$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$IN")
read -r N PAD FADE_START FADE_LEN <<<"$(awk -v d="$D" -v f="$FADE" 'BEGIN {
  n = int(d); if (d - n > 0.001) n++
  if (n < 1) n = 1
  pad = n - d; if (pad < 0.001) pad = 0
  fl = f; if (fl > d) fl = d
  printf "%d %.6f %.6f %.6f\n", n, pad, d - fl, fl }')"

VF="scale=${W}:${H}:force_original_aspect_ratio=decrease,pad=${W}:${H}:(ow-iw)/2:(oh-ih)/2,setsar=1,fps=${FPS}"
AF="aresample=48000"
if awk -v p="$PAD" 'BEGIN { exit !(p > 0) }'; then
  VF="$VF,fade=t=out:st=${FADE_START}:d=${FADE_LEN},tpad=stop_mode=add:stop_duration=${PAD}:color=black"
  AF="$AF,afade=t=out:st=${FADE_START}:d=${FADE_LEN},apad=whole_dur=${N}"
  echo "the file lasts ${D}s: fading out the last ${FADE_LEN}s and filling ${PAD}s with black and silence to reach ${N}s"
else
  AF="$AF,apad=whole_dur=${N}"
fi

mkdir -p "$OUT"
ffmpeg -nostdin -v error -stats -y -i "$IN" -vf "$VF" -af "$AF" -t "$N" \
  -c:v libx264 -profile:v high -level:v 4.1 -pix_fmt yuv420p -preset medium \
  -g "$FPS" -keyint_min "$FPS" -sc_threshold 0 -force_key_frames "expr:gte(t,n_forced*1)" \
  -b:v 2500k -maxrate 3000k -bufsize 6000k \
  -c:a aac -b:a 128k -ac 2 -ar 48000 \
  -f hls -hls_time 1 -hls_playlist_type vod -hls_segment_type fmp4 -hls_flags independent_segments \
  -hls_fmp4_init_filename init.mp4 -hls_segment_filename "$OUT/seg_%d.m4s" "$OUT/index.m3u8"

# every segment must be exactly one second; anything else is refused at runtime
count=$(grep -c '^#EXTINF' "$OUT/index.m3u8")
bad=$(grep '^#EXTINF' "$OUT/index.m3u8" | grep -vc '^#EXTINF:1\.0*,' || true)
echo "segments: $count, duration_s: $N, segments that are not exactly 1 s: $bad"
[ "$bad" = 0 ] && [ "$count" = "$N" ] || { echo "the result does not have $N segments of exactly one second" >&2; exit 1; }
