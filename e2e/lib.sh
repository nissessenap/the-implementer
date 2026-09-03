# shellcheck shell=bash
# Sourced by every e2e stage, not executed.

NS=${NS:-implementer-e2e}
# The proxy's Helm release, and so its Deployment and Service names. Here rather
# than in a stage, because two stages install against it and read its log.
RELEASE=${RELEASE:-credential-proxy}
E2E_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

# `make kind-up` writes here; a local k3s just exports KUBECONFIG.
if [[ -z ${KUBECONFIG:-} && -r "$E2E_DIR/../.kind.kubeconfig" ]]; then
  export KUBECONFIG="$E2E_DIR/../.kind.kubeconfig"
fi

SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
if [[ ${E2E_ALLOW_REMOTE:-0} != 1 ]]; then
  case $SERVER in
    https://127.0.0.1:* | https://localhost:* | 'https://[::1]:'*) ;;
    *)
      echo "REFUSING: API server '$SERVER' is not local." >&2
      echo "  kind: make kind-up. k3s: export KUBECONFIG. Deliberate: E2E_ALLOW_REMOTE=1." >&2
      exit 1
      ;;
  esac
fi

stage() { printf '\n=== %s: %s\n' "$(basename "$0")" "$*" >&2; }

# The one long-lived key the run secret derives from. A literal here because the
# e2e is both ends at once — it plays the orchestrator, which mints the credential,
# and installs the proxy, which recomputes it. In a cluster this is a Secret both
# mount and neither of them ships.
RUN_KEY=${RUN_KEY:-e2e-shared-run-key-not-a-real-one}

# run_key_file — RUN_KEY as a file, because that is how both binaries read it:
# RUN_KEY_FILE, in the proxy and in the orchestrator alike. One mechanism, and a
# key in a process environment is a key in every child's.
#
# ponytail: a fixed path rather than mktemp, so a full run reuses one file instead
# of leaking one per stage — lib.sh has exactly one EXIT trap and it is already
# spoken for. A real cleanup stack here is what a third litter-producing helper
# wants, and openbao_up says the same thing.
RUN_KEY_PATH=${RUN_KEY_PATH:-${TMPDIR:-/tmp}/implementer-e2e-run-key}
run_key_file() {
  ( umask 077; printf '%s' "$RUN_KEY" > "$RUN_KEY_PATH" )
  echo "$RUN_KEY_PATH"
}

# run_cred <owner> <repo> <issue> <run-uid> — the userinfo the orchestrator puts in
# the sandbox's https_proxy URL. Recomputed by the proxy from the pod's own
# annotations, so the two agreeing is the whole authentication.
#
# Derived by the orchestrator's own binary and not by an `openssl dgst -hmac`
# here. That is the point of #70 building the Job builder first: the derivation is
# proxy.Cred, exported for exactly this consumer, and a second implementation in
# bash is a second thing that can drift — silently, because a wrong HMAC and a
# wrong key look identical in a 407.
run_cred() {
  RUN_KEY_FILE=$(run_key_file) go run "$E2E_DIR/../cmd/orchestrator" cred "$1" "$2" "$3" "$4"
}

