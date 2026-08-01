#!/usr/bin/env bash
# PROTOTYPE — throwaway. Does https_proxy catch `go`? (issue #33)
#   ./proto/proxy/up.sh && ./proto/proxy/probe.sh
#
# A one-off pod on the upstream golang image rather than a toolchain in the agent
# image: the question is about `go`, not about our Dockerfile, and 450MB of apt in
# the sandbox image would slow every build/import cycle to answer it once.
#
# The pod gets NO egress credential and NO DNS tricks — just https_proxy. Whether
# the traffic really went through the proxy is answered by the proxy's CONNECT log,
# printed at the end, not by these commands succeeding.
set -euo pipefail

DIR=$(cd "$(dirname "$0")" && pwd)
NS=implementer-proto
POD=proxy-probe-go
PROXY=http://proto-proxy.$NS.svc.cluster.local:8080
# shellcheck source=../kubeconf.sh
source "$DIR/../kubeconf.sh"

kubectl -n "$NS" delete pod "$POD" --ignore-not-found --wait >/dev/null

kubectl -n "$NS" run "$POD" --image=golang:1.25 --restart=Never --attach --rm \
  --env=https_proxy="$PROXY" --env=HTTPS_PROXY="$PROXY" \
  --env=GOFLAGS=-mod=mod --env=GOPATH=/tmp/go --env=GOCACHE=/tmp/gocache \
  --command -- sh -c '
    set -x
    cd /tmp && go mod init probe >/dev/null 2>&1
    go get golang.org/x/oauth2@v0.36.0     # GOPROXY over https_proxy
    go install golang.org/x/tools/cmd/goimports@latest
    git ls-remote https://github.com/git/git HEAD | head -1
    printf "\n== unset the proxy and the same fetch must still be attempted directly ==\n"
    env -u https_proxy -u HTTPS_PROXY go list -m -versions golang.org/x/text | head -1
  ' || echo "!!! probe pod exited non-zero (see output above)"

echo
echo "==================== PROXY LOG (the actual evidence) ===================="
kubectl -n "$NS" logs deploy/proto-proxy --tail=60 | grep -E 'CONNECT|->' || true
