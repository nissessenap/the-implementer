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
if [[ ${E2E_ALLOW_REMOTE:-0} == 1 ]]; then
  # Unlocking is not enough, because the stages below create a namespace, a CA, a
  # Deployment and Jobs in whatever context was last switched to — and on a
  # developer laptop that is a real cluster. So the bypass has to *name* what it
  # is unlocking, and stage 70 is the only reason it exists.
  : "${E2E_EXPECT_CLUSTER:?E2E_ALLOW_REMOTE=1 also needs E2E_EXPECT_CLUSTER=<substring of the intended API server URL>}"
  if [[ $SERVER != *"$E2E_EXPECT_CLUSTER"* ]]; then
    echo "REFUSING: API server '$SERVER' does not contain E2E_EXPECT_CLUSTER='$E2E_EXPECT_CLUSTER'." >&2
    exit 1
  fi
else
  case $SERVER in
    https://127.0.0.1:* | https://localhost:* | 'https://[::1]:'*) ;;
    *)
      echo "REFUSING: API server '$SERVER' is not local." >&2
      echo "  kind: make kind-up. k3s: export KUBECONFIG." >&2
      echo "  Deliberate: E2E_ALLOW_REMOTE=1 plus E2E_EXPECT_CLUSTER=<substring of that server URL>." >&2
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

# run_cred <owner> <repo> <issue> <run-uid> — the userinfo the orchestrator puts in
# the sandbox's https_proxy URL. Recomputed by the proxy from the pod's own
# annotations, so the two agreeing is the whole authentication.
run_cred() {
  local user="$1,$2,$3,$4"
  printf '%s:%s' "$user" \
    "$(printf '%s' "$user" | openssl dgst -sha256 -hmac "$RUN_KEY" -r | cut -d' ' -f1)"
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
    # Checked up front, as proto/go.sh did: a RuntimeClass the cluster does not
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
  local job=$1 manifest=$2 rcl phase= kv
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
