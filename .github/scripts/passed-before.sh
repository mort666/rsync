#!/bin/sh
# Usage: passed-before.sh <workflow file>
#
# For a pull request from a branch of this repository, says skip=true
# in $GITHUB_OUTPUT, as the branch's push run covers it. On a push to
# master, says skip=true in $GITHUB_OUTPUT when this workflow
# already passed on the same files: the same commit (a fast-forward
# merge), or for a merge commit whose tree is its second parent's (a
# branch merged while up to date with master), that parent. Anything else
# says skip=false, and the workflow runs as usual.
set -eu
workflow=$1
out=${GITHUB_OUTPUT:-/dev/stdout}

# A pull request from a branch of this repository has a push run of its
# own; a fork's has not, and runs.
if [ "${GITHUB_EVENT_NAME:-}" = pull_request ] && [ "${PR_HEAD_REPO:-}" = "${GITHUB_REPOSITORY:-}" ]; then
  echo "the push run of this branch covers it"
  echo skip=true >>"$out"
  exit 0
fi

if [ "${GITHUB_EVENT_NAME:-}" != push ] || [ "${GITHUB_REF:-}" != refs/heads/master ]; then
  echo skip=false >>"$out"
  exit 0
fi

candidates=$(git rev-parse HEAD)
if git rev-parse -q --verify 'HEAD^2' >/dev/null &&
  [ "$(git rev-parse 'HEAD^{tree}')" = "$(git rev-parse 'HEAD^2^{tree}')" ]; then
  candidates="$candidates $(git rev-parse 'HEAD^2')"
fi

for sha in $candidates; do
  runs=$(gh api "repos/$GITHUB_REPOSITORY/actions/workflows/$workflow/runs?head_sha=$sha&status=success" \
    -q "[.workflow_runs[] | select(.id != ${GITHUB_RUN_ID:-0})] | .[0].html_url // empty") || runs=
  if [ -n "$runs" ]; then
    echo "$workflow already passed on these files: $runs"
    echo skip=true >>"$out"
    exit 0
  fi
done
echo "no earlier passing $workflow run on these files"
echo skip=false >>"$out"
