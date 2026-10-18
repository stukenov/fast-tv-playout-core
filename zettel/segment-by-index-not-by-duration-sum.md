# The source segment is chosen by index (offset in seconds), not by adding up durations

With one second segments of exactly 1.000 s the index is simply the offset in seconds, and it never drifts. Summing durations would carry
rounding errors of `EXTINF` over a movie. A source segment whose duration differs from one second by more than
SOURCE_DURATION_TOLERANCE_S (0.5 ms, the rounding of `EXTINF`) is refused (the slate airs and `packager_stitch_errors_total{reason="duration"}` counts it).
Before the one second rule the tolerance was 0.25 s so that 2.002 s sources worked; that is gone, see [[one-second-segments-and-the-fade]].
