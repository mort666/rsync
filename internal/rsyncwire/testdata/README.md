# Codec golden data

`codecs.golden` records what rsync's own varint, varlong and file-list index (ndx) functions from `io.c` produce for boundary values and malformed input. `TestCodecsGolden` checks the Go codecs in `varint.go` and `ndx.go` against it byte for byte.

Each line is a command and the result, separated by a tab. `wvarint 128` gives the bytes `write_varint(128)` writes, in hex. `rvarint 8080` gives the value `read_varint` returns and how many bytes it used, or `ERR` when it fails. `wndx` and `rndx` keep state from line to line, as the real functions do, until a `reset`.

The file was generated from rsync 3.5.2 with a small C harness that compiles those functions straight out of an rsync source tree. The harness is not part of this repository because it builds upstream (GPL) code. To regenerate the file, build the harness, then:

```bash
RSYNC_CODEC_HARNESS=/path/to/codec-harness go test ./internal/rsyncwire -run TestCodecsGolden -args -update
```

With `RSYNC_CODEC_HARNESS` set, `TestCodecsAgainstHarness` also runs 200,000 random encode and decode operations through both implementations and fails on the first difference. Without it, that test is skipped.

The codecs behave the same in rsync 3.0.9, 3.2.7, 3.4.1 and 3.5.2, with one exception. Since 3.4, `read_ndx` rejects an index above 2^31−1 instead of letting it wrap. The Go reader rejects it too.
