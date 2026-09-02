#!/usr/bin/env bash
# Stage 85 — ADR 0003's three language images, each building and testing a real
# repository of its language inside a real sandbox, through the credential proxy.
#
# Credentials: none, and it never skips. The repositories are public, so the clone
# goes through the proxy anonymously exactly as stage 80's does; what is under test
# here is the *toolchain*, which needs no credential at all.
#
# It is the half of the acceptance criteria `sandbox/contract.sh` cannot reach: the
# contract check proves an image is shaped right, and only this proves it can
# actually resolve dependencies and run a test suite from inside the sandbox.
#
# Four runs. Three are the language images doing their language's work. The fourth
# is the `go` image with the rootlesskit wrap on — the one thing that needs gVisor,
# a real uid 1000 and the builder's own securityContext patch, and which nothing
# offline can prove.
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"

ORCH=${ORCH:-implementer-orchestrator}
PREFIX=${E2E_SANDBOX_IMAGE_PREFIX:-ghcr.io/nissessenap/implementer}
TAG=${E2E_SANDBOX_TAG:-dev}
BASE=${E2E_SANDBOX_BASE:-ghcr.io/nissessenap/implementer-base:$TAG}
ISSUE=${E2E_ISSUE:-74}

# The repositories, and they are the stage's one piece of drift risk — small, old
# and boring on purpose. Overridable, because an air-gapped operator has their own
# and an upstream that reorganises its tests is not this stage's bug to carry.
GO_REPO=${E2E_GO_REPO:-google/go-cmp}
NODE_REPO=${E2E_NODE_REPO:-sindresorhus/is-plain-obj}
PYTHON_REPO=${E2E_PYTHON_REPO:-benjaminp/six}

PROBE=$(mktemp)
trap 'rm -f "$PROBE"' EXIT

# The posture assertion every probe carries, and the reason it is in all four: the
# wrap is per-run and off by default, so "still uid 1000" is a claim about *this*
# run and not about the image. `bwrap` is reported rather than asserted — it is the
# base image's contract, measured by #22 and #71, and a language layer that broke
# it would show up here as a difference between images rather than as a failure
# this stage invented.
posture() {
  cat <<'EOP'
# The one thing every probe must do before anything else, because this script
# stands in for the run plan and the run plan is what builds it: the builder points
# all seven trust variables at $SSL_CERT_FILE, a path no image can ship — the CA is
# minted at install time and /etc/ssl/certs is read-only. Missing, the first HTTPS
# call fails at CA load and looks like a certificate problem.
cat /etc/ssl/certs/ca-certificates.crt /run/proxy-ca/ca.crt > "$SSL_CERT_FILE"

echo "PROBE uid              $(id -u) (rootlesskit wrap ${SANDBOX_DOCKER:-off})"
[ "$(id -u)" = 1000 ] || { echo "!!! FAIL: the run is not uid 1000" >&2; exit 1; }
bwrap --ro-bind / / true 2>/dev/null && echo "PROBE bwrap            ok" || echo "PROBE bwrap            unavailable (base-image contract, see #22)"
EOP
}

# clone <repo> — through the proxy, and **anonymously**, for the same reason
# stage 80's clone half is: this stage installs no GitHub credential, so nothing
# swaps the sentinel and `x-access-token:<sentinel>` reaches github.com as a real
# credential and is refused with a 401. The repositories are public and what is
# under test here is the toolchain, so the credential would only be a second thing
# to get wrong.
clone() {
  cat <<EOP
set -eu
git clone --quiet --depth 1 "https://github.com/$1.git" /workspace/repo
cd /workspace/repo
echo "PROBE clone            $1 through the proxy"
EOP
}

