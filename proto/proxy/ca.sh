#!/usr/bin/env bash
# PROTOTYPE — throwaway. The private CA the sandbox is made to trust (issue #34).
#   ./proto/proxy/ca.sh
# Called automatically by up.sh; standalone for iterating.
#
# cert-manager rather than openssl-in-a-shell-script on purpose: #34 assumes it is
# present in a production cluster anyway, so the question worth answering is
# "does the cert-manager shape work", not "can bash mint a certificate".
#
# Two Issuers, because a self-signed *leaf* is not a CA and nothing chains to it:
#   selfsigned Issuer -> CA Certificate (proto-ca-tls) -> CA Issuer -> leaf.
set -euo pipefail

DIR=$(cd "$(dirname "$0")" && pwd)
NS=implementer-proto
CM_VERSION=${CM_VERSION:-v1.21.1}
# shellcheck source=../kubeconf.sh
source "$DIR/../kubeconf.sh"

if ! kubectl get crd certificates.cert-manager.io >/dev/null 2>&1; then
  echo "==> install cert-manager $CM_VERSION"
  kubectl apply -f "https://github.com/cert-manager/cert-manager/releases/download/${CM_VERSION}/cert-manager.yaml" >/dev/null
  for d in cert-manager cert-manager-webhook cert-manager-cainjector; do
    kubectl -n cert-manager rollout status "deploy/$d" --timeout=180s
  done
fi

kubectl create namespace "$NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# The SAN list is the *only* place the intercepted hosts are configured: the proxy
# reads them back off its own certificate, so the two cannot drift apart.
# github.com          git clone/push, and the OAuth-ish endpoints
# api.github.com      gh
# codeload / objects / raw   tarballs, LFS, release assets — cheap to include now,
#                     expensive to discover missing halfway through a run.
# *.pkg.dev           Artifact Registry (issue #34). A wildcard, because the Go
#                     endpoint is {region}-go.pkg.dev and pinning a region here
#                     would reintroduce exactly the region config #33 got rid of.
#                     `*-go.pkg.dev` would be tighter and does not work: crypto/x509
#                     (and every other TLS stack worth naming) only matches a
#                     wildcard that is the *whole* leftmost label. So the cert is
#                     wider than the credential rule, and main.go's credFor() is
#                     what keeps -docker.pkg.dev from being handed a bearer token.
kubectl apply -f - >/dev/null <<YAML
apiVersion: cert-manager.io/v1
kind: Issuer
metadata: { name: proto-selfsigned, namespace: $NS }
spec: { selfSigned: {} }
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: { name: proto-ca, namespace: $NS }
spec:
  isCA: true
  commonName: the-implementer proto CA
  secretName: proto-ca-tls
  duration: 2160h
  privateKey: { algorithm: ECDSA, size: 256 }
  issuerRef: { name: proto-selfsigned, kind: Issuer, group: cert-manager.io }
---
apiVersion: cert-manager.io/v1
kind: Issuer
metadata: { name: proto-ca, namespace: $NS }
spec:
  ca: { secretName: proto-ca-tls }
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata: { name: proto-github, namespace: $NS } # ponytail: no longer only github
spec:
  secretName: proto-github-tls
  duration: 720h
  privateKey: { algorithm: ECDSA, size: 256 }
  issuerRef: { name: proto-ca, kind: Issuer, group: cert-manager.io }
  dnsNames:
    - github.com
    - api.github.com
    - codeload.github.com
    - objects.githubusercontent.com
    - raw.githubusercontent.com
    - "*.pkg.dev"
YAML

kubectl -n "$NS" wait --for=condition=Ready certificate/proto-ca --timeout=120s
kubectl -n "$NS" wait --for=condition=Ready certificate/proto-github --timeout=120s

# The sandbox needs the CA cert and must never see the key, so it gets a ConfigMap
# holding only ca.crt rather than a mount of either TLS Secret.
# ponytail: cert-manager's trust-manager does exactly this distribution in
# production. One more install to answer a question this `kubectl get` answers.
kubectl -n "$NS" create configmap proto-ca \
  --from-literal=ca.crt="$(kubectl -n "$NS" get secret proto-ca-tls -o jsonpath='{.data.ca\.crt}' | base64 -d)" \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

echo "==> CA ready; sandbox trusts:"
kubectl -n "$NS" get secret proto-ca-tls -o jsonpath='{.data.ca\.crt}' | base64 -d \
  | openssl x509 -noout -subject -dates 2>/dev/null || true
echo "==> leaf SANs (== the proxy's intercept list):"
kubectl -n "$NS" get secret proto-github-tls -o jsonpath='{.data.tls\.crt}' | base64 -d \
  | openssl x509 -noout -ext subjectAltName 2>/dev/null || true
