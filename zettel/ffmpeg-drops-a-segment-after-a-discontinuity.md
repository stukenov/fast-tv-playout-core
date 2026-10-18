# ffmpeg loses a segment when decode times step back, so the packager keeps them continuous

With a playlist that pointed straight at the sources, decoding the stream with ffmpeg gave 41 pictures for 50 segments: one segment
fewer per discontinuity. Hand written playlists show when it happens: two segments, a discontinuity, two more segments (the tests then used 2 s segments, the effect is the same at 1 s) from the same
or from another file give 150 frames instead of 200 whenever the decode times of the second part are lower than the end of the first
(true for every new file, which starts at zero). When the next part continues the timeline, or jumps forward, all frames decode, with or
without the discontinuity tag and with a different init section. Reproduced with ffmpeg 6.1, 7.1 and 9.0.

So it is not a quirk to ignore: a strict player may do the same. The packager rewrites `tfdt` to make the timeline continuous, and
the end-to-end test demands exactly one second of pictures (25) per segment from the whole stream. See [[stitching-instead-of-transcoding]].
