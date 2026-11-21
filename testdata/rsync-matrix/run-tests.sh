#!/bin/sh
# Run the test suite once per rsync build from build.sh, with that build first
# on PATH. internal/rsynctest finds rsync on PATH, and so do the tests that
# exec "rsync" directly, so no test needs to know about the matrix.
#
# Usage: run-tests.sh [-n name[,name...]] [go test flags and packages]
#   default packages: ./...
#
# Logs go to $RSYNC_MATRIX_DIR/test-logs/<name>.log and a summary is printed.
set -eu

dir=${RSYNC_MATRIX_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/gokrazy-rsync-matrix}
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)

only=""
if [ "${1:-}" = "-n" ]; then
  only=$(echo "$2" | tr ',' ' ')
  shift 2
fi

# Builds whose failures are reported but do not fail the run. Most of the
# suite drives "rsync" with tridge rsync's command line, which openrsync does
# not fully accept (see gokrazy/rsync#33), so its result is informational.
informational="openrsync"

if [ $# -eq 0 ]; then
  set -- ./...
fi

mkdir -p "$dir/test-logs"
summary=""
status=0
while read -r name _ _ _; do
  case $name in ''|'#'*) continue ;; esac
  if [ -n "$only" ] && ! echo " $only " | grep -q " $name "; then
    continue
  fi
  bin="$dir/$name/bin"
  if [ ! -x "$bin/rsync" ]; then
    summary="$summary$(printf '%-12s not built\n' "$name")
"
    continue
  fi
  version=$("$bin/rsync" --version 2>&1 | head -1)
  log="$dir/test-logs/$name.log"
  echo "== $name: $version"
  set +e
  (cd "$root" && PATH="$bin:$PATH" go test -count=1 -timeout 10m "$@") >"$log" 2>&1
  rc=$?
  set -e
  ok=$(grep -c '^ok ' "$log" || true)
  bad=$(grep '^FAIL[[:space:]]' "$log" | awk '{print $2}' | sed 's|github.com/gokrazy/rsync/||' | tr '\n' ' ')
  if [ "$rc" -eq 0 ]; then
    result="PASS ($ok packages)"
  elif echo " $informational " | grep -q " $name "; then
    result="FAIL (informational): $bad"
  else
    result="FAIL: $bad"
    status=1
  fi
  summary="$summary$(printf '%-12s %s\n' "$name" "$result")
"
done <"$here/versions.txt"

echo
echo "Summary (logs in $dir/test-logs):"
printf '%s' "$summary"
exit $status
