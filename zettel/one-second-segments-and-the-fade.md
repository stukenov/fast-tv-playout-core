# Every segment lasts exactly one second, and an incomplete last second is faded out and filled when the file is prepared

The rule: a segment is one second, not more and not less (`config.SegmentSec`, not a setting). Everything counts in whole seconds of
this length: the schedule, ad pods, the offset into a source, the playlist. Sources with a segment of another length are refused, so the
packager never has to guess how a short segment should be filled.

"Not enough" happens at the end of a file: 5.6 s of content gives a last segment of 0.6 s. The fix is a fade-out of the picture and of the
sound over the last half second and black plus silence for the rest of that second. A fade needs an encoder, and the packager copies bytes,
so it is done once per file in `tools/normalize.sh`, not at runtime on every airing. The price: a file that was not prepared with the script
and ends mid-second airs the slate for its last second instead of a fade.

29.97 fps cannot give a second of exactly 1.000 s (30 frames last 1.001 s) and is refused when the file is prepared, not 25 seconds into a
movie when the drift is noticed. See [[decode-times-follow-the-clock]], [[stitching-instead-of-transcoding]].
