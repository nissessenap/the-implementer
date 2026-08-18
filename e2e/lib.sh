# shellcheck shell=bash
# Sourced by every e2e stage, not executed.

NS=${NS:-implementer-e2e}
E2E_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# `make kind-up` writes here; a local k3s just exports KUBECONFIG.
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