# The worthless string the sandbox holds where the GitHub credential would be, and
# the one the proxy matches on. Padded to 40 bytes, the length of every GitHub
# token, so the swap never changes a request's length. proxy.Sentinel is the same
# constant on the Go side; TestSentinelIsTokenLength pins the length there and the
# check below pins it here, which is all that ties the two copies together.
SENTINEL='proxy-injected--------------------------'
[[ ${#SENTINEL} -eq 40 ]] || { echo "SENTINEL is ${#SENTINEL} bytes, not 40" >&2; exit 1; }

# Empty in CI, `gvisor` against a local k3s. Substituted away entirely rather than
# left empty: runtimeClassName is a *string, and "" is not a DNS subdomain.
runtime_class_line() {
  if [[ -n ${RUNTIME_CLASS:-} ]]; then
    # Checked up front, as the prototype did: a RuntimeClass the cluster does not
    # have leaves the pod unschedulable and the poll below spends its whole
    # timeout on it.
    kubectl get runtimeclass "$RUNTIME_CLASS" >/dev/null
    echo "runtimeClassName: $RUNTIME_CLASS"
  else
    echo "# no runtimeClassName (RUNTIME_CLASS unset)"
  fi
}

# run_job <name> <manifest> [KEY=value…] — apply a Job fixture, wait for its single
# pod, print its logs, and fail the stage unless it succeeded. Each KEY=value
# replaces __KEY__ in the manifest, on top of __RUNTIME_CLASS__.
run_job() {
  local job=$1 manifest=$2 rcl kv
  shift 2
  rcl=$(runtime_class_line)
  local -a subst=(-e "s|__RUNTIME_CLASS__|$rcl|")
  for kv in "$@"; do subst+=(-e "s|__${kv%%=*}__|${kv#*=}|g"); done

  # --cascade=foreground, because the default background cascade returns as soon
  # as the Job object is gone and leaves the previous run's pod being collected.
  # The poll below selects on job-name, which matches that pod too — a stale
  # Succeeded one would report the stage green without the fixture having run.
  kubectl -n "$NS" delete job "$job" --ignore-not-found --cascade=foreground --wait >/dev/null
  sed "${subst[@]}" "$manifest" | kubectl apply -n "$NS" -f - >/dev/null
  wait_job "$job"
}

# wait_job <name> — wait for a Job's single pod, print its logs, and fail unless it
# succeeded. Split out of run_job for stage 80, whose Job is created by the
# orchestrator's own binary rather than applied from a fixture.
wait_job() {
  local job=$1 phase=

  # Polled rather than `kubectl wait --for=condition=Complete`, which blocks for
  # the full timeout when the Job fails — the case whose logs we want soonest.
  # Multiple --for flags are ANDed, so there is no one-shot Complete-or-Failed
  # wait. backoffLimit is 0, so the selector matches exactly one pod.
  for _ in $(seq 100); do
    phase=$(kubectl -n "$NS" get pod -l "job-name=$job" -o jsonpath='{.items[0].status.phase}' 2>/dev/null || true)
    case $phase in Succeeded | Failed) break ;; esac
    sleep 3
  done

  echo
  kubectl -n "$NS" logs "job/$job" --all-containers=true || true
  [[ $phase == Succeeded ]] || {
    echo "!!! FAIL: $job ended in '${phase:-<no pod>}'" >&2
    kubectl -n "$NS" describe "job/$job" >&2
    return 1
  }
}

# tee_stderr — `tee` a pipeline to stderr without *reopening* it. Plain
# `tee /dev/stderr` opens the path afresh with O_TRUNC, so when a caller redirects
# the run to a file (`make e2e >log 2>&1`) every stage before this one is wiped and
# the failure arrives with no context. The process substitution inherits fd 2.
tee_stderr() { tee >(cat >&2); }

# load_image <image> — put a locally built image where the cluster can see it.
# The one cluster-flavour variable the harness has: kind by default, and a local
# k3s exports E2E_IMAGE_LOAD='<command taking the image name>', e.g.
#   E2E_IMAGE_LOAD='sh -c "docker save \"\$0\" | sudo k3s ctr images import -"'
load_image() {
  if [[ -n ${E2E_IMAGE_LOAD:-} ]]; then
    eval "$E2E_IMAGE_LOAD \"\$1\""
  else
    kind load docker-image --name implementer-e2e "$1"
  fi
}

# The GitHub mock stages point GITHUB_API_URL at, and the reason it is here rather
# than in the stage that first needed it: issue #72 asks for the mock to be
# infrastructure a later ticket adds *assertions* to, not one it re-stands-up.
#
# Where the *harness* reaches it, which is a different address from the Service the
# in-cluster side would use: the orchestrator is a command line until the webhook
# front-end lands, so it runs on this machine and reaches the mock through a
# forward. The port is the knob and the URL is derived, so the two cannot disagree.
GITHUB_MOCK_PORT=${GITHUB_MOCK_PORT:-18080}
GITHUB_MOCK_ADDR=http://127.0.0.1:$GITHUB_MOCK_PORT

# github_mock_up — apply the Deployment, wait for it, and leave a port-forward behind
# on $GITHUB_MOCK_ADDR. Idempotent, so a stage run on its own stands it up and a
# later one finds it there — which is why it also does not reset the mock's state:
# that is `POST /_reset`, and it is the calling stage's business.
#
# ponytail: it does *not* install an EXIT trap, unlike openbao_up. bash has exactly
# one, and a stage that needs this also has litter of its own — so the caller owns
# the trap and calls github_mock_down from it. A third caller of either wants a real
# cleanup stack instead of this note.
github_mock_up() {
  kubectl apply -n "$NS" -f "$E2E_DIR/github-mock.yaml" >/dev/null
  kubectl -n "$NS" rollout status deploy/github-mock --timeout=180s
  kubectl -n "$NS" port-forward deploy/github-mock "$GITHUB_MOCK_PORT:8080" >/dev/null 2>&1 &
  GITHUB_MOCK_PF=$!
  for _ in $(seq 30); do
    # The forward is a *background* job, so `set -e` cannot see it fail and its
    # output is discarded: a port already in use leaves this loop polling whatever
    # else answers there. Checked before the health probe rather than trusted after.
    kill -0 "$GITHUB_MOCK_PF" 2>/dev/null || {
      echo "!!! FAIL: the port-forward to github-mock died — is $GITHUB_MOCK_PORT in use?" >&2
      return 1
    }
    curl -sf --max-time 2 "$GITHUB_MOCK_ADDR/_calls" >/dev/null && return 0
    sleep 1
  done
  echo "!!! FAIL: no github-mock on $GITHUB_MOCK_ADDR (is the port already in use?)" >&2
  return 1
}

# github_mock_down — kill the port-forward *and reap it*. `kill` returns when the
# signal is sent, not when the socket is released, so without the wait a stage that
# rebinds the same port seconds later fails occasionally and never reproduces.
github_mock_down() {
  [[ -n ${GITHUB_MOCK_PF:-} ]] || return 0
  kill "$GITHUB_MOCK_PF" 2>/dev/null || true
  # `|| true` on both: a reaped child killed by the SIGTERM above exits 143, and this
  # runs from an EXIT trap where a non-zero last command is the *stage's* status.
  wait "$GITHUB_MOCK_PF" 2>/dev/null || true
}

# OpenBao, the networked signer stages 55 and 60 sign the App JWT through. A dev
# root token the harness invents, exactly like RUN_KEY above: it is a literal
# because this OpenBao is the harness's own, in-memory, and gone with the pod.
#
# Not overridable, deliberately: BAO_TOKEN is the variable the `bao` CLI itself
# reads, so a developer with an OpenBao of their own has one exported — and
# honouring it here would write their real root token into openbao.yaml's pod spec
# and into a Secret in the cluster.
BAO_TOKEN=e2e-openbao-dev-root-not-a-real-one
# Where *the proxy* reaches it — a Service name, so nothing about the cluster's
# flavour comes into it.
# shellcheck disable=SC2034  # read by the stages, not by this file
BAO_SVC=http://openbao:8200
# Where *the harness* reaches it, which is a different address: the import needs
# the PEM, the PEM is on this machine, so that half goes through a port-forward.
# The port is the knob and the URL is built from it, so the two cannot disagree —
# the port-forward needs the number and the import needs the URL.
BAO_PORT=${BAO_PORT:-8200}
BAO_ADDR=http://127.0.0.1:$BAO_PORT

# openbao_up — apply the Deployment, wait for it, put the token where the proxy
# can read it, and leave a port-forward behind on $BAO_ADDR. Idempotent, so a
# stage run on its own stands it up and a full run's second stage finds it there.
openbao_up() {
  sed -e "s|__TOKEN__|$BAO_TOKEN|" "$E2E_DIR/openbao.yaml" | kubectl apply -n "$NS" -f - >/dev/null
  kubectl -n "$NS" rollout status deploy/openbao --timeout=180s
  kubectl -n "$NS" create secret generic openbao-token \
    --from-literal=token="$BAO_TOKEN" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

  # Backgrounded, and killed on the way out — a leaked port-forward makes the
  # *next* run's health check pass against a dead tunnel.
  #
  # ponytail: bash has exactly one EXIT trap, and the whole of the machinery for
  # sharing it is that $BAO_PF is a global — a stage with litter of its own writes
  # its own trap and has to remember to call bao_down in it. Stage 55 does, three
  # lines away from this one. A third caller, or a stage that forgets and leaks the
  # forward, wants a real cleanup stack here.
  kubectl -n "$NS" port-forward deploy/openbao "$BAO_PORT:8200" >/dev/null 2>&1 &
  BAO_PF=$!
  trap bao_down EXIT
  for _ in $(seq 30); do
    # The forward is a *background* job, so `set -e` cannot see it fail and its
    # output is discarded: a port already in use leaves this loop polling whatever
    # else answers there — and 8200 is the port a real OpenBao is on, whose
    # /v1/sys/health needs no token to say 200. That is the one way this stage
    # reports success having never reached its own OpenBao, so it is checked
    # before the health probe rather than trusted after it.
    kill -0 "$BAO_PF" 2>/dev/null || {
      echo "!!! FAIL: the port-forward to openbao died — is $BAO_PORT already in use?" >&2
      return 1
    }
    curl -sf --max-time 2 "$BAO_ADDR/v1/sys/health" >/dev/null && return 0
    sleep 1
  done
  echo "!!! FAIL: no OpenBao on $BAO_ADDR (is the port already in use?)" >&2
  return 1
}

# bao_down — kill the port-forward *and reap it*. `kill` returns when the signal is
# sent, not when the socket is released, and in a full run stage 60 binds the same
# port seconds after stage 55's trap fires: without the wait that is an occasional
# bind failure which never reproduces in a single stage.
bao_down() {
  [[ -n ${BAO_PF:-} ]] || return 0
  kill "$BAO_PF" 2>/dev/null || true
  # `|| true` on both, because a reaped child that died of the SIGTERM above exits
  # 143 and this runs from an EXIT trap: a non-zero last command there is the
  # *stage's* exit status, and the stage passed.
  wait "$BAO_PF" 2>/dev/null || true
}
