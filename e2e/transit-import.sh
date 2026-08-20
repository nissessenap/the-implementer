#!/usr/bin/env bash
# transit-import.sh <key-name> <pem-path> — put a private key that already exists
# into an OpenBao/Vault transit key.
#
# Why by hand. Transit must **import** rather than generate: GitHub generates the
# App's key pair and offers no bring-your-own-public-key, so the key arrives as a
# PEM and the only question is how to get it in. Vault ships `vault transit
# import` for exactly this; OpenBao ships no equivalent — there is no `bao transit
# import` — so this is that helper, spelled out against the HTTP API:
#
#   1. read transit's own RSA-4096 wrapping key
#   2. generate an ephemeral AES-256 key
#   3. AES-KWP-wrap the PKCS#8 DER private key with it
#   4. RSA-OAEP-SHA256-wrap the AES key with the wrapping key
#   5. concatenate, in that order, and base64
#
# Not e2e-only, deliberately: it takes a path and a name and knows nothing about
# the harness, because an operator running the self-hosted story does this same
# step once per App key.
#
#   BAO_ADDR=http://127.0.0.1:8200 BAO_TOKEN=… ./transit-import.sh github-app app.pem
#
# It refuses to touch a key that already exists. TRANSIT_REIMPORT=1 replaces one —
# which the e2e sets, because its stages are re-runnable, and which an operator
# should not: transit will not import over a key, so replacing means *deleting* it,
# and a mistyped name would delete the wrong one.
set -euo pipefail

NAME=${1:?usage: transit-import.sh <key-name> <pem-path>}
PEM=${2:?usage: transit-import.sh <key-name> <pem-path>}
: "${BAO_ADDR:?BAO_ADDR is required}" "${BAO_TOKEN:?BAO_TOKEN is required}"

api() { curl -sS --max-time 30 -f -H "X-Vault-Token: $BAO_TOKEN" "$@"; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# The mount may already be there, and that is not a failure — anything else here
# fails on the wrapping key read below.
api -X POST -d '{"type":"transit"}' "$BAO_ADDR/v1/sys/mounts/transit" >/dev/null 2>&1 || true

# The key may already be there, and that *is* a decision. Transit refuses to import
# over an existing key, so the only way through is to delete it — destructive, and
# it needs deletion_allowed, which import deliberately does not set. So it is asked
# for explicitly rather than done because a re-run is convenient.
if api "$BAO_ADDR/v1/transit/keys/$NAME" >/dev/null 2>&1; then
  [[ ${TRANSIT_REIMPORT:-} == 1 ]] || {
    echo "transit/keys/$NAME already exists; set TRANSIT_REIMPORT=1 to delete and replace it" >&2
    exit 1
  }
  api -X POST -d '{"deletion_allowed":true}' "$BAO_ADDR/v1/transit/keys/$NAME/config" >/dev/null
  api -X DELETE "$BAO_ADDR/v1/transit/keys/$NAME" >/dev/null
  echo "deleted the existing transit/keys/$NAME (TRANSIT_REIMPORT=1)" >&2
fi

# Read per import rather than cached: transit generates it on first use, and it is
# per mount.
api "$BAO_ADDR/v1/transit/wrapping_key" \
  | sed -n 's/.*"public_key":"\([^"]*\)".*/\1/p' | sed 's/\\n/\n/g' >"$tmp/wrapping.pem"
grep -q 'BEGIN PUBLIC KEY' "$tmp/wrapping.pem" || {
  echo "no wrapping key from $BAO_ADDR — is transit mounted and the token valid?" >&2
  exit 1
}

# PKCS#8 DER, which is what transit imports. GitHub hands out PKCS#1 PEM, and this
# is the conversion ADR 0005 warns eats an afternoon when it is discovered late.
openssl pkcs8 -topk8 -nocrypt -in "$PEM" -outform DER -out "$tmp/key.der"

# A65959A6 is RFC 5649's alternative initial value. `openssl enc` wants the wrap
# cipher's IV spelled out and has no default for it; this is the only value a
# KWP unwrap on the other side will accept.
openssl rand -out "$tmp/aes.bin" 32
# Both forms are needed — `enc` wants hex on the command line, `pkeyutl` wants the
# bytes in a file — and `od` rather than `xxd`, which is not on every machine.
#
# ponytail: `openssl enc` has no way to take a raw key off anything but argv, so for
# the length of that one call the ephemeral wrapping key is in `ps` for every user
# on this machine. It wraps the App key rather than being it, and it is discarded
# here — but on a shared or CI-shared host that is a real window, and closing it
# means doing RFC 5649 in Go or Python instead of in openssl.
aes=$(od -An -tx1 -v <"$tmp/aes.bin" | tr -d ' \n')
openssl enc -id-aes256-wrap-pad -K "$aes" -iv A65959A6 -in "$tmp/key.der" -out "$tmp/wrapped-key"
openssl pkeyutl -encrypt -pubin -inkey "$tmp/wrapping.pem" \
  -pkeyopt rsa_padding_mode:oaep -pkeyopt rsa_oaep_md:sha256 -pkeyopt rsa_mgf1_md:sha256 \
  -in "$tmp/aes.bin" -out "$tmp/wrapped-aes"

# Order is the protocol: the RSA-OAEP block first, then the AES-KWP one. Base64,
# and so nothing in it needs escaping into the JSON below.
ct=$(cat "$tmp/wrapped-aes" "$tmp/wrapped-key" | base64 -w0)

# rsa-2048 rather than transit's default, because it is what GitHub generates —
# and it is what ghait's boot-time Check() insists on, by name.
api -X POST -d "{\"ciphertext\":\"$ct\",\"type\":\"rsa-2048\",\"hash_function\":\"SHA256\"}" \
  "$BAO_ADDR/v1/transit/keys/$NAME/import" >/dev/null
echo "imported $PEM into transit/keys/$NAME (rsa-2048)" >&2
