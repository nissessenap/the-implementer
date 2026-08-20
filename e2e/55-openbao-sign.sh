#!/usr/bin/env bash
# Stage 55 — the App JWT's signature arrives **over the network**. That is the one
# link the `file` provider leaves untested and the one production has: from KMS the
# signature is an RPC, and everything about it that can be wrong — the key material
# transit holds, the digest it signs, the marshaling of what comes back — is wrong
# in a way a local PEM never is.
#
# Credentials: none, and none possible. OpenBao runs in dev mode in the cluster and
# the key is generated here and thrown away, so this stage runs on every pull
# request — which is the point of choosing OpenBao over a cloud KMS.
#
# Two assertions, and they are different halves:
#
#   1. the signature verifies against the key that was imported. Signed through
#      transit with the exact parameters ghait's provider sends, so what is proved
#      is ghait's request and not a request this stage invented — the BYOK import,
#      the PKCS#1-v1.5-over-SHA-256 digest, and the JWS marshaling that makes the
#      answer a JWT signature segment rather than raw base64.
#   2. the proxy boots on it, from inside the cluster. `provider: vault` linked in
#      by build tag, VAULT_ADDR/VAULT_TOKEN from the chart, and ghait's boot-time
#      Check() reading the key back over that connection — a pod that goes ready
#      has done all four.
#
# What it cannot prove is the mint, because that needs GitHub — and so it never
# calls ghait's Sign() at all: assertion 1 is this stage's own request and
# assertion 2 is ghait's Check(), which only reads. Stage 60 calls it, with the
# operator's App key through this same OpenBao; `proxy/mint_vault_test.go` calls it
# with no credential at all, against a transit and a GitHub that are both fakes.
# Between the three there is no link left untested, and only this one needs a
# cluster.
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"

KEY=e2e-selftest
# A private key lives in here, so it is removed on every exit. Armed twice, because
# bash has one EXIT trap and openbao_up installs its own over this one: until it
# returns the directory is still empty, and from there on this trap does both.
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

stage "stand up OpenBao in dev mode"
openbao_up
trap 'rm -rf "$tmp"; bao_down' EXIT

stage "import a throwaway RSA-2048 key into transit"
# Throwaway, and generated here: this half of the stage is about transit, so the
# key it holds may as well be one nobody has to have. Stage 60 imports the App's.
openssl genrsa -out "$tmp/key.pem" 2048 2>/dev/null
openssl rsa -in "$tmp/key.pem" -pubout -out "$tmp/pub.pem" 2>/dev/null
# BAO_ADDR and BAO_TOKEN are lib.sh's and unexported, so they are handed over
# explicitly: the script is a separate process and takes exactly these.
BAO_ADDR=$BAO_ADDR BAO_TOKEN=$BAO_TOKEN TRANSIT_REIMPORT=1 \
  "$E2E_DIR/transit-import.sh" "$KEY" "$tmp/key.pem"

stage "sign through transit and verify against the imported key"
# The three parameters are ghait's, copied from provider/vault: sha2-256, pkcs1v15,
# jws. Sending anything else would prove a signing path the proxy does not use.
printf '%s' '{"iss":"e2e","exp":0}' >"$tmp/msg"
sig=$(curl -sS -f --max-time 30 -H "X-Vault-Token: $BAO_TOKEN" -X POST \
  -d "{\"input\":\"$(base64 -w0 <"$tmp/msg")\",\"hash_algorithm\":\"sha2-256\",\"signature_algorithm\":\"pkcs1v15\",\"marshaling_algorithm\":\"jws\"}" \
  "$BAO_ADDR/v1/transit/sign/$KEY" | sed -n 's/.*"signature":"\([^"]*\)".*/\1/p')
[[ -n $sig ]] || { echo "!!! FAIL: transit returned no signature" >&2; exit 1; }

