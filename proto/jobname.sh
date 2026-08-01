#!/usr/bin/env bash
# Prototype for #15: what does the apiserver actually accept as a Job name?
# The ticket asks whether `implementer-<owner>-<repo>-<issue>` survives the
# awkward cases. Rather than reason about DNS-1123 from memory, ask k3s.
#
# Dry-run only: no Job ever runs, so this is pure validation feedback.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [[ -z ${KUBECONFIG:-} ]]; then
  KUBECONFIG="$DIR/.k3s.kubeconfig"
  if [[ ! -r $KUBECONFIG ]]; then
    sudo cat /etc/rancher/k3s/k3s.yaml > "$KUBECONFIG"
    chmod 600 "$KUBECONFIG"
  fi
fi
export KUBECONFIG

SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
case "$SERVER" in
  https://127.0.0.1:*|https://localhost:*) ;;
  *) echo "REFUSING: API server '$SERVER' is not local k3s" >&2; exit 1 ;;
esac

try() { # try <label> <name>
  local out
  if out=$(kubectl create --dry-run=server -f - <<YAML 2>&1
apiVersion: batch/v1
kind: Job
metadata:
  name: $2
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: run
          image: busybox
YAML
  ); then
    printf 'ACCEPT  %-22s len=%-3d %s\n' "$1" "${#2}" "$2"
  else
    printf 'REJECT  %-22s len=%-3d %s\n' "$1" "${#2}" "$2"
    printf '        %s\n' "$(echo "$out" | tr '\n' ' ' | cut -c1-200)"
  fi
}

n() { printf 'a%.0s' $(seq "$1"); } # a string of N 'a's

echo "== length boundary =="
try "63 chars"       "$(n 63)"
try "64 chars"       "$(n 64)"
try "70 chars"       "$(n 70)"
try "253 chars"      "$(n 253)"

echo
echo "== character classes =="
try "uppercase owner"  "implementer-NisseSenap-the-implementer-15"
try "underscore repo"  "implementer-acme-my_repo-15"
try "dot in repo"      "implementer-acme-repo.js-15"
try "leading digit"    "9implementer-acme-repo-15"
try "trailing dash"    "implementer-acme-repo-"
try "double dash"      "implementer-acme--repo-15"

echo
echo "== realistic names =="
try "short"            "implementer-nissessenap-the-implementer-15"
try "long org/repo"    "implementer-kubernetes-sigs-cluster-api-provider-openstack-12345"
