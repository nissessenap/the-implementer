#!/bin/sh
# PROTOTYPE phase script — throwaway. See proto/README.md.
#
# This is the run plan, as ADR 0002 says it should be: the pod's own command,
# deterministic shell with one agent CLI invocation in the middle. It is
# deliberately noisy — every probe result is printed, because the point of this
# script is to learn, not to be tidy.
set -eu

REPO=${REPO:?REPO=owner/name required}
ISSUE=${ISSUE:?ISSUE=<number> required}
WORKSPACE=${WORKSPACE:-/workspace}
BRANCH="implementer/issue-${ISSUE}${BRANCH_SUFFIX:-}"
TERM_LOG=/dev/termination-log
PHASE=preflight

# Probe outcomes, accumulated so they reach the termination log as well as stdout.
PROBE_TERMLOG=untested
PROBE_BWRAP=untested
PROBE_SANDBOX=untested
PROBE_CA=absent

say()  { printf '\n=== %s\n' "$*" >&2; }
probe() { printf 'PROBE %-16s %s\n' "$1" "$2" >&2; }

# ponytail: jq -Rs is the only string escaper we have in a sh image; good enough
# for a 4KB status blob, not good enough for the real orchestrator.
termlog() {
  jq -cn \
    --arg status "$1" --arg phase "$PHASE" --arg message "$2" \
    --arg termlog "$PROBE_TERMLOG" --arg bwrap "$PROBE_BWRAP" \
    --arg sandbox "$PROBE_SANDBOX" --arg ca "$PROBE_CA" \
    '{status:$status, phase:$phase, message:$message,
      probes:{termination_log:$termlog, bubblewrap:$bwrap, sandbox:$sandbox,
              proxy_ca:$ca}}' \
  | cut -c1-4000 > "$TERM_LOG" 2>/dev/null \
  || echo "!!! could not write $TERM_LOG" >&2
}

die() { termlog failed "$1"; echo "!!! FAILED in $PHASE: $1" >&2; exit 1; }

# ---------------------------------------------------------------- preflight ---
say "preflight"
for c in claude git gh jq bwrap; do
  command -v "$c" >/dev/null || die "image contract violation: $c not on PATH"
done
probe "image-contract" "OK ($(claude --version 2>&1 | head -1))"

# Issue #34. The proxy terminates GitHub's TLS, so the sandbox must trust the CA
# that signed its cert. Concatenated onto the system bundle rather than mounted
# over it, because SSL_CERT_FILE and GIT_SSL_CAINFO *replace* the trust store
# rather than adding to it — pointing them at ca.crt alone would leave the
# sandbox unable to verify anything else on the internet.
# ponytail: assembled here because /etc/ssl/certs is read-only at run time. In
# production the image ships the bundle or an initContainer writes it; this is
# the one line of the run plan the trust seam costs.
if [ -f /run/proxy-ca/ca.crt ]; then
  cat /etc/ssl/certs/ca-certificates.crt /run/proxy-ca/ca.crt > /tmp/ca-bundle.crt
  export SSL_CERT_FILE=/tmp/ca-bundle.crt \
         GIT_SSL_CAINFO=/tmp/ca-bundle.crt \
         CURL_CA_BUNDLE=/tmp/ca-bundle.crt \
         NODE_EXTRA_CA_CERTS=/run/proxy-ca/ca.crt
  PROBE_CA="trusted ($(grep -c 'BEGIN CERTIFICATE' /tmp/ca-bundle.crt) certs)"
fi
probe "proxy-ca" "$PROBE_CA"

# ADR 0001 / issue #22 question 1: is /dev/termination-log writable when the
# root filesystem is read-only? The entire compact result channel depends on it.
if printf 'preflight-probe' > "$TERM_LOG" 2>/dev/null; then
  PROBE_TERMLOG=writable
else
  PROBE_TERMLOG=NOT-WRITABLE
fi
probe "termination-log" "$PROBE_TERMLOG"

