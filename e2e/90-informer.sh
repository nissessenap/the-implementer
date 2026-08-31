#!/usr/bin/env bash
# Stage 90 — the informer, against a real cluster. Two runs end, and each one gets
# exactly one issue comment: the first from the blob it wrote, the second from the
# Kubernetes-level reason, because it wrote nothing at all.
#
# The second is the point of the whole component and the half no unit test reaches:
# when `activeDeadlineSeconds` expires the Job controller **deletes** the pod, so
# there is no termination message, no `pods/log`, and no in-pod code that ever ran.
# An in-pod reporter cannot report that. Without something watching from outside,
# the issue sits there labelled `ready-for-agent` and nobody is told the run
# happened.
#
# Credentials: none, and never skips. GitHub is a mock behind the `GITHUB_API_URL`
# seam — the precedent stage 70 set for the model route, for the reason CLAUDE.md
# records: a stage that needs a real credential is a stage that skips, and a skipped
# stage proves nothing. The App JWT is signed by ghait's `file` provider against a
# key this stage generates and throws away; production links `ghait.no_file` and
# cannot sign that way at all, which `make test` builds to prove.
#
# Depends on stages 10 and 20 for the proxy-ca ConfigMap the Job template mounts,
# exactly as stage 80 does.
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"

ORCH=${ORCH:-implementer-orchestrator}
# Not a real repository, and nothing here reaches one: the mock answers for any
# owner and repo, and the whole assertion is what the orchestrator sent.
RUN_REPO=${E2E_MOCK_REPO:-acme/widgets}
# Three runs: one that writes a blob, one whose deadline the harness expires, and
# one left running — which is the "restart mid-run" case, and must stay silent.
BLOB_ISSUE=1001
DEAD_ISSUE=1002
LIVE_ISSUE=1003

TMPL=$(mktemp)
PROBE=$(mktemp)
APP_KEY=$(mktemp)
# The key is an actual RSA private key in /tmp, so the removal must not be behind
# anything that can fail first — which is why this is `|| return 0` and not
# `[[ … ]] && { … }`, the shape lib.sh's bao_down deliberately avoids. bash has one
# EXIT trap, so github_mock_down is called from here rather than owning its own.
cleanup() {
  github_mock_down
  rm -f "$TMPL" "$PROBE" "$APP_KEY"
}
trap cleanup EXIT

# The sandbox, as this stage needs it: one script for all three runs, branching on
# the issue number the builder wrote into the environment. One chart install and one
# Job template that way, rather than three.
cat > "$PROBE" <<'PROBE_EOF'
echo "PROBE run ${REPO}#${ISSUE}"
case "$ISSUE" in
1001)
  # The ordinary ending, in the shape phase.sh writes: a review phase died, so the
  # run is completed_unreviewed and the comment has to name which one.
  printf '%s' '{"status":"completed_unreviewed","branch":"implementer/issue-1001",
"commits":3,"cost_usd":2.13,"elapsed_s":452,"pr_title":"feat: informer e2e",
"message":"pushed implementer/issue-1001",
"phases":[{"name":"implement","status":"completed","summary":"wrote it"},
{"name":"review","status":"error","summary":"API error 529 from the reviewer"},
{"name":"ponytail","status":"completed","summary":"lean already"}]}' \
    | tr -d '\n' > /dev/termination-log
  echo "PROBE wrote the result blob"
  ;;
*)
  # A run that does not end on its own. The harness expires one of these and leaves
  # the other running.
  echo "PROBE sleeping — this run does not end by itself"
  sleep 600
  ;;
esac
PROBE_EOF

stage "stand up the GitHub mock"
# Stood up by lib.sh, so a later ticket adds assertions here rather than
# infrastructure. Left standing on the way out, which is why its state is reset
# rather than assumed empty: it is one process holding the thread in memory, and a
# thread that outlived the previous run would make "exactly one comment" count that
# run's too.
github_mock_up
curl -sf --max-time 5 -X POST "$GITHUB_MOCK_ADDR/_reset" >/dev/null

stage "the App key, generated and thrown away"
# PKCS#1, which is the format GitHub hands out and the one ghait's file provider
# reads. Signed locally only because the e2e is both ends at once; the orchestrator
# in a cluster holds no key — its provider is `gcp` or `vault`, and the production
# build drops this one by tag.
openssl genrsa -traditional -out "$APP_KEY" 2048 2>/dev/null
chmod 600 "$APP_KEY"

stage "install charts/orchestrator with the probe sandbox"
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

