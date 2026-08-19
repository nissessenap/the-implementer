#!/usr/bin/env bash
# Stage 70 — GAR credential injection. The fixture installs a private wheel holding
# no Google credential at all: not a token, and not even a sentinel, because `pip`
# sends Artifact Registry nothing and does not retry on a 401. The proxy attaches
# its own identity mid-flight.
#
# Credentials: the proxy's, and they are **Workload Identity** — there is no key
# file to mount, deliberately (a service account key in a Secret is the long-lived
# credential Workload Identity exists to delete). So this stage only runs against a
# cluster whose proxy pod can reach a metadata server: GKE, with
# E2E_ALLOW_REMOTE=1. On kind and k3s it skips, and proxy/gar_test.go proves the
# same path end to end there against a fake token source.
#
# It upgrades an already-installed release and does not install one, so stages 10
# to 30 have to have run against this cluster first — `make e2e` with the variables
# below set is the whole of it. Standalone, it fails with `release: not found`.
#
#   E2E_GAR_INDEX=https://europe-west1-python.pkg.dev/my-project/my-repo/simple/
#   E2E_GAR_PACKAGE=my-private-package
#   E2E_GAR_GSA=proxy@my-project.iam.gserviceaccount.com   (optional: sets the
#       Workload Identity annotation. Skip it if the proxy's KSA already carries
#       one. Either way that account needs roles/artifactregistry.reader.)
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"

if [[ -z ${E2E_GAR_INDEX:-} || -z ${E2E_GAR_PACKAGE:-} ]]; then
  stage "SKIPPED — set E2E_GAR_INDEX and E2E_GAR_PACKAGE to install a real private wheel"
  echo "  (proxy/gar_test.go proves the attach, the exclusion and a real interception offline)" >&2
  exit 0
fi

# The Docker control comes off the same index URL, so there is one thing to
# configure and the two endpoints cannot end up in different repositories.
PY_HOST=${E2E_GAR_INDEX#*://}
PY_HOST=${PY_HOST%%/*}
DOCKER_HOST=${PY_HOST/-python./-docker.}
[[ $DOCKER_HOST != "$PY_HOST" ]] || {
  echo "E2E_GAR_INDEX host '$PY_HOST' is not a {region}-python.pkg.dev endpoint" >&2
  exit 1
}

stage "turn on the Artifact Registry credential"
# --reuse-values, so whichever GitHub credential the stages before this one left
# installed stays installed: this stage adds one value and owns nothing else.
helm upgrade "$RELEASE" "$E2E_DIR/../charts/proxy" -n "$NS" --reuse-values \
  --set gar.enabled=true \
  ${E2E_GAR_GSA:+--set-string "serviceAccountAnnotations.iam\.gke\.io/gcp-service-account=$E2E_GAR_GSA"} \
  --wait --timeout=180s >/dev/null
# The proxy resolves and spends a token at boot, so a Google identity it cannot use
# is a pod that never goes ready — this rollout is itself the first assertion.
kubectl -n "$NS" rollout status "deploy/$RELEASE" --timeout=180s

stage "probe the injection (runtimeClassName=${RUNTIME_CLASS:-<none>})"
run_job e2e-gar "$E2E_DIR/gar-job.yaml" \
  "RUN_CRED=$(run_cred acme widgets 5 stage70)" \
  "INDEX=$E2E_GAR_INDEX" \
  "PACKAGE=$E2E_GAR_PACKAGE" \
  "DOCKER_HOST=$DOCKER_HOST"

echo
echo "==> proxy log (each pkg.dev request names the credential it attached):"
kubectl -n "$NS" logs "deploy/$RELEASE" --tail=20
echo "==> a real private wheel installed from a sandbox holding no Google credential"
