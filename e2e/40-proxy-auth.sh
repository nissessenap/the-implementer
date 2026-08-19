#!/usr/bin/env bash
# Stage 40 — the proxy authenticates the *caller*. Credentials: none real. The run
# secret under test is derived from a key this harness invents, and authenticates
# a run rather than a person, so nothing here is worth stealing.
#
# Stage 30 already proved the happy path from an authenticated pod. This stage is
# the refusals: no credential, a wrong one, and — the interesting one — a valid
# credential for a run this pod is not.
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"

stage "probe caller authentication (runtimeClassName=${RUNTIME_CLASS:-<none>})"
# WRONG_SECRET keeps the claim and breaks the digest — the proxy recomputes it, so
# only the shared key could have produced the real one. OTHER_RUN is the opposite:
# a digest that verifies perfectly, for four values this pod's annotations do not
# carry.
run_job e2e-proxy-auth "$E2E_DIR/auth-job.yaml" \
  "GOOD=$(run_cred acme widgets 5 stage40)" \
  "WRONG_SECRET=acme,widgets,5,stage40:$(printf 0%.0s $(seq 64))" \
  "OTHER_RUN=$(run_cred acme widgets 9 stage40-other)"

echo
echo "==> proxy log (every refusal names the run it could not tie the caller to):"
kubectl -n "$NS" logs "deploy/$RELEASE" --tail=20
echo "==> caller authentication proven"