stage "the RBAC the informer needs, and what it still does not have"
for verb in list watch; do
  for res in pods jobs; do
    kubectl auth can-i "$verb" "$res" -n "$NS" --as="system:serviceaccount:$NS:$ORCH" >/dev/null \
      || { echo "!!! FAIL: the orchestrator's ServiceAccount cannot $verb $res" >&2; exit 1; }
  done
done
# Read-only on Pods, and that is load-bearing rather than minimalism: with no way to
# write to a Pod there is nowhere in Kubernetes to mark a run as reported, which is
# why the exactly-once record is the comment itself.
for verb in patch update; do
  if kubectl auth can-i "$verb" pods -n "$NS" --as="system:serviceaccount:$NS:$ORCH" >/dev/null 2>&1; then
    echo "!!! FAIL: the orchestrator can $verb pods" >&2
    exit 1
  fi
done
echo "PROBE rbac             pods/jobs get,list,watch — read-only on pods, no Secrets"

kubectl -n "$NS" get cm "$ORCH-job-template" -o jsonpath='{.data.job\.yaml}' > "$TMPL"

orch() {
  RUN_KEY_FILE=$(run_key_file) POD_NAMESPACE="$NS" PROXY_HOST="$RELEASE" \
    JOB_TEMPLATE_FILE="$TMPL" \
    GITHUB_APP_ID=1 GITHUB_APP_PROVIDER=file GITHUB_APP_KEY="$APP_KEY" \
    GITHUB_API_URL="$GITHUB_MOCK_ADDR" \
    go run "$E2E_DIR/../cmd/orchestrator" "$@"
}
job_of() { awk '{print $2}' | cut -d/ -f2-; }
# The thread, as the mock will hand it back — which is also how the orchestrator
# reads it to decide whether it already reported.
thread() { curl -sf --max-time 5 "$GITHUB_MOCK_ADDR/repos/${RUN_REPO}/issues/$1/comments"; }
# Counted by *marker* and not by `"id":`, because the marker is what makes a comment
# one of the informer's: a run summary containing `"id":` would inflate an object
# count, and one comment carries exactly one marker.
comments() { thread "$1" | grep -o 'implementer-run:' | wc -l | tr -d ' '; }

stage "create the three runs"
kubectl -n "$NS" delete job -l app=implementer --cascade=foreground --wait >/dev/null
BLOB_JOB=$(orch run "$RUN_REPO#$BLOB_ISSUE" | tee /dev/stderr | job_of)
DEAD_JOB=$(orch run "$RUN_REPO#$DEAD_ISSUE" | tee /dev/stderr | job_of)
orch run "$RUN_REPO#$LIVE_ISSUE" | tee /dev/stderr >/dev/null

stage "the run that wrote a blob"
kubectl -n "$NS" wait --for=condition=Complete "job/$BLOB_JOB" --timeout=180s >/dev/null
orch watch -once
[[ $(comments "$BLOB_ISSUE") -eq 1 ]] ||
  { echo "!!! FAIL: $(comments "$BLOB_ISSUE") comments on #$BLOB_ISSUE, want 1" >&2; thread "$BLOB_ISSUE" >&2; exit 1; }
BODY=$(thread "$BLOB_ISSUE")
# The blob's fields, the dead phase by name, and the resolved digest — which ADR
# 0001 requires because the image and the orchestrator are a matched pair.
for want in "implementer-run: " "completed_unreviewed" "implementer/issue-1001" \
  "3 commits" "[\$]2.13" "7m32s" "API error 529" "sha256:"; do
  grep -q "$want" <<<"$BODY" ||
    { echo "!!! FAIL: the comment on #$BLOB_ISSUE does not name '$want'" >&2; echo "$BODY" >&2; exit 1; }
done
echo "PROBE blob-comment     completed_unreviewed, the dead phase named, digest included"

stage "a run still in flight says nothing, and neither does a second reconcile"
[[ $(comments "$LIVE_ISSUE") -eq 0 ]] ||
  { echo "!!! FAIL: the running run was commented on" >&2; exit 1; }
# A restart mid-run, and a redelivery: a fresh process with no memory of the first.
orch watch -once
[[ $(comments "$BLOB_ISSUE") -eq 1 ]] ||
  { echo "!!! FAIL: $(comments "$BLOB_ISSUE") comments after a second reconcile, want 1" >&2; exit 1; }
[[ $(comments "$LIVE_ISSUE") -eq 0 ]] ||
  { echo "!!! FAIL: the running run was commented on by the second reconcile" >&2; exit 1; }
