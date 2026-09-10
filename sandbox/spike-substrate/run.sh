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
# phase.sh defaults to $10 per phase and there are three, so an unset budget is
# a $30 ceiling. A spike does not need the headroom a real run is allowed.
MAX_USD_PER_PHASE="${MAX_USD_PER_PHASE:-2}"
CTX="${KUBECTL_CONTEXT:-kind-kind}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Never a real cluster. The developer laptop's current-context is a production
# GKE cluster, so this does not merely *prefer* the kind one: it minifies the
# kubeconfig down to that single context and exports it, and a minified config
# cannot name any other cluster. Structural rather than a flag on every call —
# which also sidesteps kubectl refusing its own global flags before a plugin
# name, since `kubectl ate` takes --context only *after* the subcommand.
[[ "$CTX" == kind-* ]] || { echo "refusing: KUBECTL_CONTEXT=$CTX is not a kind context" >&2; exit 1; }
PINNED="$(mktemp -t implementer-spike-kubeconfig.XXXXXX)"
kubectl --context "$CTX" config view --minify --flatten > "$PINNED"
export KUBECONFIG="$PINNED"
k() { kubectl "$@"; }

# `kubectl ate` is a plugin nobody installs — substrate's own install-ate.sh
# shells out to `go run ./cmd/kubectl-ate`, so this does the same, and prefers a
# real plugin when one is on PATH. Its own --context flag would have to come
# *after* the subcommand, which is the other reason the pinned KUBECONFIG above
# does the context work instead of a flag.
if command -v kubectl-ate >/dev/null 2>&1; then
  ate() { kubectl-ate "$@"; }
else
  ate() { (cd "$SUBSTRATE_DIR" && go run ./cmd/kubectl-ate "$@"); }
fi

# One trap, installed before anything exists, so each half is guarded rather
# than assumed: an early failure must not try to delete an actor there is none of.
cleanup() {
  if [[ -n "${ACTOR:-}" && -z "${KEEP_ACTOR:-}" ]]; then
    echo "== delete actor (--any-state: a RUNNING actor is not deletable otherwise)"
    ate delete actor "$ACTOR" -a "$ATESPACE" --any-state || true
  elif [[ -n "${ACTOR:-}" ]]; then
    # KEEP_ACTOR: the actor's pod holds the transcript, and deleting the actor
    # wipes the worker with it. Set it when the run failed and the log is the
    # only evidence left.
    echo "== KEEP_ACTOR set, leaving $ACTOR alive. Transcript, then clean up:"
    echo "     go run ./cmd/kubectl-ate logs actors $ACTOR -a $ATESPACE   # from $SUBSTRATE_DIR"
    echo "     go run ./cmd/kubectl-ate delete actor $ACTOR -a $ATESPACE --any-state"
  fi
  [[ -n "${PF_PID:-}" ]] && kill "$PF_PID" 2>/dev/null || true
  rm -f "$PINNED"
}
trap cleanup EXIT

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

# An ActorTemplate is immutable, so a template named only for the toolchain
# would pin whatever image it was first created with and `ensure`'s swallowed
# AlreadyExists would hide that forever. Naming it after the digest makes a
# rebuilt image a *different* template, which cannot go stale — and it restores
# the digest honesty ADR 0003 gets from containerStatuses[].imageID: here the
# template name is the digest.
TEMPLATE="implementer-$TOOLCHAIN-$(echo "${WORKLOAD_IMAGE##*:}" | cut -c1-12)"

echo "== atespace + actor template ($TEMPLATE)"
ensure ate create atespace "$ATESPACE"
ATESPACE="$ATESPACE" TEMPLATE="$TEMPLATE" WORKLOAD_IMAGE="$WORKLOAD_IMAGE" BUCKET_NAME="$BUCKET_NAME" \
  envsubst < "$HERE/template.yaml.tmpl" > /tmp/implementer-spike-template.yaml
ensure ate create actor-template -f /tmp/implementer-spike-template.yaml
echo "   (first create builds the golden snapshot — minutes)"

echo "== actor issue-$ISSUE"
# No swallow. A second label of the same issue collides here, which is exactly
# the deterministic-name idempotency ADR 0004 gets from the Job name.
#
# ACTOR is set only *after* the create succeeds, so a collision leaves it empty
# and the trap does not delete the actor belonging to the run already in flight.
WANT="issue-$ISSUE"
if ! ate create actor "$WANT" -a "$ATESPACE" --template "$TEMPLATE"; then
  echo "refusing: actor $WANT exists — a run for this issue is already in flight" >&2
  exit 1
fi
ACTOR="$WANT"

# A leftover forward from an interrupted run answers on this port and the run
# would proceed through *its* tunnel — which is how a stale one silently served
# a previous attempt here. Refuse instead of guessing.
if (exec 3<>/dev/tcp/127.0.0.1/"$ROUTER_PORT") 2>/dev/null; then
  echo "refusing: port $ROUTER_PORT is already in use — stale port-forward?" >&2
  echo "  pkill -f 'port-forward.*atenet-router'" >&2
  exit 1
fi
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
      --arg b "$MAX_USD_PER_PHASE" \
      '{repo:$r, issue:$i, toolchain:$t, gh_token:$gh, claude_token:$cc,
        max_usd_per_phase:$b}' \
  | curl -sS --fail-with-body --max-time "${RUN_TIMEOUT:-2400}" \
      -H "ate-target-actor: $ATESPACE/$ACTOR" \
      -H 'content-type: application/json' --data @- \
      "http://localhost:$ROUTER_PORT/run" \
  | tee /tmp/implementer-spike-result.json | jq .

echo "== transcript: kubectl ate logs actors $ACTOR -a $ATESPACE --context $CTX"
