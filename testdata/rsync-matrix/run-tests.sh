#!/bin/sh
# Run the test suite once per rsync build from build.sh, with that build first
# on PATH. internal/rsynctest finds rsync on PATH, and so do the tests that
# exec "rsync" directly, so no test needs to know about the matrix.
#
# Usage: run-tests.sh [-n name[,name...]] [-p protocol] [go test flags and packages]
#   default packages: ./...
#   -p N runs every build with --protocol=N, through a wrapper script first on
#      PATH, so the whole suite talks protocol N to stock rsync. (Tests that
#      pass --protocol themselves still get theirs: the later flag wins.)
#
# Logs go to $RSYNC_MATRIX_DIR/test-logs/<name>.log and a summary is printed.
set -eu

dir=${RSYNC_MATRIX_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/gokrazy-rsync-matrix}
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)

only=""
protocol=""
while [ $# -gt 0 ]; do
  case $1 in
    -n) only=$(echo "$2" | tr ',' ' '); shift 2 ;;
    -p) protocol=$2; shift 2 ;;
    *) break ;;
  esac
done

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
  path=$bin
  label=$name
  if [ -n "$protocol" ]; then
    # A build older than the protocol cannot be forced up to it.
    speaks=$(echo "$version" | sed -n 's/.*protocol version \([0-9]*\).*/\1/p')
    if [ -n "$speaks" ] && [ "$speaks" -lt "$protocol" ]; then
      summary="$summary$(printf '%-15s skipped (speaks protocol %s at most)\n' "$name@$protocol" "$speaks")
"
      continue
    fi
    path="$dir/wrap/$name-p$protocol"
    mkdir -p "$path"
    printf '#!/bin/sh\nexec "%s/rsync" --protocol=%s "$@"\n' "$bin" "$protocol" >"$path/rsync"
    chmod +x "$path/rsync"
    label="$name@$protocol"
  fi
  log="$dir/test-logs/$label.log"
  echo "== $label: $version"
  set +e
  # RSYNCTEST_EXTRA_RODIRS lets sandboxed tests run the build behind a
  # wrapper. RSYNC_MATRIX_PROTOCOL tells tests which protocol the wrapper
  # forces: a stock daemon speaks it whatever its client asks for.
  (cd "$root" && PATH="$path:$PATH" RSYNCTEST_EXTRA_RODIRS="$bin" RSYNC_MATRIX_PROTOCOL="$protocol" go test -count=1 -timeout 10m "$@") >"$log" 2>&1
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
  summary="$summary$(printf '%-15s %s\n' "$label" "$result")
"
done <"$here/versions.txt"

echo
echo "Summary (logs in $dir/test-logs):"
printf '%s' "$summary"
exit $status
