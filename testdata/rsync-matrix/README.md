# rsync interop matrix

Scripts to build pinned rsync releases and run the test suite against each one. The point is to test gokrazy/rsync against every rsync protocol version it has to work with (29 to 33), not just whatever `rsync` the machine happens to have.

| Name | Protocol | Why it is in the matrix |
| ---- | -------- | ----------------------- |
| `rsync-2.6.9` | 29 | Newest 2.x release. Item flags, the protocol 29 sort order. |
| `rsync-3.0.9` | 30 | Varints, compat flags, two-way multiplexing, MD5. |
| `rsync-3.1.3` | 31 | Protocol 31 without checksum or compression negotiation. |
| `rsync-3.2.7` | 31 | Negotiation of checksums, compression and daemon auth digests. |
| `rsync-3.4.1` | 32 | First release with the January 2025 security fixes. |
| `rsync-3.4.3` | 32 | Rejects literal tokens over 32 KiB ([gokrazy/rsync#63](https://github.com/gokrazy/rsync/issues/63)). Kept to catch that class of bug. |
| `rsync-3.5.2` | 33 | Current release. |
| `master` | 33 | RsyncProject/rsync master, rebuilt from git on every run. |
| `openrsync` | 27 | kristapsdz/openrsync, the BSD implementation macOS ships. |

## Building

```bash
./testdata/rsync-matrix/build.sh
```

Builds go to `~/.cache/gokrazy-rsync-matrix/<name>/bin/rsync` (override with `RSYNC_MATRIX_DIR`). Release tarballs come from download.samba.org and are checked against the SHA-256 sums in `versions.txt`, which were taken from tarballs whose signatures verified against the release managers' keys. Pass names to build only some of them, for example `build.sh rsync-3.0.9 master`. Build logs go to `~/.cache/gokrazy-rsync-matrix/log/`.

What the builds need:

- A C compiler, `make`, `curl`, `git`.
- Headers for libacl, popt, OpenSSL, xxhash, zstd, lz4 and zlib, so the 3.2+ builds offer the same checksum and compression lists as distribution packages. On Arch these come with the normal packages. On Debian or Ubuntu install `libacl1-dev libpopt-dev libssl-dev libxxhash-dev libzstd-dev liblz4-dev zlib1g-dev`.
- `bmake`, for openrsync only. Without it openrsync is skipped.

Releases before 3.4 are built with `-std=gnu17`, and those before 3.2 with extra relaxed flags, because current GCC treats their old-style C as errors.

## Running the tests

```bash
./testdata/rsync-matrix/run-tests.sh
```

This runs `go test ./...` once per build, with that build first on `PATH`, and prints a pass or fail line per build. Logs are in `~/.cache/gokrazy-rsync-matrix/test-logs/`. To run only some builds or some packages:

```bash
./testdata/rsync-matrix/run-tests.sh -n rsync-3.0.9,openrsync ./integration/...
```

Each build normally negotiates the lower of its own version and gokrazy/rsync's `MaxProtocolVersion`, so as that rises most of the suite stops running the older encodings. `-p N` puts a wrapper first on `PATH` that runs every build with `--protocol=N`, so the whole suite talks protocol N:

```bash
./testdata/rsync-matrix/run-tests.sh -p 27
```

Tests that pass `--protocol` to rsync themselves keep theirs, since the later flag wins. Results are labelled `name@N`. The wrapper runs the build from another directory, which the sandboxed tests are allowed to execute through `RSYNCTEST_EXTRA_RODIRS`.

The `openrsync` result is informational: most of the suite drives `rsync` with tridge rsync's command line, which openrsync does not fully accept ([gokrazy/rsync#33](https://github.com/gokrazy/rsync/issues/33)), so its failures are listed but do not fail the run. Proper openrsync coverage needs tests written for its command line.

The same scripts run in CI as the `interop-matrix` job in `.github/workflows/main.yml`.