# ADR 0001 / issue #22 question 2: does bubblewrap initialise here? Claude Code's
# subprocess env scrubbing needs it, and it needs a user namespace to start.
# Under Docker's default seccomp profile this fails outright, so the interesting
# question is what a *Kubernetes* seccomp profile does. Both variants are probed
# because upstream reports --unshare-net specifically failing under gVisor.
bw() { bwrap --ro-bind / / "$@" true 2>/tmp/bwrap.err && echo ok || echo "FAILED: $(tr -d '\n' < /tmp/bwrap.err | cut -c1-120)"; }
PROBE_BWRAP="plain=$(bw) net=$(bw --unshare-net)"
probe "bubblewrap" "$PROBE_BWRAP"

probe "whoami" "uid=$(id -u) gid=$(id -g) home=${HOME:-unset}"

# Am I actually in a gVisor sandbox? gVisor serves its own dmesg and announces
# itself there; on host runc dmesg is empty or denied (kernel.dmesg_restrict).
# This is the only probe that distinguishes "RuntimeClass applied" from
# "RuntimeClass silently ignored", which is worth knowing before trusting a run.
PROBE_SANDBOX=$(dmesg 2>/dev/null | head -1 | cut -c1-90)
probe "sandbox" "dmesg=${PROBE_SANDBOX:-<empty/denied>} kernel=$(uname -r)"
probe "rootfs" "$(touch /rootfs-probe 2>/dev/null && echo WRITABLE || echo read-only)"
probe "workspace" "$(touch "${WORKSPACE}/.probe" 2>/dev/null && echo writable || echo NOT-WRITABLE)"
probe "home" "$(touch "${HOME:-/nonexistent}/.probe" 2>/dev/null && echo writable || echo NOT-WRITABLE)"

# Issue #33. Two assertions, both free:
#  1. the sandbox holds no model credential at all when routing via the proxy;
#  2. https_proxy actually catches the tools. That the commands *succeed* proves
#     little on an open network — the evidence is the proxy's own CONNECT log, so
#     the point of running them here is to generate entries in it.
#     `go` is probed separately (proto/proxy/probe.sh) rather than bloating this
#     image with a toolchain it does not otherwise need.
probe "model-creds" "vertex=${CLAUDE_CODE_USE_VERTEX:-0} base=${ANTHROPIC_VERTEX_BASE_URL:-none} oauth=${CLAUDE_CODE_OAUTH_TOKEN:+SET}${CLAUDE_CODE_OAUTH_TOKEN:-absent} apikey=${ANTHROPIC_API_KEY:+SET}${ANTHROPIC_API_KEY:-absent}"
if [ -n "${https_proxy:-}" ]; then
  PROBE_PROXY="git=$(git ls-remote https://github.com/git/git HEAD >/dev/null 2>&1 && echo ok || echo FAILED)"
  PROBE_PROXY="$PROBE_PROXY gh=$(gh api rate_limit >/dev/null 2>&1 && echo ok || echo FAILED)"
  probe "https_proxy" "${https_proxy} :: $PROBE_PROXY"
  # Issue #34, and the cheapest possible proof of the swap: GitHub's rate limit is
  # 60/h anonymous and 5000/h authenticated. The sandbox holds only the sentinel,
  # so a 5000 here means the proxy substituted a real token mid-flight — and it
  # means git and gh both trusted a certificate GitHub never signed.
  probe "gh-token" "sentinel=${GH_TOKEN:-unset} rate_limit=$(gh api rate_limit --jq .rate.limit 2>&1 | tr -d '\n' | cut -c1-80)"
  # The other credential shape, and the one #33's proxy never saw: git puts the
  # token in the clone URL's *userinfo*, which reaches the proxy as HTTP basic,
  # not as a Bearer header. `service=git-receive-pack` is the push handshake —
  # it demands a push-capable credential even on a public repo, and mutates
  # nothing, so the whole clone-and-push auth path is provable for free.
  probe "git-basic" "receive-pack=$(curl -fsS -o /dev/null -w '%{http_code}' \
    -u "x-access-token:${GH_TOKEN}" \
    "https://github.com/${REPO}.git/info/refs?service=git-receive-pack" 2>&1 \
    | tr -d '\n' | cut -c1-60)"
  # ...and the same thing through git itself rather than curl, because whether
  # git *sends* basic is git's business, and push is the whole prize in #34.
  # --dry-run runs the real receive-pack handshake and mutates nothing.
  if git clone -q --depth 1 "https://x-access-token:${GH_TOKEN}@github.com/${REPO}.git" \
       /tmp/mitm-probe 2>/tmp/clone.err; then
    PROBE_GIT="clone=ok push-dry-run=$(git -C /tmp/mitm-probe push --dry-run -q \
      origin HEAD:refs/heads/implementer-mitm-probe >/dev/null 2>/tmp/push.err \
      && echo ok || echo "FAILED: $(tr -d '\n' < /tmp/push.err | cut -c1-70)")"
  else
    PROBE_GIT="clone=FAILED: $(tr -d '\n' < /tmp/clone.err | cut -c1-90)"
  fi
  probe "git-sentinel" "$PROBE_GIT"
  rm -rf /tmp/mitm-probe
