### Added

- `mp4.Defragment` and `mp4.DefragmentTracks` keep the sample groups and sample dependencies of
  the fragmented input: `sbgp` and `sgpd` boxes from the sample table and from every track fragment
  merge into the progressive sample table, identical descriptions are written once, and each
  surviving sample points at its description, so pre-roll and random-access recovery points and
  leading-frame flags survive. Leading, dependency and redundancy flags are written as an `sdtp`
  box. Descriptions of superseded samples and of dropped tracks are not written, and input without
  sample groups or dependency information gets no such boxes
