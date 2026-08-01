#!/usr/bin/env bash
# PROTOTYPE — throwaway. Does https_proxy catch `go` (#33), and does the proxy put
# a GAR credential on traffic the sandbox has no credential for (#34)?
#   ./proto/proxy/up.sh && ./proto/proxy/probe.sh
#
# A one-off pod on the upstream golang image rather than a toolchain in the agent
# image: the question is about `go`, not about our Dockerfile, and 450MB of apt in
# the sandbox image would slow every build/import cycle to answer it once.
#
# The pod gets NO egress credential and NO DNS tricks — just https_proxy and the
# proxy's CA. Whether the traffic really went through the proxy is answered by the
# proxy's own log, printed at the end, not by these commands succeeding.
#
# The #34 half is a *differential*: the same GAR URL, from the same pod, direct and
# proxied. Direct is 401, proxied is 404 — 404 being "your identity is fine, that
# repo does not exist". A 401->404 flip cannot happen unless a credential the pod
# never held reached Artifact Registry.
set -euo pipefail

DIR=$(cd "$(dirname "$0")" && pwd)
NS=implementer-proto
POD=proxy-probe-go
PROXY=http://proto-proxy.$NS.svc.cluster.local:8080
# shellcheck source=../kubeconf.sh
source "$DIR/../kubeconf.sh"

# The GAR half needs a project id, and that id lives only in the gitignored env
# file — same rule as #33's model pins.
# shellcheck source=/dev/null
[[ -f $DIR/../.vertex.env ]] && { set -a; source "$DIR/../.vertex.env"; set +a; }
GAR_PROJECT=${GAR_PROJECT:-${VERTEX_PROJECT:-}}
GAR_REGION=${GAR_REGION:-europe-west1}
GAR_REPO=${GAR_REPO:-nosuchrepo} # see README: the project has no Go repo at all
GAR_MODULE=${GAR_MODULE:-github.com/x/y}
[[ -n $GAR_PROJECT ]] || { echo "set GAR_PROJECT (or VERTEX_PROJECT in proto/.vertex.env)" >&2; exit 1; }
GAR_URL=https://$GAR_REGION-go.pkg.dev/$GAR_PROJECT/$GAR_REPO

# github.com and *.pkg.dev are both intercepted now, so the probe has to trust the
# proxy's CA or every handshake is refused before any of this means anything.
CA=$(kubectl -n "$NS" get cm proto-ca -o jsonpath='{.data.ca\.crt}')

kubectl -n "$NS" delete pod "$POD" --ignore-not-found --wait >/dev/null

kubectl -n "$NS" run "$POD" --image=golang:1.25 --restart=Never --attach --rm \
  --env=https_proxy="$PROXY" --env=HTTPS_PROXY="$PROXY" \
  --env=CA_PEM="$CA" --env=GAR_URL="$GAR_URL" --env=GAR_MODULE="$GAR_MODULE" \
  --env=GOFLAGS=-mod=mod --env=GOPATH=/tmp/go --env=GOCACHE=/tmp/gocache \
  --env=GOSUMDB=off \
  --command -- sh -c '
    # The three variables that *replace* the trust store, so the bundle is the
    # system one concatenated with ours — see the #34 findings. Assembled before
    # `set -x`, or the trace prints the whole PEM.
    cat /etc/ssl/certs/ca-certificates.crt > /tmp/bundle.crt
    printf "%s\n" "$CA_PEM" >> /tmp/bundle.crt
    export SSL_CERT_FILE=/tmp/bundle.crt CURL_CA_BUNDLE=/tmp/bundle.crt GIT_SSL_CAINFO=/tmp/bundle.crt
    set -x

    # Not /tmp itself: Go 1.25 ignores a go.mod in the system temp root.
    mkdir -p /tmp/probe && cd /tmp/probe && go mod init probe >/dev/null 2>&1
    go get golang.org/x/oauth2@v0.36.0     # GOPROXY over https_proxy
    go install golang.org/x/tools/cmd/goimports@latest
    git ls-remote https://github.com/git/git HEAD | head -1

    printf "\n== issue #34: same URL, no credential in this pod. direct vs proxied ==\n"
    env -u https_proxy -u HTTPS_PROXY \
      curl -s -o /dev/null -w "PROBE gar-direct   %{http_code} (401 = anonymous)\n" "$GAR_URL/$GAR_MODULE/@v/list"
    curl -s -o /dev/null -w "PROBE gar-proxied  %{http_code} (404 = authenticated, no such repo)\n" "$GAR_URL/$GAR_MODULE/@v/list"

    printf "\n== and through the go command itself, which is what a real build runs ==\n"
    GOPROXY=$GAR_URL go list -m -versions "$GAR_MODULE" || true

    printf "\n== unset the proxy and the same fetch must still be attempted directly ==\n"
    env -u https_proxy -u HTTPS_PROXY go list -m -versions golang.org/x/text | head -1
  ' || echo "!!! probe pod exited non-zero (see output above)"

echo
echo "==================== PROXY LOG (the actual evidence) ===================="
kubectl -n "$NS" logs deploy/proto-proxy --tail=80 | grep -E 'CONNECT|MITM|->' || true