echo "PROBE exactly-once     one comment across two processes, and silence mid-run"

stage "the pod that wrote nothing: expire its deadline"
# Polled for Running first, or the deadline is patched onto a Job whose pod the
# scheduler has not started and the failure is a different one. Polled rather than
# `kubectl wait --selector`, which errors out when nothing matches yet.
PHASE=
for _ in $(seq 40); do
  PHASE=$(kubectl -n "$NS" get pod -l "job-name=$DEAD_JOB" \
    -o jsonpath='{.items[0].status.phase}' 2>/dev/null || true)
  [[ $PHASE == Running ]] && break
  sleep 3
done
[[ $PHASE == Running ]] ||
  { echo "!!! FAIL: the pod of $DEAD_JOB is '${PHASE:-<none>}', not Running" >&2; exit 1; }
# The real mechanism, on demand: activeDeadlineSeconds is a mutable field, and the
# Job controller's response to an expired one is to fail the Job and **delete** the
# pod. So there is no termination message and no pod left to read — which is
# precisely the ending an in-pod reporter cannot report.
kubectl -n "$NS" patch "job/$DEAD_JOB" --type=merge -p '{"spec":{"activeDeadlineSeconds":1}}' >/dev/null
kubectl -n "$NS" wait --for=condition=Failed "job/$DEAD_JOB" --timeout=120s >/dev/null

orch watch -once
[[ $(comments "$DEAD_ISSUE") -eq 1 ]] ||
  { echo "!!! FAIL: $(comments "$DEAD_ISSUE") comments on #$DEAD_ISSUE, want 1" >&2; thread "$DEAD_ISSUE" >&2; exit 1; }
DEAD_BODY=$(thread "$DEAD_ISSUE")
for want in "without writing a result" "DeadlineExceeded"; do
  grep -q "$want" <<<"$DEAD_BODY" ||
    { echo "!!! FAIL: the comment on #$DEAD_ISSUE does not name '$want'" >&2; echo "$DEAD_BODY" >&2; exit 1; }
done
echo "PROBE silent-death     a pod that wrote nothing still produced a comment"

stage "what the orchestrator actually sent"
CALLS=$(curl -sf --max-time 5 "$GITHUB_MOCK_ADDR/_calls")
echo "$CALLS"
# The mint path: an App JWT asks which installation has the repository, and the
# installation token comes back from a POST signed the same way.
grep -q "^GET /repos/$RUN_REPO/installation auth=app-jwt$" <<<"$CALLS" ||
  { echo "!!! FAIL: the installation lookup did not arrive with an App JWT" >&2; exit 1; }
grep -q "^POST /app/installations/4242/access_tokens auth=app-jwt$" <<<"$CALLS" ||
  { echo "!!! FAIL: no mint signed by the App" >&2; exit 1; }
# And the writes: one per run, at GitHub's documented method and path, carrying the
# *installation token* rather than the App JWT — an App JWT cannot comment.
for issue in $BLOB_ISSUE $DEAD_ISSUE; do
  n=$(grep -c "^POST /repos/$RUN_REPO/issues/$issue/comments auth=token$" <<<"$CALLS" || true)
  [[ $n -eq 1 ]] || { echo "!!! FAIL: $n comment POSTs for #$issue, want 1" >&2; exit 1; }
  # And each write was preceded by the read that decides whether to write at all.
  grep -q "^GET /repos/$RUN_REPO/issues/$issue/comments auth=token$" <<<"$CALLS" ||
    { echo "!!! FAIL: #$issue was commented on without reading the thread first" >&2; exit 1; }
done
# Nothing else was written anywhere. The informer creates no objects on GitHub's
# side in this ticket: the pull request is the next one.
OTHER=$(grep '^POST ' <<<"$CALLS" | grep -v "access_tokens\|/issues/[0-9]*/comments" || true)
[[ -z $OTHER ]] || { echo "!!! FAIL: the orchestrator wrote something else: $OTHER" >&2; exit 1; }
echo "PROBE call-shape       mint as the App, comment as the installation, nothing else"

stage "clean up"
kubectl -n "$NS" delete job -l app=implementer --cascade=foreground --wait >/dev/null
# The mock Deployment stays, deliberately: it is the infrastructure a later ticket
# adds assertions to, and each stage resets its state on the way in. Only the
# port-forward is torn down, by the EXIT trap.
echo "==> the informer proven: a blob became a comment, and so did a pod that wrote none"
