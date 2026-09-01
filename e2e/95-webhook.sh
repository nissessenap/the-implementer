#!/usr/bin/env bash
# Stage 95 — the trigger. A signed `issues` webhook lands on the orchestrator's
# Service and a run exists; every other shape of the same event lands and nothing
# does.
#
# Credentials: none, and it never skips. That is not a compromise here — it is the
# shape of the component. The webhook front-end holds **no GitHub credential at
# all**: the authorization is two clauses on the payload, and the refusal is silent
# on purpose, so there is nothing in this half that could call GitHub even if a
# token were mounted. So the whole path is exercisable with a payload this stage
# signs itself.
#
# **No public endpoint and no tunnel.** The delivery is a POST at the Service
# through a port-forward, which is what makes this runnable unattended in CI — the
# alternative is ngrok, a real App, and a stage that only ever runs on a laptop.
#
# The ignored cases are asserted by the **absence of a Job**, and each one uses its
# own issue number: sharing one would make "still exactly one Job" true whether the
# case was ignored or wrongly started a run for an issue that already had one.
#
# Depends on stages 10 and 20 for the proxy-ca ConfigMap the Job template mounts,
# exactly as stages 80 and 90 do.
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "$0")/lib.sh"

ORCH=${ORCH:-implementer-orchestrator}
# Not a real repository, and nothing here reaches one — the webhook makes no API
# call, so the payload is the whole of the input.
RUN_REPO=${E2E_MOCK_REPO:-acme/widgets}
# The one delivery that must start a run, and one issue per ignored case.
RUN_ISSUE=4242
OTHER_LABEL_ISSUE=4301
NO_LABEL_ISSUE=4302
BOT_ISSUE=4303
GHOST_ISSUE=4304
BAD_SIG_ISSUE=4305

# The webhook secret, a literal for lib.sh's RUN_KEY reason: this stage is both
# ends at once — it puts the Secret in the cluster and signs the delivery with it.
WEBHOOK_SECRET=${WEBHOOK_SECRET:-e2e-webhook-secret-not-a-real-one}
WEBHOOK_PORT=${WEBHOOK_PORT:-18095}
WEBHOOK_ADDR=http://127.0.0.1:$WEBHOOK_PORT

PROBE=$(mktemp)
cleanup() {
  if [[ -n ${WEBHOOK_PF:-} ]]; then
    kill "$WEBHOOK_PF" 2>/dev/null || true
    # Reaped, not just signalled: `kill` returns when the signal is sent and not
    # when the socket is released, and a leaked forward makes the *next* run's
    # health check pass against a dead tunnel.
    wait "$WEBHOOK_PF" 2>/dev/null || true
  fi
  rm -f "$PROBE"
}
trap cleanup EXIT

# A run that does not end on its own, which is what makes "the handler responds
# without waiting for the run" assertable: the POST comes back 202 while this is
# still sleeping.
cat > "$PROBE" <<'PROBE_EOF'
echo "PROBE run ${REPO}#${ISSUE}"
sleep 600
PROBE_EOF

stage "build the orchestrator image with ko"
# Prints the reference and nothing else, like `make image`. Its tag is a hash of
# the binary, so an unchanged orchestrator reinstalls as a no-op.
IMG=$(make -s -C "$E2E_DIR/.." orchestrator-image)
stage "load $IMG"
load_image "$IMG"

stage "the two Secrets the front-end mounts"
# The run key is the proxy's too — one Secret, both charts, neither owning it.
kubectl -n "$NS" create secret generic proxy-run-key \
  --from-literal=key="$RUN_KEY" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$NS" create secret generic orchestrator-webhook \
  --from-literal=secret="$WEBHOOK_SECRET" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

