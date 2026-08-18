#!/usr/bin/env bash
# Stage 10 — cert-manager, the private CA, and the leaf carrying the intercept list.
# Credentials: none.
#
# selfSigned Issuer -> CA Certificate -> CA Issuer -> leaf, because a self-signed
# *leaf* is not a CA and nothing chains to it. cert-manager rather than openssl in a
# shell script: a production cluster has it anyway, so the shape is the point.
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"
CM_VERSION=${CM_VERSION:-v1.21.1}

if ! kubectl get crd certificates.cert-manager.io >/dev/null 2>&1; then
  stage "install cert-manager $CM_VERSION"
  kubectl apply -f "https://github.com/cert-manager/cert-manager/releases/download/${CM_VERSION}/cert-manager.yaml" >/dev/null
fi

# Outside the install branch on purpose: a run interrupted partway through the
# rollout leaves the CRDs present and cert-manager not serving.
for d in cert-manager cert-manager-webhook cert-manager-cainjector; do
  kubectl -n cert-manager rollout status "deploy/$d" --timeout=300s
done

stage "issue the CA and the leaf into $NS"
kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# The SAN list is the *only* place the intercepted hosts are configured: the proxy
# (#50) reads them back off its own certificate, so the two cannot drift apart.
# `*.pkg.dev` and not `*-go.pkg.dev`, because crypto/x509 only matches a wildcard
# occupying the whole leftmost label — so the certificate is deliberately wider than
# the credential rule, and #52's credFor is what keeps -docker.pkg.dev tokenless.
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
# validating webhook is callable — cainjector still has to write its caBundle. Each
# attempt prints its own error, so a real failure is loud without a replay.
for i in $(seq 30); do
  printf '%s\n' "$CA_MANIFEST" | kubectl apply -n "$NS" -f - && break
  [[ $i -lt 30 ]] || exit 1
  sleep 5
done

kubectl -n "$NS" wait --for=condition=Ready certificate/proxy-ca --timeout=180s
kubectl -n "$NS" wait --for=condition=Ready certificate/proxy-leaf --timeout=180s

# The sandbox needs the CA certificate and must never see either key, so it gets a
# ConfigMap holding only ca.crt. A Secret mount with `items` would also hide the
# key, but leaves the CA private key one careless edit away from the sandbox.
# ponytail: cert-manager's trust-manager does this distribution in production. One
# more install to answer a question this `kubectl get` answers.
kubectl -n "$NS" create configmap proxy-ca \
  --from-literal=ca.crt="$(kubectl -n "$NS" get secret proxy-ca-tls -o jsonpath='{.data.ca\.crt}' | base64 -d)" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

echo "==> leaf SANs (== the proxy's intercept list):"
kubectl -n "$NS" get secret proxy-leaf-tls -o jsonpath='{.data.tls\.crt}' | base64 -d \
  | openssl x509 -noout -ext subjectAltName
