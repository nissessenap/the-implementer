#!/usr/bin/env bash
# Stage 80 — the orchestrator's Job builder, against a real cluster. The binary
# creates the Job; the chart renders its PodSpec; the pod reaches the proxy with a
# credential the builder derived and holds nothing of its own.
#
# Credentials: none for the clone half, which is why this stage never skips. The
# push half needs a repository the proxy's credential can write to and runs only
# when there is one — E2E_GITHUB_REPO, plus a proxy that stage 50 or 60 left
# credentialed. That is read off the Deployment rather than guessed from the
# environment, so this stage is honest when run on its own.
#
# What the apiserver answers here and nothing else can: whether the derived Job
# name is accepted. The cap is 63 and it comes from `spec.template`'s labels, not
# `metadata.name`, so the long name below is *created* and then deleted rather than
# reasoned about. The `_` versus `-` pair is a server dry-run instead: what is under
# test there is that the two names differ, which needs no object.
#
# The push is `--dry-run`, as in stages 50 and 60 and for their reason: it still
# does the ref discovery and the authentication, which is the whole of what is under
# test, and a real push would leave a branch on the operator's repository every run.
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"

ORCH=${ORCH:-implementer-orchestrator}
# The public repository the sandbox clones anonymously. The clone proves the run
# reached the proxy and trusted its CA; whose repository it is does not matter, so
# it is this one.
CLONE_REPO=${E2E_CLONE_REPO:-nissessenap/the-implementer}
ISSUE=${E2E_ISSUE:-70}

TMPL=$(mktemp)
PROBE=$(mktemp)
trap 'rm -f "$TMPL" "$PROBE"' EXIT

# The run this stage creates. Its owner/repo is the push target when there is one,
# because a *minted* credential is scoped to the annotations and not to the URL —
# so pushing to anything else would fail for a reason that has nothing to do with
# the builder.
RUN_REPO=${E2E_GITHUB_REPO:-$CLONE_REPO}

# The probe, plus the write half when there is a repository to push to *and* a
# proxy holding a credential to push with. That second half is read off the
# Deployment, because either stage 50 (static token) or stage 60 (minted as an App)
# may have put one there and this stage must not install one itself — the two are
# not interchangeable and the chart refuses to render both.
#
# Two files concatenated rather than one substituted: sed replaces one line with
# one line, and quoting a shell script through its replacement syntax is how a
# probe stops being readable.
sed "s|__CLONE_REPO__|$CLONE_REPO|g" "$E2E_DIR/orchestrator-probe.sh" > "$PROBE"
#
# A here-string and not a pipe: `grep -q` exits on the first match, kubectl then
# dies of SIGPIPE, and `set -o pipefail` promotes that 141 to the pipeline's status
# — so a *matching* probe takes the else branch and the push half reads as a
# legitimate skip. Today's Deployment fits the pipe buffer and it does not fire,
# which is the worst version of a bug like this.
if [[ -n ${E2E_GITHUB_REPO:-} ]] &&
  grep -qE 'GITHUB_TOKEN_FILE|GITHUB_APP_ID' <<<"$(kubectl -n "$NS" get "deploy/$RELEASE" -o yaml 2>/dev/null)"; then
  # Named, because the bare `cat:` this replaces aborted the stage mid-script and
  # said nothing about which half was missing.
  [[ -f $E2E_DIR/orchestrator-push-probe.sh ]] ||
    { echo "!!! FAIL: e2e/orchestrator-push-probe.sh is missing — the push half cannot run" >&2; exit 1; }
  cat "$E2E_DIR/orchestrator-push-probe.sh" >> "$PROBE"
else
  echo 'echo "PROBE git-push-dry-run skipped (no credentialed proxy and E2E_GITHUB_REPO — run stage 50 or 60)"' >> "$PROBE"
fi

stage "install charts/orchestrator"
# The chart renders the Job template, the ServiceAccount and the Role. Resources
# are cut to what a test cluster schedules — the defaults are a real run's.
runtime_class_line >/dev/null # the same up-front check the fixtures get
helm upgrade --install "$ORCH" "$E2E_DIR/../charts/orchestrator" -n "$NS" \
  --set-string sandbox.image=alpine/git:v2.54.0 \
  --set-string sandbox.runtimeClassName="${RUNTIME_CLASS:-}" \
  --set-file sandbox.script="$PROBE" \
  --set-string proxyCAConfigMapName=proxy-ca \
  --set-string sandbox.resources.requests.cpu=50m \
  --set-string sandbox.resources.requests.memory=64Mi \
  --set-string sandbox.resources.limits.memory=256Mi \
  --wait --timeout=60s >/dev/null

