#!/usr/bin/env bash
# Stage 10 — cert-manager, the private CA, and the leaf carrying the intercept list.
# Credentials: none. Runs on every PR, forks included.
#
# cert-manager rather than openssl-in-a-shell-script because a production cluster
# has it anyway, so the question worth answering is "does the cert-manager shape
# work". Two Issuers, because a self-signed *leaf* is not a CA and nothing chains
# to it: selfSigned Issuer -> CA Certificate -> CA Issuer -> leaf.
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"
CM_VERSION=${CM_VERSION:-v1.21.1}

if ! kubectl get crd certificates.cert-manager.io >/dev/null 2>&1; then
  stage "install cert-manager $CM_VERSION"
  kubectl apply -f "https://github.com/cert-manager/cert-manager/releases/download/${CM_VERSION}/cert-manager.yaml" >/dev/null
fi

# Outside the install branch on purpose: a run interrupted partway through the
# rollout leaves the CRDs present and cert-manager not serving, and skipping the
# wait then fails at the first apply with a webhook connection error.
for d in cert-manager cert-manager-webhook cert-manager-cainjector; do
  kubectl -n cert-manager rollout status "deploy/$d" --timeout=300s
done

stage "issue the CA and the leaf into $NS"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# The SAN list is the *only* place the intercepted hosts are configured: the proxy
# (#50) reads them back off its own certificate, so the two cannot drift apart.
# github.com          git clone/push, and the OAuth-ish endpoints
# api.github.com      gh
# codeload / objects / raw   tarballs, LFS, release assets — cheap to include now,
#                     expensive to discover missing halfway through a run.
# *.pkg.dev           Artifact Registry. A wildcard, because the Go endpoint is
#                     {region}-go.pkg.dev and pinning a region here would
#                     reintroduce the region config #33 got rid of. `*-go.pkg.dev`
#                     is tighter and does not work: crypto/x509 (and every other
#                     TLS stack worth naming) only matches a wildcard occupying
#                     the whole leftmost label. So the certificate is deliberately
#                     *wider* than the credential rule, and #52's credFor is what
#                     keeps -docker.pkg.dev from being handed a bearer token.
# ponytail: a heredoc rather than a committed manifest. It becomes chart templates
# in #50; a second home for the SAN list before then is a second thing to drift.
CA_MANIFEST=$(cat <<'YAML'
apiVersion: cert-manager.io/v1
kind: Issuer
metadata: { name: proxy-selfsigned }
spec: { selfSigned: {} }
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: { name: proxy-ca }
spec:
  isCA: true
  commonName: the-implementer e2e CA
  secretName: proxy-ca-tls
  duration: 2160h
  privateKey: { algorithm: ECDSA, size: 256 }
  issuerRef: { name: proxy-selfsigned, kind: Issuer, group: cert-manager.io }
---
apiVersion: cert-manager.io/v1
kind: Issuer
metadata: { name: proxy-ca }
spec:
  ca: { secretName: proxy-ca-tls }
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: { name: proxy-leaf }
spec:
  secretName: proxy-leaf-tls
  duration: 720h
  privateKey: { algorithm: ECDSA, size: 256 }
  issuerRef: { name: proxy-ca, kind: Issuer, group: cert-manager.io }
  dnsNames:
    - github.com
    - api.github.com
    - codeload.github.com
    - objects.githubusercontent.com
    - raw.githubusercontent.com
    - "*.pkg.dev"
YAML
)

# Retried rather than waited on: an Available Deployment does not mean the
# validating webhook is callable — cainjector still has to write its caBundle and
# the Service still needs an endpoint. Retrying is what cert-manager's own docs
# advise for exactly this cold-cluster window.
for _ in $(seq 30); do
  if printf '%s\n' "$CA_MANIFEST" | kubectl apply -n "$NS" -f - >/dev/null 2>&1; then
    APPLIED=1
    break
  fi
  sleep 5
done
if [[ -z ${APPLIED:-} ]]; then
  echo "!!! the cert-manager webhook never admitted the apply. Once more, loudly:" >&2
  printf '%s\n' "$CA_MANIFEST" | kubectl apply -n "$NS" -f -
  exit 1
fi

kubectl -n "$NS" wait --for=condition=Ready certificate/proxy-ca --timeout=180s
kubectl -n "$NS" wait --for=condition=Ready certificate/proxy-leaf --timeout=180s

# The sandbox needs the CA certificate and must never see either key, so it gets a
# ConfigMap holding only ca.crt rather than a mount of a TLS Secret.
# ponytail: cert-manager's trust-manager does exactly this distribution in
# production. One more install to answer a question this `kubectl get` answers.
kubectl -n "$NS" create configmap proxy-ca \
  --from-literal=ca.crt="$(kubectl -n "$NS" get secret proxy-ca-tls -o jsonpath='{.data.ca\.crt}' | base64 -d)" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

echo "==> CA:"
kubectl -n "$NS" get secret proxy-ca-tls -o jsonpath='{.data.ca\.crt}' | base64 -d \
  | openssl x509 -noout -subject -dates
echo "==> leaf SANs (== the proxy's intercept list):"
kubectl -n "$NS" get secret proxy-leaf-tls -o jsonpath='{.data.tls\.crt}' | base64 -d \
  | openssl x509 -noout -ext subjectAltName
