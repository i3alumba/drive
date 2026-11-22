# Provenance: native FFmpeg 5.1.9 HLS fixture

Checked-in synthetic regression fixture for the bounded metadata compatibility
exception in `preflight.go` (`nativeMetadataHex`).

- Source: synthetic `lavfi` input only; no real media.
- Producer: native ARM64 FFmpeg 5.1.9-0+deb12u1 (`Lavf59.27.100`), runtime image
  digest
  `sha256:1d5fc53e8b65a9a0bc9b1d456591f604e6131f983889a6e22007cd0b3b221d16`.
- Exact producer argv/log: `diagnostic.py` / log retained in the parent
  qualification directory `/tmp/drive-video-qualification-268/`
  (`exact-hls-artifacts/exact-hls-diagnostic`); the producer run exited 0 with
  empty stderr.
- The `moov/udta` box of `init.mp4` is the exact 98-byte native FFmpeg 5.1.9
  shape (`ilst/©too/data` = `Lavf59.27.100`) accepted as an explicit,
  byte-exact compatibility exception.

SHA-256 of the checked-in files:

- `init.mp4`: `e9afa40d179c8dc46d6a013158c2bc375cd4cec4d71bf8c8aefca335d0f11c38`
- `segment-000000.m4s`: `50d44282a0e2f3b89e0a6821179497ec128120c2fdd9d437d40b2e4a9eaaf39d`
- `segment-000001.m4s`: `12595fd748367af9ae0776421426d0ec933d38155101be46fc1a4f68bf54b07e`
- `segment-000002.m4s`: `3ccc77b9bb501c4acdade54778a8392bda554b2bcf4d8d6c853cb991f285773f`

The playlist (`stream.m3u8`, three 4.000000 s segments, independent segments)
is not checked in; durations are asserted by the regression test instead.
