# The packager copies source segments and rewrites only their decode times; it does not encode

The sources are already HLS/CMAF, so nothing is re-encoded. The packager downloads the 2-second segment a clip points at,
changes the `tfdt` (decode time) of its track fragments so that the segment sits on the timeline of the channel ([[decode-times-follow-the-clock]]), and writes it
next to the playlist. The media data is byte for byte the source's. A channel costs a few megabytes of memory and some disk writes.

Rejected, first: a playlist that points straight at the sources (zero copy). It was the first version. It works in players that honor
discontinuities, but every file restarts its timeline at zero, and strict demuxers (ffmpeg, and many TVs) lose or mis-order data when
time steps back, see [[ffmpeg-drops-a-segment-after-a-discontinuity]]. Rejected, second: an ffmpeg process per channel that cuts and re-encodes.
It is the usual FAST design but needs CPU, disk and a supervisor, and adds a way to fail that this design does not have.

The price: the sources must be 1-second fMP4 with 64-bit `tfdt`, and the packager downloads every segment once per airing.
Bonus: players only talk to `/hls/`, the sources can stay private. See [[discontinuity-follows-the-source]].