fi

# SMOKE=1: one agent turn and nothing else. Issue #33 needs to know that Claude
# Code itself reaches the model through the proxy while holding no credential of
# its own — that is a startup-and-one-request question, not an implement-an-issue
# question, and this way it costs a fraction of a cent instead of a dollar.
# --debug so the resolved provider, base URL and model land in the log.
if [ -n "${SMOKE:-}" ]; then
  say "agent smoke: one turn through ${ANTHROPIC_VERTEX_BASE_URL:-the default endpoint}"
  SMOKE_START=$(date +%s)
  set +e
  claude -p 'Reply with exactly: SMOKE-OK' --debug \
    --dangerously-skip-permissions --output-format json \
    --max-budget-usd "${MAX_BUDGET_USD:-10}" --max-turns 1 > /tmp/smoke.json 2>/tmp/smoke.err
  SMOKE_RC=$?
  set -e
  probe "smoke-rc" "$SMOKE_RC elapsed=$(( $(date +%s) - SMOKE_START ))s"
  probe "smoke-result" "$(jq -c '{is_error, total_cost_usd, num_turns, result}' /tmp/smoke.json 2>/dev/null | cut -c1-300 || cut -c1-300 /tmp/smoke.json)"
  # The provider/model the CLI actually resolved, which is the interesting half.
  grep -iE 'vertex|aiplatform|model|provider' /tmp/smoke.err | tail -25 >&2 || true
fi

# Probe-only mode: everything above costs nothing, so it is worth running on its
# own against different security contexts before spending a real agent run.
if [ -n "${PREFLIGHT_ONLY:-}" ]; then
  termlog probes "preflight only"
  say "PREFLIGHT_ONLY set — stopping here"
  exit 0
fi

# -------------------------------------------------------------------- clone ---
PHASE=clone
say "clone ${REPO}"
: "${GH_TOKEN:?GH_TOKEN required}"
git clone --quiet "https://x-access-token:${GH_TOKEN}@github.com/${REPO}.git" \
  "${WORKSPACE}/repo" || die "clone failed"
cd "${WORKSPACE}/repo"
git checkout -q -b "$BRANCH"
BASE_SHA=$(git rev-parse HEAD)
probe "base-sha" "$BASE_SHA"

# -------------------------------------------------------------------- brief ---
PHASE=brief
say "fetch issue #${ISSUE}"
gh issue view "$ISSUE" --repo "$REPO" --json title,body,comments \
  > "${WORKSPACE}/issue.json" || die "could not read issue"