stage "install charts/orchestrator with its Deployment"
runtime_class_line >/dev/null # the same up-front check the fixtures get
helm upgrade --install "$ORCH" "$E2E_DIR/../charts/orchestrator" -n "$NS" \
  --set-string image="$IMG" \
  --set-string webhook.secretName=orchestrator-webhook \
  --set-string proxyHost="$RELEASE" \
  --set-string sandbox.image=alpine/git:v2.54.0 \
  --set-string sandbox.runtimeClassName="${RUNTIME_CLASS:-}" \
  --set-file sandbox.script="$PROBE" \
  --set-string proxyCAConfigMapName=proxy-ca \
  --set-string sandbox.resources.requests.cpu=50m \
  --set-string sandbox.resources.requests.memory=64Mi \
  --set-string sandbox.resources.limits.memory=256Mi \
  --wait --timeout=120s >/dev/null
kubectl -n "$NS" rollout status "deploy/$ORCH" --timeout=120s

stage "what the front-end is not holding"
# Structural, and the reason two acceptance criteria need no runtime assertion:
# with no App id, no signer reference and no token mount, there is nothing in this
# pod that could call the collaborator-permission endpoint or post a refusal
# comment. The silence is the security property, so it is asserted as an absence.
# An allow-list and not a deny-list: greping for the three names a credential
# *usually* arrives under passes an `envFrom: secretRef` or a volume called
# something else, and what is being asserted is an absence. So the set of env
# names is compared whole, and anything new has to be added here deliberately.
ENVS=$(kubectl -n "$NS" get "deploy/$ORCH" \
  -o jsonpath='{.spec.template.spec.containers[*].env[*].name}' | tr ' ' '\n' | sort | paste -sd,)
# The five this install renders; TOOLCHAIN is a sixth when `toolchain` is set,
# which it is not above.
WANT=$(printf '%s\n' GITHUB_WEBHOOK_SECRET JOB_TEMPLATE_FILE POD_NAMESPACE PROXY_HOST RUN_KEY_FILE |
  sort | paste -sd,)
[[ $ENVS == "$WANT" ]] ||
  { echo "!!! FAIL: the front-end's env is [$ENVS], want [$WANT]" >&2; exit 1; }
# Nothing arriving whole from a Secret or ConfigMap either, which is the shape the
# name check above cannot see.
[[ $(kubectl -n "$NS" get "deploy/$ORCH" -o jsonpath='{.spec.template.spec.containers[*].envFrom}') == "" ]] ||
  { echo "!!! FAIL: the front-end uses envFrom" >&2; exit 1; }
echo "PROBE no-credential    env is exactly [$ENVS], and no envFrom"

stage "forward to svc/$ORCH — a Service, not a public endpoint"
kubectl -n "$NS" port-forward "svc/$ORCH" "$WEBHOOK_PORT:8080" >/dev/null 2>&1 &
WEBHOOK_PF=$!
for _ in $(seq 30); do
  # The forward is a *background* job, so `set -e` cannot see it fail and its
  # output is discarded: a port already in use leaves this loop polling whatever
  # else answers there. Checked before the probe rather than trusted after it.
  kill -0 "$WEBHOOK_PF" 2>/dev/null ||
    { echo "!!! FAIL: the port-forward to $ORCH died — is $WEBHOOK_PORT in use?" >&2; exit 1; }
  curl -sf --max-time 2 "$WEBHOOK_ADDR/healthz" >/dev/null && break
  sleep 1
done
curl -sf --max-time 2 "$WEBHOOK_ADDR/healthz" >/dev/null ||
  { echo "!!! FAIL: no orchestrator on $WEBHOOK_ADDR" >&2; exit 1; }

# payload <action> <label-or-empty> <sender-login> <sender-type> <issue> — an
# `issues` delivery in the shape GitHub sends. The whole of the **required** set is
# here (action, issue, repository, sender) and `label` is deliberately not in it:
# an empty second argument produces `"label": null`, which is a real delivery shape
# and the one this handler must ignore rather than dereference.
payload() {
  local lbl='null'
  [[ -n $2 ]] && lbl=$(printf '{"id":208045946,"name":"%s","color":"0e8a16","default":false}' "$2")
  printf '{"action":"%s","label":%s,' "$1" "$lbl"
  printf '"issue":{"number":%s,"title":"a ticket","state":"open","user":{"login":"someone","type":"User"}},' "$5"
  printf '"repository":{"id":1296269,"name":"%s","full_name":"%s","private":false,"owner":{"login":"%s","type":"User"}},' \
    "${RUN_REPO#*/}" "$RUN_REPO" "${RUN_REPO%%/*}"
  printf '"sender":{"login":"%s","type":"%s"},"installation":{"id":4242}}' "$3" "$4"
}

