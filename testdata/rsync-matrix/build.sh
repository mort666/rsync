#!/bin/sh
# Build pinned rsync releases for interop testing against several protocol
# versions. Each build ends up at $RSYNC_MATRIX_DIR/<name>/bin/rsync, ready to
# be put first on PATH by run-tests.sh.
#
# Usage: build.sh [name ...]   (default: every entry in versions.txt)
#
# Release tarballs come from download.samba.org and are checked against the
# SHA-256 sums pinned in versions.txt. "master" is a shallow git clone of
# RsyncProject/rsync. "openrsync" is a git clone of kristapsdz/openrsync and
# needs bmake.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
dir=${RSYNC_MATRIX_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/gokrazy-rsync-matrix}
jobs=${JOBS:-$(nproc 2>/dev/null || echo 4)}
mkdir -p "$dir/src" "$dir/log"

# Releases before 3.4 predate C23 (where "f()" means "f(void)"), and the
# oldest also predate GCC 14's stricter defaults. Ubuntu's GCC defaults to
# _FORTIFY_SOURCE=3, which aborts rsync 3.1.3 as a server ("buffer overflow
# detected"); level 2, as Arch and most distributions use, does not.
new_cflags="-O2 -std=gnu17"
old_cflags="-O2 -std=gnu89 -fcommon -Wno-implicit-function-declaration -Wno-implicit-int -Wno-incompatible-pointer-types -Wno-int-conversion -Wno-return-mismatch -U_FORTIFY_SOURCE -D_FORTIFY_SOURCE=2"

build_release() {
  name=$1 version=$2 sum=$3 kind=$4
  out="$dir/$name"
  if [ -x "$out/bin/rsync" ]; then
    echo "$name: already built"
    return 0
  fi
  tarball="$dir/src/rsync-$version.tar.gz"
  if [ ! -f "$tarball" ]; then
    echo "$name: downloading rsync-$version.tar.gz"
    curl -sSfL -o "$tarball.part" "https://download.samba.org/pub/rsync/src/rsync-$version.tar.gz"
    mv "$tarball.part" "$tarball"
  fi
  got=$(sha256sum "$tarball" | cut -d' ' -f1)
  if [ "$got" != "$sum" ]; then
    echo "$name: SHA-256 mismatch for $tarball: got $got, want $sum" >&2
    return 1
  fi
  build=$(mktemp -d "$dir/src/build-$name.XXXXXX")
  tar -xzf "$tarball" -C "$build"
  cd "$build/rsync-$version"
  case $kind in
    old) CFLAGS="$old_cflags" ./configure --prefix="$out" --with-included-popt ;;
    new) CFLAGS="$new_cflags" ./configure --prefix="$out" --disable-md2man ;;
  esac
  make -j"$jobs"
  mkdir -p "$out/bin"
  cp rsync "$out/bin/rsync"
  cd "$here"
  echo "$name: built $("$out/bin/rsync" --version | head -1)"
}

build_master() {
  out="$dir/master"
  src="$dir/src/rsync-master"
  if [ ! -d "$src/.git" ]; then
    git clone -q --depth 1 https://github.com/RsyncProject/rsync "$src"
  else
    git -C "$src" pull -q --ff-only
  fi
  cd "$src"
  if [ ! -f Makefile ] || [ configure.ac -nt Makefile ]; then
    ./configure --prefix="$out" --disable-md2man
  fi
  make -j"$jobs" rsync
  mkdir -p "$out/bin"
  cp rsync "$out/bin/rsync"
  cd "$here"
  echo "master: built $("$out/bin/rsync" --version | head -1) at $(git -C "$src" rev-parse --short HEAD)"
}

build_openrsync() {
  if ! command -v bmake >/dev/null; then
    echo "openrsync: skipped, needs bmake (Arch: pacman -S bmake, Debian: apt install bmake)"
    return 0
  fi
  out="$dir/openrsync"
  src="$dir/src/openrsync"
  if [ ! -d "$src/.git" ]; then
    git clone -q --depth 1 https://github.com/kristapsdz/openrsync "$src"
  fi
  cd "$src"
  ./configure PREFIX="$out"
  bmake -j"$jobs"
  mkdir -p "$out/bin"
  cp openrsync "$out/bin/rsync"
  cd "$here"
  echo "openrsync: built at $(git -C "$src" rev-parse --short HEAD)"
}

want="$*"
failed=""
while read -r name version sum kind; do
  case $name in ''|'#'*) continue ;; esac
  if [ -n "$want" ] && ! echo " $want " | grep -q " $name "; then
    continue
  fi
  log="$dir/log/$name.log"
  case $kind in
    git) fn=build_master ;;
    openrsync) fn=build_openrsync ;;
    *) fn="build_release $name $version $sum $kind" ;;
  esac
  # Run outside any "if" or "||": POSIX shells ignore set -e inside a
  # condition, which would let a failed make look like success.
  set +e
  (set -e; eval "$fn") >"$log" 2>&1
  rc=$?
  set -e
  if [ "$rc" -ne 0 ]; then
    echo "$name: FAILED, see $log"
    failed="$failed $name"
  else
    tail -1 "$log"
  fi
done <"$here/versions.txt"

if [ -n "$failed" ]; then
  echo "failed:$failed" >&2
  exit 1
fi