# NOTE: the real plugin registers the skill as `mattpocock-skills:implement`, not
# bare `implement` — contradicting ADR 0001's incidental finding, which was made
# against a hand-rolled single-skill plugin. Using the name the plugin actually
# advertises; bare `/implement` is worth a separate check.
PROMPT=$(jq -r --arg repo "$REPO" --arg issue "$ISSUE" '
  "/mattpocock-skills:implement\n\n" +
  "You are implementing GitHub issue #\($issue) in the repository \($repo).\n" +
  "The Agent Brief below is the authoritative specification. Work only from it.\n" +
  "You are running unattended: nobody can answer a question. If the brief is\n" +
  "genuinely unimplementable, stop and report status \"blocked\".\n\n" +
  "--- ISSUE: \(.title) ---\n\(.body)\n" +
  ([.comments[]? | "\n--- COMMENT by \(.author.login) ---\n\(.body)"] | join("\n"))
' "${WORKSPACE}/issue.json")

# ---------------------------------------------------------------- implement ---
PHASE=implement
say "claude -p /implement"
STARTED=$(date +%s)
set +e
# --json-schema wants the schema inline, not a path (it JSON-parses the argument).
# And $? after a pipe is tee's, not the agent's — so the rc goes via a file.
{
  claude -p "$PROMPT" \
    --plugin-dir /opt/skills \
    --dangerously-skip-permissions \
    --output-format stream-json --verbose \
    --json-schema "$(cat /opt/result.schema.json)" \
    --max-budget-usd "${MAX_BUDGET_USD:-10}" \
    --max-turns 200
  echo $? > /tmp/agent.rc
} | tee "${WORKSPACE}/stream.jsonl"
AGENT_RC=$(cat /tmp/agent.rc 2>/dev/null || echo "?")
set -e
ELAPSED=$(( $(date +%s) - STARTED ))
probe "agent-exit" "rc=${AGENT_RC} elapsed=${ELAPSED}s"

# The audit found multiple `result` messages are possible — take the last.
RESULT=$(jq -c 'select(.type=="result")' "${WORKSPACE}/stream.jsonl" | tail -1)
[ -n "$RESULT" ] || die "no result message (rc=${AGENT_RC}): $(tail -c 300 "${WORKSPACE}/stream.jsonl" 2>/dev/null)"

STRUCTURED=$(printf '%s' "$RESULT" | jq -c '.structured_output // empty')
COST=$(printf '%s' "$RESULT" | jq -r '.total_cost_usd // "?"')
probe "cost-usd" "$COST"
probe "structured-output" "${STRUCTURED:-ABSENT}"

# Process success != task success. Trust the structured status, not the exit code.
AGENT_STATUS=$(printf '%s' "${STRUCTURED:-{\}}" | jq -r '.status // "unknown"')
# ...but a result carrying is_error is never a success, whatever it says.
if [ "$(printf '%s' "$RESULT" | jq -r '.is_error // false')" = "true" ]; then
  die "agent reported is_error: $(printf '%s' "$RESULT" | jq -r '.result // ""' | cut -c1-200)"
fi

# --------------------------------------------------------------------- push ---
PHASE=push
COMMITS=$(git rev-list --count "${BASE_SHA}..HEAD")
probe "commits" "$COMMITS"
if [ "$COMMITS" -eq 0 ]; then
  termlog "$AGENT_STATUS" "agent produced no commits (cost=\$${COST}, ${ELAPSED}s)"
  say "no commits — nothing to push"
  exit 1
fi

say "push ${BRANCH}"
git push -q origin "$BRANCH" || die "push failed"
probe "pushed" "${BRANCH} (+${COMMITS} commits)"

# ------------------------------------------------------------------- report ---
PHASE=done
SUMMARY=$(printf '%s' "${STRUCTURED:-{\}}" | jq -r '.summary // "no summary"')
termlog "$AGENT_STATUS" "branch=${BRANCH} commits=${COMMITS} cost=\$${COST} elapsed=${ELAPSED}s :: ${SUMMARY}"
say "done — status=${AGENT_STATUS} branch=${BRANCH}"
