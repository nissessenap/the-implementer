# shellcheck shell=bash
# Sourced by every e2e stage, not executed.
#
# The e2e is staged by the credentials each stage needs, so later tickets slot in
# rather than retrofit: unauthenticated stages run on every PR including forks,
# and a stage needing a GitHub App or GCP calls `requires` and skips cleanly when
# its secrets are absent. Skipping is exit 0 on purpose — a fork PR is green,
# not red, for credentials it was never going to have.

NS=${NS:-implementer-e2e}
E2E_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# `make kind-up` writes its own kubeconfig rather than touching ~/.kube/config,
# which on a developer laptop points at a real cluster. A local k3s needs no
# target at all: export KUBECONFIG and the harness does not care which flavour it
# is talking to.
if [[ -z ${KUBECONFIG:-} && -r "$E2E_DIR/../.kind.kubeconfig" ]]; then
  export KUBECONFIG="$E2E_DIR/../.kind.kubeconfig"
fi

SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
if [[ ${E2E_ALLOW_REMOTE:-0} != 1 ]]; then
  case $SERVER in
    https://127.0.0.1:* | https://localhost:* | 'https://[::1]:'*) ;;
    *)
      echo "REFUSING: API server '$SERVER' is not local." >&2
      echo "  kind: make kind-up. k3s: export KUBECONFIG. Deliberate: E2E_ALLOW_REMOTE=1." >&2
      exit 1
      ;;
  esac
fi

stage() { printf '\n=== %s: %s\n' "$(basename "$0")" "$*" >&2; }

requires() {
  local v
  for v in "$@"; do
    if [[ -z ${!v:-} ]]; then
      echo "SKIP $(basename "$0"): $v is not set" >&2
      exit 0
    fi
  done
}
