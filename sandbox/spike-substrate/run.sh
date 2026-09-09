#!/usr/bin/env bash
# The whole driver for the substrate spike. One command, start to finish:
# build the actor image, ensure the pool/atespace/template, create one Actor for
# the issue, POST the run through atenet-router, print the result blob, delete
# the Actor.
#
# There is no orchestrator change anywhere in this spike. If the answer comes
# back yes, the create-and-POST moves into `orchestrator run` and reuses the
# informer's comment writer; until then this script is the whole of it.
#
# ponytail: throwaway. No retry policy, no PR, no issue comment — the blob on
# stdout is the answer the spike is for.
#
# Usage: ./run.sh owner/repo#42 [toolchain]
set -o errexit -o nounset -o pipefail

TARGET="${1:?usage: run.sh owner/repo#N [toolchain]}"
REPO="${TARGET%%#*}"
ISSUE="${TARGET##*#}"
TOOLCHAIN="${2:-go}"
[[ "$REPO" == */* && "$ISSUE" =~ ^[0-9]+$ ]] || { echo "bad target: $TARGET" >&2; exit 1; }

SUBSTRATE_DIR="${SUBSTRATE_DIR:-$HOME/projects/oss/substrate}"
ATESPACE="${ATESPACE:-implementer-spike}"
BUCKET_NAME="${BUCKET_NAME:-ate-snapshots}"
REGISTRY="${REGISTRY:-localhost:5001}"
BASE="${BASE:-ghcr.io/nissessenap/implementer-base:dev}"
ROUTER_PORT="${ROUTER_PORT:-8000}"
CTX="${KUBECTL_CONTEXT:-kind-kind}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Never a real cluster. The developer laptop's current-context is a production
# GKE cluster and every kubectl below is explicit about where it lands.
[[ "$CTX" == kind-* ]] || { echo "refusing: KUBECTL_CONTEXT=$CTX is not a kind context" >&2; exit 1; }
k() { kubectl --context "$CTX" "$@"; }

GH_TOKEN="${GH_TOKEN:-$(gh auth token)}"
: "${CLAUDE_CODE_OAUTH_TOKEN:?CLAUDE_CODE_OAUTH_TOKEN required — no proxy in this spike}"

# ADR 0004's swallowed AlreadyExists, for the objects that are per-toolchain
# rather than per-run. The Actor create below deliberately does NOT use this:
# there, a collision IS the idempotency mechanism.
ensure() {
  local out
  if ! out="$("$@" 2>&1)"; then
    case "$out" in
      *AlreadyExists*|*already\ exists*|*ALREADY_EXISTS*) echo "  (exists)" ;;
      *) echo "$out" >&2; return 1 ;;
    esac
  fi
}

echo "== build + push the actor image (digest-pinned; kind load gives no digest)"
docker build --build-arg "BASE=$BASE" -t "$REGISTRY/implementer-spike:dev" "$HERE"
docker push "$REGISTRY/implementer-spike:dev" >/dev/null
WORKLOAD_IMAGE="$(docker inspect --format '{{index .RepoDigests 0}}' "$REGISTRY/implementer-spike:dev")"
echo "   $WORKLOAD_IMAGE"

echo "== worker pool (ko resolves the ateom-gvisor worker image)"
(cd "$SUBSTRATE_DIR" && KO_DOCKER_REPO="$REGISTRY" KO_DEFAULTPLATFORMS="linux/$(go env GOARCH)" \
  hack/run-tool.sh ko resolve -f - < "$HERE/workerpool.yaml") | k apply -f -

echo "== atespace + actor template"
ensure k ate create atespace "$ATESPACE"
ATESPACE="$ATESPACE" TOOLCHAIN="$TOOLCHAIN" WORKLOAD_IMAGE="$WORKLOAD_IMAGE" BUCKET_NAME="$BUCKET_NAME" \
  envsubst < "$HERE/template.yaml.tmpl" > /tmp/implementer-spike-template.yaml
ensure k ate create actor-template -f /tmp/implementer-spike-template.yaml
echo "   (first create builds the golden snapshot — minutes)"

echo "== actor issue-$ISSUE"
ACTOR="issue-$ISSUE"
# No swallow. A second label of the same issue collides here, which is exactly
# the deterministic-name idempotency ADR 0004 gets from the Job name.
k ate create actor "$ACTOR" -a "$ATESPACE" --template "implementer-$TOOLCHAIN"
cleanup() {
  echo "== delete actor (--any-state: a RUNNING actor is not deletable otherwise)"
  k ate delete actor "$ACTOR" -a "$ATESPACE" --any-state || true
  [[ -n "${PF_PID:-}" ]] && kill "$PF_PID" 2>/dev/null || true
}
trap cleanup EXIT

k port-forward -n ate-system svc/atenet-router "$ROUTER_PORT:80" >/dev/null &
PF_PID=$!

echo "== wait for the actor to answer /readyz (cold gVisor boot + image unpack)"
for i in $(seq 1 120); do
  curl -sf -o /dev/null -m 10 -H "ate-target-actor: $ATESPACE/$ACTOR" \
    "http://localhost:$ROUTER_PORT/readyz" && break
  [[ $i -eq 120 ]] && { echo "actor never became ready" >&2; exit 1; }
  sleep 5
done

echo "== POST /run  (~450s, ~\$2 — one shot, no retry)"
# Deliberately un-retried, for ADR 0002's reason for backoffLimit: 0 — a retry
# is a second *paid* run against an unchanged issue, and the clone phase would
# fail anyway because the branch now exists remotely. The body goes over a pipe
# rather than a temp file so the two tokens never touch the disk.
#
# The only deadline there is. Substrate has no activeDeadlineSeconds equivalent
# and --max-time does not stop the actor — the trap's delete does.
jq -n --arg r "$REPO" --arg i "$ISSUE" --arg t "$TOOLCHAIN" \
      --arg gh "$GH_TOKEN" --arg cc "$CLAUDE_CODE_OAUTH_TOKEN" \
      '{repo:$r, issue:$i, toolchain:$t, gh_token:$gh, claude_token:$cc}' \
  | curl -sS --fail-with-body --max-time "${RUN_TIMEOUT:-2400}" \
      -H "ate-target-actor: $ATESPACE/$ACTOR" \
      -H 'content-type: application/json' --data @- \
      "http://localhost:$ROUTER_PORT/run" \
  | tee /tmp/implementer-spike-result.json | jq .

echo "== transcript: kubectl --context $CTX ate logs actors $ACTOR -a $ATESPACE"
