#!/usr/bin/env bash
# Stage 70 — the model route: the sandbox sends an unsigned model call to a plain
# base URL and the proxy attaches a credential the sandbox never held. That is the
# claim ADR 0005 makes about model access, and this is it in a cluster: the fixture
# holds no credential, no sentinel, and not even a CA.
#
# Credentials: none, and the upstream is a mock. Not a compromise — the *credential*
# here is Workload Identity, so a kind or k3s cluster cannot produce one at all and
# there is deliberately nowhere to mount one (the same rule as GAR's, see
# charts/proxy/values.yaml). What a mock cannot prove is that Google accepts the
# URL shape; what it does prove is everything between the sandbox and Google — the
# chart's environment, the base URL, the rewrite, the attach, the location pin, and
# SSE surviving the hop. Real Vertex is measured in ADR 0005 and pinned offline by
# proxy/vertex_test.go; a credentialed run belongs to whichever ticket gives CI a
# Workload Identity pool.
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"

stage "build the image with ko"
IMG=$(make -s -C "$E2E_DIR/.." image)
stage "load $IMG"
load_image "$IMG"

stage "stand up the Vertex mock"
kubectl apply -n "$NS" -f "$E2E_DIR/vertex-mock.yaml" >/dev/null
kubectl -n "$NS" rollout status deploy/vertex-mock --timeout=180s

stage "install the chart with the model route on"
# vertex.upstream is the seam: it points the route at the mock *and* replaces the
# token source with a worthless stub, which is why this needs no Google identity
# and why the seam cannot be a way to send a real credential somewhere else.
# The run key Secret is stage 30's; the GitHub credentials default off, and the
# model route needs none of them.
#
# No --reuse-values, like every stage here: an upgrade resets whatever it does not
# --set back to the chart default, so this one puts the GitHub credentials stages
# 50 and 60 configured back to off. Harmless only because 70 runs last — a stage
# after it that expects them still wired must set them again, or say --reuse-values.
helm upgrade --install "$RELEASE" "$E2E_DIR/../charts/proxy" -n "$NS" \
  --set-string image="$IMG" \
  --set vertex.enabled=true \
  --set-string vertex.upstream=http://vertex-mock:8080 --wait --timeout=180s >/dev/null
kubectl -n "$NS" rollout status "deploy/$RELEASE" --timeout=180s

stage "probe the model route (runtimeClassName=${RUNTIME_CLASS:-<none>})"
# The base URL the orchestrator will hand a sandbox, port and prefix included.
# `http://` on purpose: the request carries no credential in either direction.
run_job e2e-vertex "$E2E_DIR/vertex-job.yaml" \
  "BASE_URL=http://$RELEASE:8080/vertex"

echo
echo "==> proxy log (each model call names the run it was identified as):"
kubectl -n "$NS" logs "deploy/$RELEASE" --tail=20
echo "==> the model route proven: unsigned in, credentialed out, streaming intact"

# Last, and only on success — `set -e` means a failed stage leaves the mock and its
# log where they can be read. Nothing after this needs it: the proxy dials the
# upstream per request, never at boot, so the release is left healthy without it.
kubectl -n "$NS" delete -f "$E2E_DIR/vertex-mock.yaml" --ignore-not-found >/dev/null