# The prefix is transit's key version, and ghait strips exactly "vault:v1:" —
# a hardcoded 1, so a *rotated* key returns "vault:v2:…" and ghait leaves it
# inside the JWT's signature segment. Asserted here rather than tolerated: in dev
# mode nothing rotates, and the day this stage fails on it is the day the pin in
# go.mod needs the fix that is not upstream yet.
[[ $sig == vault:v1:* ]] || {
  echo "!!! FAIL: signature is prefixed '$(printf '%s' "$sig" | cut -d: -f1-2):' — ghait only strips 'vault:v1:'" >&2
  exit 1
}
raw=${sig#vault:v1:}
# base64url, unpadded, because that is what "jws" means — which is why ghait can
# hand it straight to golang-jwt as the third segment.
case $(( ${#raw} % 4 )) in 2) raw="$raw==" ;; 3) raw="$raw=" ;; esac
printf '%s' "$raw" | tr '_-' '/+' | base64 -d >"$tmp/sig.bin"
openssl dgst -sha256 -verify "$tmp/pub.pem" -signature "$tmp/sig.bin" "$tmp/msg" >/dev/null || {
  echo "!!! FAIL: the transit signature does not verify against the imported key" >&2
  exit 1
}
echo "PROBE transit-sign     ok   (over the network, verified against the key that was imported)"

stage "build the image with the vault provider linked in"
# ghait.vault links the transit signer; ghait.no_file drops the PEM-on-disk one,
# as production does — so this image cannot sign from a file even by accident, and
# the App key never enters the cluster in stage 60 at all.
IMG=$(make -s -C "$E2E_DIR/.." image GO_TAGS=ghait.vault,ghait.no_file)
stage "load $IMG"
load_image "$IMG"

stage "boot the proxy against transit"
# App 1 is a number, not an App: nothing here mints, and GITHUB_APP_ID is only what
# makes the proxy resolve a signer at all. What is under test is the four steps
# between "provider: vault" and a ready pod — the tag, the address, the token, and
# ghait's Check() reading rsa-2048/supports_signing back off the key.
helm upgrade --install "$RELEASE" "$E2E_DIR/../charts/proxy" -n "$NS" \
  --set-string image="$IMG" \
  --set-string githubTokenSecretName= \
  --set-string githubApp.appId=1 \
  --set-string githubApp.provider=vault \
  --set-string githubApp.key="transit/$KEY" \
  --set-string githubApp.vault.addr="$BAO_SVC" \
  --set-string githubApp.vault.tokenSecretName=openbao-token --wait --timeout=180s >/dev/null
kubectl -n "$NS" rollout status "deploy/$RELEASE" --timeout=180s

# The rollout above is the assertion — Check() runs before the listener exists, so
# an unlinked provider, a wrong address or a key transit will not sign with is a
# pod that never goes ready. The log below only names what that proved.
#
# By image, and not `logs deploy/…`: for a few seconds after a rollout the previous
# pod still matches the Deployment's selector, and `logs deploy/…` picks whichever
# it lists first — which read stage 40's tokenless proxy and failed this stage in a
# full run while passing on its own.
pod=$(kubectl -n "$NS" get pod -l "app=$RELEASE" \
  -o jsonpath="{range .items[*]}{.metadata.name}{'\t'}{.spec.containers[0].image}{'\n'}{end}" \
  | awk -F'\t' -v img="$IMG" '$2 == img { print $1; exit }')
[[ -n $pod ]] || { echo "!!! FAIL: no proxy pod running $IMG" >&2; exit 1; }
kubectl -n "$NS" logs "$pod" | grep -q 'signed by vault' || {
  echo "!!! FAIL: the proxy is ready but its log does not say it signs through vault" >&2
  kubectl -n "$NS" logs "$pod" >&2
  exit 1
}
echo "PROBE vault-provider   ok   (linked by build tag, reached at $BAO_SVC, key checked at boot)"
echo
echo "==> the networked signer proven: imported, signed, verified, and booted on"
