#!/usr/bin/env bash
# PROTOTYPE driver — throwaway. The same phase.sh, no Kubernetes.
#   ./proto/local.sh nissessenap/tmp-test-repo 2
#
# proto/go.sh exists because #22/#33/#34 asked questions about the *posture* —
# gVisor, readOnlyRootFilesystem, the credential proxy. Issue #31 asks about the
# prompts, and none of that machinery bears on whether a review phase hangs. So
# this runs the identical script and image with `docker run` and nothing else.
set -euo pipefail

REPO=${1:?usage: local.sh <owner/repo> <issue-number>}
ISSUE=${2:?usage: local.sh <owner/repo> <issue-number>}
DIR=$(cd "$(dirname "$0")" && pwd)
IMG=implementer-proto:dev

[[ -z ${CLAUDE_CODE_OAUTH_TOKEN:-} && -r $DIR/.token ]] &&
  CLAUDE_CODE_OAUTH_TOKEN=$(tr -d '[:space:]' < "$DIR/.token")
: "${CLAUDE_CODE_OAUTH_TOKEN:?export it, or write it to proto/.token}"
export CLAUDE_CODE_OAUTH_TOKEN # `docker run -e NAME` reads the *exported* value

echo "==> build"
docker build -q -t "$IMG" "$DIR"

RUN=$DIR/.run
rm -rf "$RUN" && mkdir -p "$RUN" && chmod 0777 "$RUN"

echo "==> run (artifacts in proto/.run)"
# --user 1000 mirrors the PodSpec's UID cheaply. No read-only rootfs: docker's own
# seccomp profile already breaks bubblewrap, so the hardened posture is go.sh's job
# and #22 has already answered it. The workspace is a bind mount so the per-phase
# streams and the resulting clone survive the container.
docker run --rm --init \
  -e REPO="$REPO" -e ISSUE="$ISSUE" \
  -e TERM_LOG=/workspace/termination-log \
  -v "$RUN:/workspace" \
  -e TOOLCHAIN="${TOOLCHAIN:-}" \
  -e MAX_BUDGET_USD="${MAX_BUDGET_USD:-10}" \
  -e PREFLIGHT_ONLY="${PREFLIGHT_ONLY:-}" -e SMOKE="${SMOKE:-}" \
  -e BRANCH_SUFFIX="${BRANCH_SUFFIX:-}" \
  -e IS_SANDBOX=1 -e HOME=/home/agent -e WORKSPACE=/workspace \
  -e GIT_AUTHOR_NAME=the-implementer -e GIT_AUTHOR_EMAIL=the-implementer@users.noreply.github.com \
  -e GIT_COMMITTER_NAME=the-implementer -e GIT_COMMITTER_EMAIL=the-implementer@users.noreply.github.com \
  -e CLAUDE_CODE_OAUTH_TOKEN \
  -e GH_TOKEN="${GH_PAT:-$(gh auth token)}" \
  --user 1000:1000 \
  "$IMG"

echo
echo "==================== RESULT ===================="
jq . "$RUN/termination-log" 2>/dev/null || cat "$RUN/termination-log"
echo "================================================"
