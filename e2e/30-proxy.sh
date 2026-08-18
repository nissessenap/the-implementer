#!/usr/bin/env bash
# Stage 30 — the credential proxy itself: TLS interception for the hosts on its
# certificate, an opaque tunnel for everything else, both as an authenticated run.
# Credentials: none, on purpose — the run secret authenticates the caller and is
# worth nothing outside this cluster.
# The credentials this proxy exists to attach arrive in #52, #54 and #55.
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"

stage "build the image with ko"
# `make image` prints the reference and nothing else. Its tag is a hash of the
# binary, so an unchanged proxy reinstalls as a no-op and a changed one rolls.
IMG=$(make -s -C "$E2E_DIR/.." image)
stage "load $IMG"
load_image "$IMG"

stage "create the shared run key"
# One Secret, mounted by the proxy and (later) the orchestrator. There is no
# per-run Secret: both ends derive the run's credential from this key.
kubectl -n "$NS" create secret generic proxy-run-key \
  --from-literal=key="$RUN_KEY" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

stage "install the chart"
# The leaf Secret from stage 10. Its SANs are the intercept list, so this is the
# only place the intercepted hosts are named — there is no host list in values.yaml
# to drift away from the certificate.
helm upgrade --install "$RELEASE" "$E2E_DIR/../charts/proxy" -n "$NS" \
  --set-string image="$IMG" --wait --timeout=180s >/dev/null
kubectl -n "$NS" rollout status "deploy/$RELEASE" --timeout=180s

stage "probe through the proxy (runtimeClassName=${RUNTIME_CLASS:-<none>})"
# The credential must be derived from the same four values as the fixture's
# annotations, or the proxy refuses every request in it.
run_job e2e-proxy "$E2E_DIR/proxy-job.yaml" "RUN_CRED=$(run_cred acme widgets 5 stage30)"

echo
echo "==> proxy log (the egress inventory #16 starts from):"
kubectl -n "$NS" logs "deploy/$RELEASE" --tail=20
echo "==> interception and tunnelling proven"
