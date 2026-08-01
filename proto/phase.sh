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

say()  { printf '\n=== %s\n' "$*" >&2; }
probe() { printf 'PROBE %-16s %s\n' "$1" "$2" >&2; }

# ponytail: jq -Rs is the only string escaper we have in a sh image; good enough
# for a 4KB status blob, not good enough for the real orchestrator.
termlog() {
  jq -cn \
    --arg status "$1" --arg phase "$PHASE" --arg message "$2" \
    --arg termlog "$PROBE_TERMLOG" --arg bwrap "$PROBE_BWRAP" \
    --arg sandbox "$PROBE_SANDBOX" \
    '{status:$status, phase:$phase, message:$message,
      probes:{termination_log:$termlog, bubblewrap:$bwrap, sandbox:$sandbox}}' \
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
