#!/usr/bin/env bash
# PROTOTYPE driver — throwaway. Stands in for the Go orchestrator.
#   ./proto/go.sh nissessenap/tmp-test-repo 1
# Targets a local single-node k3s (was kind; k3s is what has gVisor).
set -euo pipefail

REPO=${1:?usage: go.sh <owner/repo> <issue-number>}
ISSUE=${2:?usage: go.sh <owner/repo> <issue-number>}
DIR=$(cd "$(dirname "$0")" && pwd)
IMG=implementer-proto:dev
NS=implementer-proto
JOB="proto-issue-${ISSUE}${JOB_SUFFIX:-}"

# PREFLIGHT_ONLY=1 runs the probes and stops — no agent, no cost, no creds needed.
# SECCOMP=RuntimeDefault|Unconfined to compare profiles (bubblewrap cares).
# ROOT=1 runs as uid 0 — the gVisor posture, where the sandbox is the boundary
#   rather than the UID. Root inside a gVisor sandbox is not root on the host.
# RUNTIME=<class>|none picks the RuntimeClass explicitly instead of autodetecting.
PREFLIGHT_ONLY=${PREFLIGHT_ONLY:-}
SECCOMP=${SECCOMP:-}
ROOT=${ROOT:-}
RUNTIME=${RUNTIME:-}

# ---------------------------------------------------------- cluster safety ---
# This laptop's ~/.kube/config points at real GKE clusters. A prototype that
# applies Jobs must not be one `kubectl config use-context` away from prod, so
# it brings its own kubeconfig and refuses any API server that is not loopback.
if [[ -z ${KUBECONFIG:-} ]]; then
  KUBECONFIG="$DIR/.k3s.kubeconfig"
  if [[ ! -r $KUBECONFIG ]]; then
    sudo cat /etc/rancher/k3s/k3s.yaml > "$KUBECONFIG"
    chmod 600 "$KUBECONFIG"
  fi
fi
export KUBECONFIG
SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
case $SERVER in
  https://127.0.0.1:* | https://localhost:* | 'https://[::1]:'*) ;;
  *) echo "REFUSING: API server '$SERVER' is not local k3s" >&2; exit 1 ;;
esac
echo "==> cluster $SERVER"

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

echo "==> import into k3s containerd"
# k3s ctr defaults to the k8s.io namespace, which is the one the kubelet reads.
# docker save names it docker.io/library/$IMG, which is what the PodSpec resolves.
docker save "$IMG" | sudo k3s ctr images import - >/dev/null

echo "==> namespace + secret"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$NS" delete secret proto-creds --ignore-not-found >/dev/null
kubectl -n "$NS" create secret generic proto-creds \
  --from-literal=CLAUDE_CODE_OAUTH_TOKEN="${CLAUDE_CODE_OAUTH_TOKEN:-}" \
  --from-literal=GH_TOKEN="$GH_TOKEN_VALUE" >/dev/null

# The pod securityContext goes in as one flow mapping, so switching UID or
# seccomp never turns into a YAML-indentation problem in sed.
# An emptyDir mounts root:root 0755; fsGroup is what makes it writable by a
# non-root user, which is how the UID stays a PodSpec field. Root needs none.
if [[ -n $ROOT ]]; then
  POD_SECURITY='runAsNonRoot: false, runAsUser: 0, runAsGroup: 0'
else
  POD_SECURITY='runAsNonRoot: true, runAsUser: 1000, runAsGroup: 1000, fsGroup: 1000'
fi
[[ -n $SECCOMP ]] && POD_SECURITY="$POD_SECURITY, seccompProfile: { type: $SECCOMP }"
POD_SECURITY="{ $POD_SECURITY }"

# k3s ≥1.26 autodetects runtimes on the host and generates a RuntimeClass named
# after the containerd handler — so gVisor lands as `runsc`, not `gvisor`.
# Only use one that exists: a missing RuntimeClass leaves the Job Pending forever
# and tells us nothing.
if [[ $RUNTIME == none ]]; then
  RUNTIME_CLASS='# runtime class explicitly disabled (RUNTIME=none)'
  echo "==> no RuntimeClass (host runc)"
else
  for rc in ${RUNTIME:-runsc gvisor}; do
    if kubectl get runtimeclass "$rc" >/dev/null 2>&1; then FOUND=$rc; break; fi
  done
  if [[ -n ${FOUND:-} ]]; then
    RUNTIME_CLASS="runtimeClassName: $FOUND"
    echo "==> RuntimeClass $FOUND"
  elif [[ -n $RUNTIME ]]; then
    echo "REFUSING: RuntimeClass '$RUNTIME' not in this cluster" >&2; exit 1
  else
    RUNTIME_CLASS='# no gVisor RuntimeClass in this cluster'
    echo "==> no runsc/gvisor RuntimeClass — running on host runc"
  fi
fi

echo "==> apply job $JOB"
kubectl -n "$NS" delete job "$JOB" --ignore-not-found >/dev/null
sed -e "s|__JOB__|$JOB|" -e "s|__IMG__|$IMG|" -e "s|__REPO__|$REPO|" \
    -e "s|__ISSUE__|$ISSUE|" -e "s|__RUNTIME_CLASS__|$RUNTIME_CLASS|" \
    -e "s|__POD_SECURITY__|$POD_SECURITY|" -e "s|__PREFLIGHT_ONLY__|$PREFLIGHT_ONLY|" \
    -e "s|__BRANCH_SUFFIX__|${BRANCH_SUFFIX:-}|" \
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

# `logs -f` returns as soon as the stream closes, which is before the kubelet has
# written .state.terminated — reading the result channel straight after is a race
# that silently reports an empty termination log.
for _ in $(seq 30); do
  case $(kubectl -n "$NS" get "$POD" -o jsonpath='{.status.phase}') in
    Succeeded | Failed) break ;;
  esac
  sleep 2
done

echo
echo "==================== RESULT ===================="
kubectl -n "$NS" get "$POD" -o jsonpath='{range .status.containerStatuses[*]}exitCode: {.state.terminated.exitCode}{"\n"}reason:   {.state.terminated.reason}{"\n"}imageID:  {.imageID}{"\n"}termination-log:{"\n"}{.state.terminated.message}{"\n"}{end}'
echo "================================================"
echo "raw stream:  kubectl -n $NS logs $POD | jq -c 'select(.type==\"assistant\")'"
