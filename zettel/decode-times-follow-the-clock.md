# Decode times are a function of the clock, not of what was written before

The packager puts a source segment at `anchor + (decode time - its second in the file)` where the anchor is the clock of the clip
(seconds since 2020-01-01). Everything but the whole second is kept, so the small offset between audio and video that a file has (audio is
cut at a whole AAC frame, 21 ms) stays as it was in the source.

The first version added up durations per track. Audio of a file is cut at a whole AAC frame, so it ends up to a frame longer or shorter than its video, and
every new file would have moved audio a little further from the video: tens of milliseconds per file, seconds over many files. (Found by reasoning about the
design, the end-to-end test of that version did not measure A/V sync; the unit test `TestStitch_AudioStaysInSyncWithVideoAcrossManyFiles` now does.) It also needed the counters to survive a restart (they were stored in the playlist). The
clock needs neither. Inside a file the result is exactly continuous, at a change of file audio may differ by one audio frame, which is why there is a
discontinuity there. A file whose decode times are not on its own seconds (cut, joined, 29.97 fps) is refused with `reason="timeline"`.
Integer arithmetic only: seconds as floats lose milliseconds at 2.9e8 seconds. See [[one-second-segments-and-the-fade]].
