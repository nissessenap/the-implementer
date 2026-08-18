#!/usr/bin/env bash
# Stage 30 — the credential proxy itself: TLS interception for the hosts on its
# certificate, an opaque tunnel for everything else. Credentials: none, on purpose.
# The credentials this proxy exists to attach arrive in #52, #54 and #55.
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"
IMG=implementer-proxy:e2e
RELEASE=credential-proxy

stage "build and load $IMG"
docker build -q -t "$IMG" "$E2E_DIR/.." >/dev/null
load_image "$IMG"

stage "install the chart"
# The leaf Secret from stage 10. Its SANs are the intercept list, so this is the
# only place the intercepted hosts are named — there is no host list in values.yaml
# to drift away from the certificate.
helm upgrade --install "$RELEASE" "$E2E_DIR/../charts/proxy" -n "$NS" \
  --set image.tag="${IMG#*:}" --wait --timeout=180s >/dev/null
# The tag never changes, so a rebuilt image needs the pod replaced explicitly.
kubectl -n "$NS" rollout restart "deploy/$RELEASE" >/dev/null
kubectl -n "$NS" rollout status "deploy/$RELEASE" --timeout=180s

stage "probe through the proxy (runtimeClassName=${RUNTIME_CLASS:-<none>})"
run_job e2e-proxy "$E2E_DIR/proxy-job.yaml"

echo
echo "==> proxy log (the egress inventory #16 starts from):"
kubectl -n "$NS" logs "deploy/$RELEASE" --tail=20
echo "==> interception and tunnelling proven"
