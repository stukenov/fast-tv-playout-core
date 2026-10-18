# EXT-X-DISCONTINUITY is written wherever the source file changes, not only at ads

Every source file has its own init section, so a change of source is a discontinuity for the player even though the decode times are
now continuous ([[stitching-instead-of-transcoding]]). The packager marks it when the source playlist changes, when the segment index does
not follow the previous one (a slate looping around, a gap), and after a restart (the predecessor is unknown). The first README promised
"discontinuity only at ad boundaries"; that is not possible when sources are different files.
