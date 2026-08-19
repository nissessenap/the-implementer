#!/usr/bin/env bash
# Stage 50 — the sentinel swap for GitHub, on a static token. The fixture pod holds
# the worthless string and nothing else; the proxy substitutes a real token inside a
# TLS session the pod believes is GitHub's.
#
# Credentials: this is the one stage that wants a real one. It needs a scratch
# repository it may push a dry run against, and a token that can, so it skips rather
# than fails when they are absent — the token is the operator's, not the harness's.
#
#   E2E_GITHUB_TOKEN=ghp_…  a PAT (or installation token) with contents:write
#   E2E_GITHUB_REPO=me/scratch  a repository it may push-dry-run to
#
# What makes the assertion airtight is the rate limit: GitHub gives 60/h anonymous
# and 5000/h authenticated, and the pod holds nothing that could reach 5000.
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"

if [[ -z ${E2E_GITHUB_TOKEN:-} || -z ${E2E_GITHUB_REPO:-} ]]; then
  stage "SKIPPED — set E2E_GITHUB_TOKEN and E2E_GITHUB_REPO to run the swap against real GitHub"
  echo "  (proxy/creds_test.go covers both credential shapes and the 401 round-trip offline)" >&2
  exit 0
fi

stage "create the GitHub token Secret"
# The only place the real token exists in this stage. The fixture never sees it:
# it is mounted into the proxy, and the pod is handed the sentinel.
kubectl -n "$NS" create secret generic proxy-github-token \
  --from-literal=token="$E2E_GITHUB_TOKEN" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

stage "upgrade the proxy to hold it"
# Stage 30 installed the same release with no credential — deliberately, because
# that is the tokenless path. This adds the one value that binds a credential to
# the GitHub hosts on the certificate.
IMG=$(kubectl -n "$NS" get "deploy/$RELEASE" -o jsonpath='{.spec.template.spec.containers[0].image}')
helm upgrade --install "$RELEASE" "$E2E_DIR/../charts/proxy" -n "$NS" \
  --set-string image="$IMG" \
  --set-string githubTokenSecretName=proxy-github-token --wait --timeout=180s >/dev/null
kubectl -n "$NS" rollout status "deploy/$RELEASE" --timeout=180s

stage "probe the swap (runtimeClassName=${RUNTIME_CLASS:-<none>})"
run_job e2e-github "$E2E_DIR/github-job.yaml" \
  "RUN_CRED=$(run_cred acme widgets 5 stage50)" \
  "SENTINEL=$SENTINEL" \
  "REPO=$E2E_GITHUB_REPO"

echo
echo "==> proxy log (each swap names the credential it attached):"
kubectl -n "$NS" logs "deploy/$RELEASE" --tail=20
echo "==> the sentinel swap proven against real GitHub"
# Left credentialed on purpose: this is the last stage, and a re-run of stage 30
# reinstalls the release with no credential before this one adds it back. A stage
# 60 that must see a tokenless proxy has to say so itself.
