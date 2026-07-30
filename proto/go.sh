#!/usr/bin/env bash
# PROTOTYPE driver — throwaway. Stands in for the Go orchestrator.
#   ./proto/go.sh nissessenap/tmp-test-repo 1
set -euo pipefail

REPO=${1:?usage: go.sh <owner/repo> <issue-number>}
ISSUE=${2:?usage: go.sh <owner/repo> <issue-number>}
DIR=$(cd "$(dirname "$0")" && pwd)
IMG=implementer-proto:dev
NS=implementer-proto
JOB="proto-issue-${ISSUE}${JOB_SUFFIX:-}"

# PREFLIGHT_ONLY=1 runs the probes and stops — no agent, no cost, no creds needed.
# SECCOMP=RuntimeDefault|Unconfined to compare profiles (bubblewrap cares).
PREFLIGHT_ONLY=${PREFLIGHT_ONLY:-}
SECCOMP=${SECCOMP:-}

# Credentials. Never baked, always injected — the one ADR 0001 rule the prototype
# has no excuse to break.
if [[ -z "${CLAUDE_CODE_OAUTH_TOKEN:-}" && -r "$DIR/.token" ]]; then
  CLAUDE_CODE_OAUTH_TOKEN=$(tr -d '[:space:]' < "$DIR/.token")
fi
if [[ -z "$PREFLIGHT_ONLY" ]]; then
  : "${CLAUDE_CODE_OAUTH_TOKEN:?export it, or write it to proto/.token — get one with: claude setup-token}"
fi
GH_TOKEN_VALUE=${GH_PAT:-$(gh auth token)}

echo "==> build"
docker build -q -t "$IMG" "$DIR"

echo "==> load into kind"
kind load docker-image "$IMG"

echo "==> namespace + secret"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$NS" delete secret proto-creds --ignore-not-found >/dev/null
kubectl -n "$NS" create secret generic proto-creds \
  --from-literal=CLAUDE_CODE_OAUTH_TOKEN="${CLAUDE_CODE_OAUTH_TOKEN:-}" \
  --from-literal=GH_TOKEN="$GH_TOKEN_VALUE" >/dev/null

if [[ -n "$SECCOMP" ]]; then
  SECCOMP_YAML="seccompProfile: { type: $SECCOMP }"
else
  SECCOMP_YAML="# seccompProfile unset (kubelet default)"
fi

# gVisor only if the cluster actually has the RuntimeClass — otherwise the Job
# would sit Pending forever and tell us nothing.
if kubectl get runtimeclass gvisor >/dev/null 2>&1; then
  RUNTIME_CLASS='runtimeClassName: gvisor'
  echo "==> gvisor RuntimeClass present — using it"
else
  RUNTIME_CLASS='# no gvisor RuntimeClass in this cluster'
  echo "==> no gvisor RuntimeClass — running without it (issue #22 stays half-open)"
fi

echo "==> apply job $JOB"
kubectl -n "$NS" delete job "$JOB" --ignore-not-found >/dev/null
sed -e "s|__JOB__|$JOB|" -e "s|__IMG__|$IMG|" -e "s|__REPO__|$REPO|" \
    -e "s|__ISSUE__|$ISSUE|" -e "s|__RUNTIME_CLASS__|$RUNTIME_CLASS|" \
    -e "s|__SECCOMP__|$SECCOMP_YAML|" -e "s|__PREFLIGHT_ONLY__|$PREFLIGHT_ONLY|" \
    "$DIR/job.yaml" | kubectl apply -f - >/dev/null

echo "==> waiting for pod"
for _ in $(seq 60); do
  POD=$(kubectl -n "$NS" get pod -l job-name="$JOB" -o name 2>/dev/null | head -1)
  [[ -n "$POD" ]] && break
  sleep 2
done
[[ -n "${POD:-}" ]] || { echo "no pod appeared"; exit 1; }

kubectl -n "$NS" wait --for=condition=Ready "$POD" --timeout=180s 2>/dev/null || true
echo "==> logs ($POD)"
kubectl -n "$NS" logs -f "$POD" || true

echo
echo "==================== RESULT ===================="
kubectl -n "$NS" get "$POD" -o jsonpath='{range .status.containerStatuses[*]}exitCode: {.state.terminated.exitCode}{"\n"}reason:   {.state.terminated.reason}{"\n"}imageID:  {.imageID}{"\n"}termination-log:{"\n"}{.state.terminated.message}{"\n"}{end}'
echo "================================================"
echo "raw stream:  kubectl -n $NS logs $POD | jq -c 'select(.type==\"assistant\")'"