# deliver <event> <body> [signature] — POST one delivery and print the status code.
# The signature is computed here from the same literal the Secret holds, which is
# the whole of the authentication: GitHub signs the raw body with HMAC-SHA256.
deliver() {
  local sig=${3:-}
  if [[ -z $sig ]]; then
    sig="sha256=$(printf '%s' "$2" | openssl dgst -sha256 -hmac "$WEBHOOK_SECRET" | awk '{print $NF}')"
  fi
  curl -s -o /dev/null -w '%{http_code}' --max-time 10 -X POST "$WEBHOOK_ADDR/webhook" \
    -H 'Content-Type: application/json' \
    -H "X-GitHub-Event: $1" \
    -H "X-GitHub-Delivery: $(head -c8 /dev/urandom | od -An -tx1 | tr -d ' \n')" \
    -H "X-Hub-Signature-256: $sig" \
    --data-binary "$2"
}

jobs_of() { kubectl -n "$NS" get job -l app=implementer -o name | wc -l | tr -d ' '; }
# want <status> <expected-jobs> <what> — every case is this shape.
want() {
  local got=$1 code=$2 n=$3 what=$4
  [[ $got == "$code" ]] || { echo "!!! FAIL: $what answered $got, want $code" >&2; exit 1; }
  local have
  have=$(jobs_of)
  [[ $have -eq $n ]] || { echo "!!! FAIL: $what left $have Jobs, want $n" >&2; exit 1; }
}

stage "label an issue ready-for-agent — one run"
kubectl -n "$NS" delete job -l app=implementer --cascade=foreground --wait >/dev/null
want "$(deliver issues "$(payload labeled ready-for-agent a-maintainer User "$RUN_ISSUE")")" \
  202 1 "the labelled issue"
JOB=$(kubectl -n "$NS" get job -l app=implementer -o jsonpath='{.items[0].metadata.name}')
ANN='{.metadata.annotations.implementer\.dev/owner}/{.metadata.annotations.implementer\.dev/repo}#{.metadata.annotations.implementer\.dev/issue}'
IDENT=$(kubectl -n "$NS" get "job/$JOB" -o jsonpath="$ANN")
[[ $IDENT == "$RUN_REPO#$RUN_ISSUE" ]] ||
  { echo "!!! FAIL: the run says '$IDENT', want '$RUN_REPO#$RUN_ISSUE'" >&2; exit 1; }
UID_BEFORE=$(kubectl -n "$NS" get "job/$JOB" -o jsonpath='{.metadata.annotations.implementer\.dev/run-uid}')
echo "PROBE trigger          $JOB for $IDENT, uid $UID_BEFORE"

stage "the response did not wait for the run"
# The Job exists with no terminal condition and its probe sleeps for ten minutes,
# so the 202 above was answered while the run was still in flight — which is the
# whole shape of this component: it creates one object and is done.
COND=$(kubectl -n "$NS" get "job/$JOB" -o jsonpath='{.status.conditions[*].type}')
[[ -z $COND ]] || { echo "!!! FAIL: the run already ended ($COND) — it cannot have been unwaited" >&2; exit 1; }
echo "PROBE no-wait          202 answered with the run still active"

stage "redelivery, and labelling twice"
# The same delivery again, and then the same event with a fresh delivery id: both
# resolve to the Job name plus a swallowed AlreadyExists, which is the whole of
# idempotency. The uid must survive it, or the running pod's credential would stop
# matching its own annotations.
BODY=$(payload labeled ready-for-agent a-maintainer User "$RUN_ISSUE")
want "$(deliver issues "$BODY")" 202 1 "the redelivery"
want "$(deliver issues "$BODY")" 202 1 "the second label"
UID_AFTER=$(kubectl -n "$NS" get "job/$JOB" -o jsonpath='{.metadata.annotations.implementer\.dev/run-uid}')
[[ $UID_BEFORE == "$UID_AFTER" ]] || { echo "!!! FAIL: redelivery rewrote the run uid" >&2; exit 1; }
echo "PROBE idempotency      3 deliveries, 1 Job, uid unchanged"

