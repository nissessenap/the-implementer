#!/usr/bin/env bash
# PROTOTYPE — throwaway. Build + deploy the credential proxy into local k3s.
#   ./proto/proxy/up.sh
# Called automatically by `VERTEX=1 ./proto/go.sh`; standalone for iterating.
set -euo pipefail

DIR=$(cd "$(dirname "$0")" && pwd)
IMG=implementer-proto-proxy:dev
NS=implementer-proto
# shellcheck source=../kubeconf.sh
source "$DIR/../kubeconf.sh"

# The proxy's own credential. On k3s there is no metadata server, so it cannot use
# Workload Identity — it holds a key file. That shortcut is allowed for the *proxy*
# and must never leak to the sandbox side (issue #33).
ADC=${GOOGLE_APPLICATION_CREDENTIALS:-$HOME/.config/gcloud/application_default_credentials.json}
[[ -r $ADC ]] || { echo "no ADC at $ADC — run: gcloud auth application-default login" >&2; exit 1; }
# A user ADC credential needs a quota project; a service account key does not.
QUOTA_PROJECT=${VERTEX_PROJECT:-}
grep -q '"type": *"authorized_user"' "$ADC" || QUOTA_PROJECT=""

# Issue #34: the proxy also terminates GitHub's TLS, so it needs a cert for
# github.com and the real installation token. Both live here and only here.
GH_TOKEN_SENTINEL=${GH_TOKEN_SENTINEL:-proxy-injected}
GH_TOKEN_VALUE=${GH_PAT:-$(gh auth token)}

echo "==> build proxy"
docker build -q -t "$IMG" "$DIR"
docker save "$IMG" | sudo k3s ctr images import - >/dev/null

"$DIR/ca.sh"

kubectl -n "$NS" delete secret proto-adc --ignore-not-found >/dev/null
kubectl -n "$NS" create secret generic proto-adc --from-file=adc.json="$ADC" >/dev/null
kubectl -n "$NS" create secret generic proto-gh \
  --from-literal=GH_TOKEN="$GH_TOKEN_VALUE" \
  --from-literal=GH_TOKEN_SENTINEL="$GH_TOKEN_SENTINEL" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

sed -e "s|__IMG__|$IMG|" -e "s|__QUOTA_PROJECT__|$QUOTA_PROJECT|" \
    "$DIR/proxy.yaml" | kubectl apply -f - >/dev/null
# The image tag never changes, so apply alone would leave the old pod running.
kubectl -n "$NS" rollout restart deploy/proto-proxy >/dev/null
kubectl -n "$NS" rollout status deploy/proto-proxy --timeout=120s
echo "==> proxy up at http://proto-proxy.$NS.svc.cluster.local:8080"
kubectl -n "$NS" logs deploy/proto-proxy --tail=5