# run_toolchain <name> <image> <owner/repo#n> [orchestrator flags…]
run_toolchain() {
  local name=$1 image=$2 ref=$3
  stage "$name: $image"
  helm upgrade --install "$ORCH" "$E2E_DIR/../charts/orchestrator" -n "$NS" \
    --set-string sandbox.image="$image" \
    --set-string sandbox.runtimeClassName="${RUNTIME_CLASS:-}" \
    --set-file sandbox.script="$PROBE" \
    --set-string proxyCAConfigMapName=proxy-ca \
    --set-string sandbox.resources.requests.cpu=200m \
    --set-string sandbox.resources.requests.memory=512Mi \
    --set-string sandbox.resources.limits.memory=3Gi \
    --wait --timeout=60s >/dev/null

  local tmpl job
  tmpl=$(mktemp)
  kubectl -n "$NS" get cm "$ORCH-job-template" -o jsonpath='{.data.job\.yaml}' > "$tmpl"
  kubectl -n "$NS" delete job -l app=implementer --cascade=foreground --wait >/dev/null

  # The run's identity is the repository the probe clones, so the proxy log and
  # the Job annotations name the same thing a reader is looking at. Each run gets
  # its own reference, and the previous Job is deleted above, so nothing is
  # swallowed as a redelivery.
  job=$(RUN_KEY_FILE=$(run_key_file) POD_NAMESPACE="$NS" PROXY_HOST="$RELEASE" \
    JOB_TEMPLATE_FILE="$tmpl" go run "$E2E_DIR/../cmd/orchestrator" run "${@:4}" \
    "$ref" | tee /dev/stderr | awk '{print $2}' | cut -d/ -f2-)
  rm -f "$tmpl"
  wait_job "$job"
}

stage "build and load the images"
docker build -q -t "$BASE" "$E2E_DIR/../sandbox" >/dev/null
load_image "$BASE"
for t in go node python; do
  docker build -q --build-arg BASE="$BASE" -t "$PREFIX-$t:$TAG" "$E2E_DIR/../sandbox/$t" >/dev/null
  "$E2E_DIR/../sandbox/contract.sh" "$PREFIX-$t:$TAG" "$t"
  load_image "$PREFIX-$t:$TAG"
done

# --------------------------------------------------------------------- go ---
{
  posture
  clone "$GO_REPO"
  cat <<'EOP'
# Two statements and not `build && test`: a non-last command of an AND-OR list is
# exempt from errexit, so `build && test` lets a failed build fall through to the
# success line below and the whole stage goes green on an image that cannot build.
go build ./...
go test ./... >/dev/null
echo "PROBE go               built and tested $(go env GOVERSION)"

# A toolchain this image does not carry. Routine rather than exotic — Go's own
# auto-download makes it so — and the acceptance criterion is that it *completes*,
# not that we prevent it. GOTOOLCHAIN=local would turn this into a failed run that
# looks like the repository's fault.
#
# Named on the command line rather than pinned in go.mod, and that is the whole
# trick: Go never *downgrades* for a go.mod, so a pin below the image's version
# downloads nothing and proves nothing, while a pin above it needs a released
# version newer than the one the image ships — which does not exist whenever the
# image is current. An explicit GOTOOLCHAIN fetches the named version either way.
mkdir -p /workspace/pinned && cd /workspace/pinned
printf 'module pinned\n\ngo 1.26.0\n' > go.mod
printf 'package main\nfunc main() {}\n' > main.go
GOTOOLCHAIN=go1.26.0 go build ./...
echo "PROBE toolchain-pin    go1.26.0, which this $(go env GOVERSION) image lacks, downloaded and used"
EOP
} > "$PROBE"
run_toolchain go "$PREFIX-go:$TAG" "$GO_REPO#$ISSUE" -toolchain go

# ------------------------------------------------------------------- node ---
{
  posture
  clone "$NODE_REPO"
  cat <<'EOP'
npm install --no-audit --no-fund --loglevel=error
npm test
echo "PROBE node             installed and tested with node $(node --version), npm $(npm --version)"
EOP
} > "$PROBE"
run_toolchain node "$PREFIX-node:$TAG" "$NODE_REPO#$ISSUE" -toolchain node

