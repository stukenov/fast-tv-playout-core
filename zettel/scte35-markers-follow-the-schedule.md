# SCTE-35 markers come from the break boundaries of the schedule

A break is marked in the playlist at its first second and its end is marked when the first segment after it is written, by `SCTE35_TAGS` as `EXT-X-DATERANGE`
(`SCTE35-OUT`/`SCTE35-IN`), `EXT-X-CUE-OUT/IN` or both. The boundaries are taken from the schedule (`BreakStart`, `BreakDurS`), not from the segments that
are played: the ads may fill the break with segments from several creatives and a restart may begin in the middle of one. A clip in the middle of a break after a restart is
not a start, and the restore parses the old tags from the playlist file so that the end marker is not lost. The splice_insert is a real one with CRC-32/MPEG-2, checked against the vector in the spec.
A message that arrived through `POST /scte35` keeps its own bytes in the OUT marker.
