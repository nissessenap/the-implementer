#!/usr/bin/env bash
# Stage 60 — the proxy mints its own installation token, signed by an external key,
# and mints it **for the repository the run's annotations name**. This is the stage
# that replaces stage 50's static token: the fixture still holds only the sentinel,
# but what it gets back is now scoped to one repository rather than to whatever the
# operator put in a Secret.
#
# The assertion is `GET /installation/repositories`, which asks GitHub what the
# token it was presented with can actually reach. A token minted for the URL rather
# than the annotation — the one failure this ticket exists to prevent — answers with
# the whole installation, and the probe fails.
#
# It signs through **OpenBao transit**, not a PEM on disk: the App key is imported
# into the same in-cluster OpenBao stage 55 uses, so the signature crosses the
# network here exactly as it does from KMS in production — and the key itself never
# enters the cluster at all. Stage 55 proves that signature in isolation and needs
# no credential; this stage is the half only GitHub can answer, which is whether
# GitHub accepts it.
#
# Credentials: an App, and so the operator's, not the harness's. Skips without them.
#
#   E2E_GITHUB_APP_ID=123456        the App's numeric id
#   E2E_GITHUB_APP_KEY=/path/app.pem  its private key, as GitHub downloads it
#   E2E_GITHUB_REPO=me/scratch      a repository the App is installed on
#   E2E_GITHUB_OTHER_REPO=me/other  optional: a *second* private repository in the
#                                   same installation, whose clone must fail
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"

if [[ -z ${E2E_GITHUB_APP_ID:-} || -z ${E2E_GITHUB_APP_KEY:-} || -z ${E2E_GITHUB_REPO:-} ]]; then
  stage "SKIPPED — set E2E_GITHUB_APP_ID, E2E_GITHUB_APP_KEY and E2E_GITHUB_REPO to mint against real GitHub"
  echo "  (proxy/mint_test.go pins the scope, the cache and the refresh offline)" >&2
  exit 0
fi
[[ -r $E2E_GITHUB_APP_KEY ]] || { echo "E2E_GITHUB_APP_KEY=$E2E_GITHUB_APP_KEY is not readable" >&2; exit 1; }

OWNER=${E2E_GITHUB_REPO%%/*}
REPO=${E2E_GITHUB_REPO#*/}

stage "build the image with the vault provider linked in"
# ghait.vault links the transit signer and ghait.no_file drops the PEM-on-disk one,
# which is production's posture: this image cannot sign from a file even if a file
# were mounted. Production swaps the first tag for ghait.gcp and nothing else.
IMG=$(make -s -C "$E2E_DIR/.." image GO_TAGS=ghait.vault,ghait.no_file)
stage "load $IMG"
load_image "$IMG"

stage "stand up OpenBao and import the App key into transit"
# The App key is read here, on this machine, and what reaches the cluster is
# wrapped key material inside a transit key. There is no Secret holding the PEM —
# `provider: gcp` in production has no such Secret either, and this is the same
# shape rather than an e2e shortcut that happens to look like it.
openbao_up
# BAO_ADDR and BAO_TOKEN are lib.sh's and unexported, so they are handed over
# explicitly: the script is a separate process and takes exactly these.
BAO_ADDR=$BAO_ADDR BAO_TOKEN=$BAO_TOKEN TRANSIT_REIMPORT=1 \
  "$E2E_DIR/transit-import.sh" github-app "$E2E_GITHUB_APP_KEY"

stage "upgrade the proxy to mint as App $E2E_GITHUB_APP_ID, signing through transit"
# githubTokenSecretName is cleared explicitly: stage 50 set it, and the chart
# refuses to render with both rather than pick one silently.
helm upgrade --install "$RELEASE" "$E2E_DIR/../charts/proxy" -n "$NS" \
  --set-string image="$IMG" \
  --set-string githubTokenSecretName= \
  --set-string githubApp.appId="$E2E_GITHUB_APP_ID" \
  --set-string githubApp.provider=vault \
  --set-string githubApp.key=transit/github-app \
  --set-string githubApp.vault.addr="$BAO_SVC" \
  --set-string githubApp.vault.tokenSecretName=openbao-token --wait --timeout=180s >/dev/null
kubectl -n "$NS" rollout status "deploy/$RELEASE" --timeout=180s

stage "probe the mint (runtimeClassName=${RUNTIME_CLASS:-<none>})"
# The annotations are the mint scope. Everything the fixture asks for is checked
# against them, and nothing it can say in a request URL changes them.
run_job e2e-mint "$E2E_DIR/mint-job.yaml" \
  "RUN_CRED=$(run_cred "$OWNER" "$REPO" 5 stage60)" \
  "SENTINEL=$SENTINEL" \
  "OWNER=$OWNER" \
  "REPO=$REPO" \
  "OTHER_REPO=${E2E_GITHUB_OTHER_REPO:-}"

echo
echo "==> proxy log (each mint names the run and the installation it was scoped to):"
kubectl -n "$NS" logs "deploy/$RELEASE" --tail=20
echo "==> the minted, per-repository installation token proven against real GitHub —"
echo "==> and every JWT behind it signed over the network by OpenBao transit"