# ----------------------------------------------------------------- python ---
{
  posture
  clone "$PYTHON_REPO"
  cat <<'EOP'
# No venv, deliberately: this filesystem is read-only and the container is
# ephemeral, so the image drops PEP 668's externally-managed marker rather than
# making every run build a virtualenv it throws away. --user because /usr is
# read-only at run time; $HOME is the emptyDir the pod can write, and the image
# puts ~/.local/bin on PATH so a console script a repository installs is callable.
pip install --user --quiet --disable-pip-version-check pytest
python3 -m pytest -q >/dev/null
echo "PROBE python           installed and tested with $(python3 --version)"
EOP
} > "$PROBE"
run_toolchain python "$PREFIX-python:$TAG" "$PYTHON_REPO#$ISSUE" -toolchain python

# ------------------------------------------------- the go image's Docker ----
# The one thing only a cluster proves. `orchestrator run -docker` is what puts
# SANDBOX_DOCKER in the environment *and* relaxes the two securityContext fields
# that a file-capability newuidmap needs — measured, and without them the only
# symptom is `newuidmap: write to uid_map failed: Operation not permitted`.
#
# The rootlesskit invocation below is the run plan's, spelled out because this
# probe replaces the run plan. sandbox/phase_test.go is what pins the script's own
# copy; what is under test here is the *image* and the *posture*.
{
  posture
  cat <<'EOP'
set -eu
[ "${SANDBOX_DOCKER:-}" = 1 ] || { echo "!!! FAIL: -docker did not reach the sandbox" >&2; exit 1; }
exec rootlesskit --net=slirp4netns --mtu=1500 --disable-host-loopback \
  --copy-up=/etc --copy-up=/run /bin/sh -c '
  echo "PROBE inside           uid=$(id -u) map=$(tr -s " " < /proc/self/uid_map | tr "\n" "|")"
  export XDG_RUNTIME_DIR=/tmp/docker DOCKER_HOST=unix:///tmp/docker/docker.sock
  mkdir -p "$XDG_RUNTIME_DIR" "$HOME/.local/share/docker"
  dockerd --iptables=false --ip6tables=false --bridge=none \
    --feature containerd-snapshotter=false \
    --data-root "$HOME/.local/share/docker" --host "$DOCKER_HOST" >/tmp/dockerd.log 2>&1 &
  i=0
  until docker version >/dev/null 2>&1; do
    i=$((i + 1))
    [ "$i" -lt 90 ] || { echo "!!! FAIL: dockerd never became usable" >&2; tail -5 /tmp/dockerd.log >&2; exit 1; }
    sleep 1
  done
  echo "PROBE dockerd          $(docker version --format "{{.Server.Version}}") ready on the version gate"
  # Reported, not asserted, and the one number here worth reading. The image
  # carries no fuse-overlayfs — it is a FUSE filesystem and the PodSpec mounts no
  # /dev/fuse, so dockerd could never have selected it — which leaves `overlay2`
  # or `vfs`. overlay2 is what the #28 spike measured in this posture and is what
  # to expect. `vfs` means the data root's own filesystem is overlayfs and overlay
  # will not stack on itself; it is slow and disk-hungry in a 3Gi pod, and the fix
  # is a memory-backed emptyDir for $HOME, not a package.
  echo "PROBE storage-driver   $(docker info --format "{{.Driver}}")"
  docker run --rm --network=host busybox echo "PROBE inner-container   ok"
'
EOP
} > "$PROBE"
run_toolchain "go + docker" "$PREFIX-go:$TAG" "$GO_REPO#$((ISSUE + 1))" -toolchain go -docker

echo
echo "==> three language images build and test their own language through the proxy,"
echo "==> and the go image runs Docker as uid 1000, unprivileged, under gVisor"