stage "the RBAC this ticket grants, and what it does not"
for verb in create get list; do
  kubectl auth can-i "$verb" jobs -n "$NS" --as="system:serviceaccount:$NS:$ORCH" >/dev/null \
    || { echo "!!! FAIL: the orchestrator's ServiceAccount cannot $verb jobs" >&2; exit 1; }
done
# The access a compromised orchestrator would want most, and ADR 0005 deleted the
# reason for it: the sandbox's credential lives in the proxy, so there is no
# per-run Secret to create and none to read.
if kubectl auth can-i get secrets -n "$NS" --as="system:serviceaccount:$NS:$ORCH" >/dev/null 2>&1; then
  echo "!!! FAIL: the orchestrator can read Secrets" >&2
  exit 1
fi
echo "PROBE rbac             jobs create,get,list — no Secrets, nothing cluster-scoped"

# The Job template as the chart rendered it. Read back out of the cluster rather
# than re-rendered here, so what the builder patches is what an operator installed.
kubectl -n "$NS" get cm "$ORCH-job-template" -o jsonpath='{.data.job\.yaml}' > "$TMPL"

orch() {
  RUN_KEY_FILE=$(run_key_file) POD_NAMESPACE="$NS" PROXY_HOST="$RELEASE" \
    JOB_TEMPLATE_FILE="$TMPL" go run "$E2E_DIR/../cmd/orchestrator" "$@"
}
# `<verb> <namespace>/<name> <owner>/<repo>#<n>` — one word, then the reference.
job_of() { awk '{print $2}' | cut -d/ -f2-; }

stage "the 63-character cap, asked of the apiserver"
# Created for real, not dry-run: the acceptance is that the apiserver *creates* the
# Job rather than rejecting it, and the cap comes from the label the Job controller
# stamps into spec.template — which a dry-run does check, but only a real create
# proves end to end. `implementer-kubernetes-sigs-cluster-api-provider-openstack-12345`
# is a real repository at 64 characters and is refused; the rejection names no cause
# an operator would recognise, which is why this is measured rather than reasoned.
#
# Deleted immediately rather than waited on: its repository does not exist, so its
# pod would fail for a reason that has nothing to do with the name.
LONG=$(orch run "kubernetes-sigs/cluster-api-provider-openstack-and-more-name-for-the-cap#12345" | job_of)
kubectl -n "$NS" get "job/$LONG" >/dev/null \
  || { echo "!!! FAIL: no Job named '$LONG' at the apiserver" >&2; exit 1; }
