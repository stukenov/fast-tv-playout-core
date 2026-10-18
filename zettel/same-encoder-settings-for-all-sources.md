# All sources must be encoded with identical settings, or strict decoders break at the change of source

In the end-to-end test everything was fine until a source prepared by `tools/normalize.sh` (x264 preset medium) met sources generated with other
x264 options (veryfast): from the first segment of that source ffmpeg printed `error while decoding MB ...` for the rest of the stream. Each source alone decoded
cleanly, and the decode times were continuous. The init sections differed, and ffmpeg's HLS demuxer logs `Found duplicated MOOV Atom. Skipped it`: it
keeps the codec parameters (SPS/PPS) of the first init section and decodes the new source with the old ones. When every source was prepared by the same script,
all 13 init references in the playlist pointed at one identical init section and ffmpeg decoded all 3100 pictures.

Real players (hls.js, Safari) re-initialize at a discontinuity and mostly cope; ffmpeg and many TVs do not, and "plays in ffmpeg" is the bar we set.
So the rule is a requirement of the sources, not something the packager can fix (it does not decode). It can notice: the init file name is a hash of the
init content, a change of it between neighbouring segments is counted in `packager_init_changes_total` and logged. See [[one-second-segments-and-the-fade]].