stage "everything that is not a run"
# 200 on all of them, deliberately: a non-2xx marks the delivery failed on the
# App's own page and invites GitHub to redeliver an event we will ignore again.
want "$(deliver issues "$(payload labeled needs-triage a-maintainer User "$OTHER_LABEL_ISSUE")")" \
  200 1 "some other label"
# The payload trap: `label` is not in the required set, so this is a delivery
# GitHub can really send. Ignored, and — asserted below — not a panic.
want "$(deliver issues "$(payload labeled '' a-maintainer User "$NO_LABEL_ISSUE")")" \
  200 1 "labeled with no label object"
# The clause the flatt.tech bypass actually turned on. The attack needs no access
# to the target repository: install your own App on your own repository and use its
# installation token here.
want "$(deliver issues "$(payload labeled ready-for-agent 'some-app[bot]' Bot "$BOT_ISSUE")")" \
  200 1 "a bot sender"
# Substituted by GitHub for an unresolvable actor, and its type is `User` — so the
# type assertion alone lets it through.
want "$(deliver issues "$(payload labeled ready-for-agent ghost User "$GHOST_ISSUE")")" \
  200 1 "the ghost sender"
want "$(deliver issues "$(payload unlabeled ready-for-agent a-maintainer User "$OTHER_LABEL_ISSUE")")" \
  200 1 "unlabeled"
# The delivery GitHub sends the moment the webhook is created.
want "$(deliver ping '{"zen":"Non-blocking is better than blocking."}')" 200 1 "a ping"
echo "PROBE ignored          other label, no label, bot, ghost, unlabeled, ping — no Job for any"

stage "an invalid signature is rejected"
want "$(deliver issues "$(payload labeled ready-for-agent a-maintainer User "$BAD_SIG_ISSUE")" \
  "sha256=$(printf '0%.0s' $(seq 64))")" 401 1 "a wrong signature"
echo "PROBE signature        401, and no Job"

stage "the refusals were silent, and nothing crashed"
# restartCount for a crash, and the log for a panic — they are different failures.
# net/http recovers a handler panic, logs it and closes the connection, so the
# process survives with restartCount 0: the label-less payload dereferencing nil
# would show up only as "http: panic serving" below, which is why both are asserted.
RESTARTS=$(kubectl -n "$NS" get pod -l "app=$ORCH,component=webhook" -o jsonpath='{.items[*].status.containerStatuses[*].restartCount}')
[[ ${RESTARTS// /} == 0 ]] || { echo "!!! FAIL: the front-end restarted ($RESTARTS)" >&2; exit 1; }
echo
echo "==> orchestrator log (every refusal is here and nowhere else):"
kubectl -n "$NS" logs "deploy/$ORCH" --tail=30
LOG=$(kubectl -n "$NS" logs "deploy/$ORCH")
# Logged is the whole of the refusal. On a public repository, commenting instead
# would hand an unauthorized actor an on-demand way to make the App write to
# issues — the `issues: write` plus untrusted-input combination the disclosure
# flags. So the log is the only channel, and it has to name who was refused.
for who in 'some-app\[bot\]' ghost; do
  grep -q "$who" <<<"$LOG" || { echo "!!! FAIL: the log does not name the refused sender $who" >&2; exit 1; }
done
! grep -q "http: panic serving" <<<"$LOG" || { echo "!!! FAIL: a handler panicked" >&2; exit 1; }
echo "PROBE silent-refusal   logged, never commented — and no restart"

stage "clean up"
kubectl -n "$NS" delete job -l app=implementer --cascade=foreground --wait >/dev/null
echo "==> the trigger proven: a signed label became a run, and nothing else did"
