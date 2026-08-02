# PROTOTYPE — sourced, not executed. Exports a KUBECONFIG and refuses anything
# that is not the local k3s.
#
# This laptop's ~/.kube/config points at real GKE clusters. A prototype that
# applies Jobs must not be one `kubectl config use-context` away from prod, so it
# brings its own kubeconfig and refuses any API server that is not loopback.
# Lives in one file because a safety check duplicated is a safety check that rots.

if [[ -z ${KUBECONFIG:-} ]]; then
  KUBECONFIG="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/.k3s.kubeconfig"
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