# <= and not ==, because trimming a cut that landed on a '-' legitimately gives 62.
# What must hold is that the name was truncated *and* hashed, which is the suffix.
[[ ${#LONG} -le 63 ]] || { echo "!!! FAIL: the truncated name '$LONG' is ${#LONG} chars" >&2; exit 1; }
[[ $LONG =~ -[0-9a-f]{8}$ ]] || { echo "!!! FAIL: '$LONG' does not end in a hash" >&2; exit 1; }
kubectl -n "$NS" delete "job/$LONG" --cascade=foreground --wait >/dev/null
echo "PROBE long-name        $LONG (${#LONG} chars, created by the apiserver)"

stage "the reason every name is hashed"
# Two pairs neither of which a length check separates. Normalisation is lossy —
# my_repo and my-repo both slugify to my-repo, and google-deepmind/open_spiel keeps
# its underscore while open-spiel 404s — and the '-' the components are joined with
# is legal inside an owner and inside a repo, so the join re-splits. Collide either
# pair and the second run is swallowed as redelivery: "no database" becomes
# "silently drops runs".
UNDER=$(orch run -dry-run "acme/my_repo#5" | job_of)
DASH=$(orch run -dry-run "acme/my-repo#5" | job_of)
SPLIT=$(orch run -dry-run "acme-my/repo#5" | job_of)
[[ $UNDER != "$DASH" ]] || { echo "!!! FAIL: acme/my_repo#5 and acme/my-repo#5 are both '$UNDER'" >&2; exit 1; }
[[ $SPLIT != "$DASH" ]] || { echo "!!! FAIL: acme-my/repo#5 and acme/my-repo#5 are both '$SPLIT'" >&2; exit 1; }
echo "PROBE normalisation    $DASH vs $UNDER vs $SPLIT"

stage "create the run for $RUN_REPO#$ISSUE"
# A previous run of this stage owns the only Job carrying this label, and the count
# below is the idempotency assertion — so it starts from none. Foreground cascade,
# or the old pod is still being collected when the new one is polled for.
kubectl -n "$NS" delete job -l app=implementer --cascade=foreground --wait >/dev/null
JOB=$(orch run "$RUN_REPO#$ISSUE" | tee_stderr | job_of)
UID_BEFORE=$(kubectl -n "$NS" get "job/$JOB" -o jsonpath='{.metadata.annotations.implementer\.dev/run-uid}')

stage "run it again — redelivery is a no-op, not a second Job and not an error"
AGAIN=$(orch run "$RUN_REPO#$ISSUE" | tee_stderr)
VERB=$(awk '{print $1}' <<<"$AGAIN")
[[ $VERB == exists ]] || { echo "!!! FAIL: the second run reported '$VERB'" >&2; exit 1; }
# And it says *which* exists. ttlSecondsAfterFinished keeps a terminal Job for a
# day, so `exists` alone covers both a redelivery and a re-run of a finished run
# that will now do nothing until the TTL expires. The phase is appended last, after
# the reference, so the two fields every other assertion here reads stay put.
[[ $AGAIN == *"(active)"* ]] || { echo "!!! FAIL: the redelivery does not name the phase: '$AGAIN'" >&2; exit 1; }
COUNT=$(kubectl -n "$NS" get job -l app=implementer -o name | wc -l)
[[ $COUNT -eq 1 ]] || { echo "!!! FAIL: $COUNT Jobs after two runs of one issue" >&2; exit 1; }
# The name is the whole of the dedupe, and AlreadyExists is swallowed rather than
# resolved: the second run's own UID must not have overwritten the first's, or the
# running pod's credential would stop matching its own annotations.
UID_AFTER=$(kubectl -n "$NS" get "job/$JOB" -o jsonpath='{.metadata.annotations.implementer\.dev/run-uid}')
[[ $UID_BEFORE == "$UID_AFTER" ]] || { echo "!!! FAIL: redelivery rewrote the run uid" >&2; exit 1; }
echo "PROBE idempotency      1 Job, uid $UID_AFTER unchanged"

stage "run identity, in both places"
# A Job's own metadata does not propagate to its pods. The pod copy is what the
# proxy resolves a source IP to, and the Job copy is what a human reads — so
# neither is redundant, and they come from one struct in the builder.
ANN='{.metadata.annotations.implementer\.dev/owner},{.metadata.annotations.implementer\.dev/repo},{.metadata.annotations.implementer\.dev/issue},{.metadata.annotations.implementer\.dev/run-uid}'
ON_JOB=$(kubectl -n "$NS" get "job/$JOB" -o jsonpath="$ANN")
ON_POD=$(kubectl -n "$NS" get "job/$JOB" -o jsonpath="${ANN//.metadata.annotations/.spec.template.metadata.annotations}")
[[ $ON_JOB == "$ON_POD" ]] || { echo "!!! FAIL: the Job says '$ON_JOB', its pod template says '$ON_POD'" >&2; exit 1; }
[[ $ON_JOB == "${RUN_REPO%%/*},${RUN_REPO#*/},$ISSUE,$UID_AFTER" ]] \
  || { echo "!!! FAIL: run identity is '$ON_JOB'" >&2; exit 1; }
echo "PROBE run-identity     $ON_JOB (on the Job and on spec.template)"

stage "the sandbox (runtimeClassName=${RUNTIME_CLASS:-<none>})"
wait_job "$JOB"

echo
echo "==> proxy log (the run authenticated with a credential the builder derived):"
kubectl -n "$NS" logs "deploy/$RELEASE" --tail=20
echo "==> the orchestrator's Job builder proven against a real cluster"
