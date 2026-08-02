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
# Overridable only so proto/local.sh can point it at a bind mount; in the PodSpec
# it is always the real thing, which is the channel ADR 0002 depends on.
TERM_LOG=${TERM_LOG:-/dev/termination-log}
PHASE=preflight
RUN_STARTED=$(date +%s)

# Probe outcomes, accumulated so they reach the termination log as well as stdout.
PROBE_TERMLOG=untested
PROBE_BWRAP=untested
PROBE_SANDBOX=untested
PROBE_CA=absent
PROBE_PLUGINS=untested

# Issue #31: the per-phase record. #12 says the termination-log blob accumulates a
# status/summary line per phase and a *summed* cost, written once at the end.
PHASES_JSON='[]'
COST_TOTAL=0
PR_TITLE=''

say()  { printf '\n=== %s\n' "$*" >&2; }
probe() { printf 'PROBE %-16s %s\n' "$1" "$2" >&2; }

# Issue #31, question 3: do *both* --plugin-dir flags resolve in one session?
# Answered off the init system message, which carries plugin_errors and the whole
# slash_commands list — so any invocation answers it and none is needed just for it.
plugin_counts() {
  jq -r 'select(.type=="system" and .subtype=="init")
    | "errors=\(.plugin_errors // "absent")"
    + " mattpocock=\([.slash_commands[]? | select(startswith("mattpocock-skills:"))] | length)"
    + " ponytail=\([.slash_commands[]? | select(startswith("ponytail:"))] | length)"
    + " review_cmds=\([.slash_commands[]? | select(test("code-review|ponytail-review"))] | join(","))"' \
    "$1" 2>/dev/null | head -1
}

# ponytail: jq -Rs is the only string escaper we have in a sh image; good enough
# for a 4KB status blob, not good enough for the real orchestrator.
termlog() {
  jq -cn \
    --arg status "$1" --arg phase "$PHASE" --arg message "$2" \
    --arg termlog "$PROBE_TERMLOG" --arg bwrap "$PROBE_BWRAP" \
    --arg sandbox "$PROBE_SANDBOX" --arg ca "$PROBE_CA" \
    --arg plugins "$PROBE_PLUGINS" --arg pr_title "$PR_TITLE" \
    --arg cost "$COST_TOTAL" --argjson phases "$PHASES_JSON" \
    '{status:$status, phase:$phase, message:$message,
      pr_title:$pr_title, cost_usd:$cost, phases:$phases,
      probes:{termination_log:$termlog, bubblewrap:$bwrap, sandbox:$sandbox,
              proxy_ca:$ca, plugins:$plugins}}' \
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
# ponytail: `${X:+SET}${X:-absent}` printed the *token* when X was set — the `:-`
# branch expands to $X, not to the default, so the two forms do not compose. A
# probe that leaks the credential into pod logs is worse than no probe.
isset() { [ -n "$1" ] && echo SET || echo absent; }
probe "model-creds" "vertex=${CLAUDE_CODE_USE_VERTEX:-0} base=${ANTHROPIC_VERTEX_BASE_URL:-none} oauth=$(isset "${CLAUDE_CODE_OAUTH_TOKEN:-}") apikey=$(isset "${ANTHROPIC_API_KEY:-}")"
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
  # Both --plugin-dir flags, because issue #31 question 3 — does ponytail resolve
  # from a *second* --plugin-dir? — is answerable from the init message alone, and
  # this is the cheapest invocation in the script. stream-json for that reason:
  # --output-format json emits only the result, never the init message.
  claude -p 'Reply with exactly: SMOKE-OK' --debug \
    --plugin-dir /opt/skills --plugin-dir /opt/ponytail \
    --dangerously-skip-permissions --output-format stream-json --verbose \
    --max-budget-usd "${MAX_BUDGET_USD:-10}" --max-turns 1 > /tmp/smoke.json 2>/tmp/smoke.err
  SMOKE_RC=$?
  set -e
  probe "smoke-rc" "$SMOKE_RC elapsed=$(( $(date +%s) - SMOKE_START ))s"
  probe "smoke-result" "$(jq -c 'select(.type=="result") | {is_error, total_cost_usd, num_turns, result}' /tmp/smoke.json 2>/dev/null | tail -1 | cut -c1-300 || cut -c1-300 /tmp/smoke.json)"
  # plugin_errors must be null and *both* prefixes must appear in slash_commands.
  # ponytail: counts, not a listing — the first version of this probe cut the line
  # at 500 chars and reported zero ponytail commands because they sort last.
  probe "plugin-errors" "$(jq -c 'select(.subtype=="init") | .plugin_errors' /tmp/smoke.json 2>/dev/null | head -1)"
  probe "plugin-commands" "$(plugin_counts /tmp/smoke.json)"
  probe "node" "$(command -v node || echo ABSENT)"
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

# ------------------------------------------------------------- agent phases ---
# Issue #31 / #12. One `claude -p` per phase; a fresh process *is* the /clear.
# Everything phase-specific is the prompt and the schema — the flags are identical,
# which is the whole reason this is a function rather than three copies.
#
# ponytail: the phase list is a hardcoded sequence of three calls at the bottom,
# not a loop over a table. Three items with three different prompts is not a table.
run_phase() {
  PHASE=$1
  _prompt=$2
  _schema=$3
  _stream="${WORKSPACE}/${PHASE}.jsonl"
  _base=$(git rev-parse HEAD)

  say "phase ${PHASE}: claude -p"
  _started=$(date +%s)
  set +e
  # --json-schema wants the schema inline, not a path (it JSON-parses the argument).
  # And $? after a pipe is tee's, not the agent's — so the rc goes via a file.
  {
    claude -p "$_prompt" \
      --plugin-dir /opt/skills --plugin-dir /opt/ponytail \
      --dangerously-skip-permissions \
      --output-format stream-json --verbose \
      --json-schema "$_schema" \
      --max-budget-usd "${MAX_BUDGET_USD:-10}" \
      --max-turns 200
    echo $? > /tmp/agent.rc
  } | tee "$_stream"
  PH_RC=$(cat /tmp/agent.rc 2>/dev/null || echo "?")
  set -e
  PH_ELAPSED=$(( $(date +%s) - _started ))

  # Issue #31, question 3: do *both* plugins resolve in one session? The init
  # message carries plugin_errors and the full slash_commands list, so the first
  # phase to run answers it for free — no extra invocation.
  if [ "$PROBE_PLUGINS" = untested ]; then
    PROBE_PLUGINS=$(plugin_counts "$_stream")
    probe "plugins" "${PROBE_PLUGINS:-no init message}"
  fi

  # The audit found multiple `result` messages are possible — take the last.
  _result=$(jq -c 'select(.type=="result")' "$_stream" | tail -1)
  if [ -z "$_result" ]; then
    PH_STATUS=no_result
    PH_SUMMARY="no result message (rc=${PH_RC}): $(tail -c 200 "$_stream" 2>/dev/null | tr -d '\n')"
    PH_COST=0
  else
    _structured=$(printf '%s' "$_result" | jq -c '.structured_output // empty')
    PH_COST=$(printf '%s' "$_result" | jq -r '.total_cost_usd // 0')
    # Process success != task success. Trust the structured status, not the exit
    # code — but a result carrying is_error is never a success, whatever it says.
    if [ "$(printf '%s' "$_result" | jq -r '.is_error // false')" = "true" ]; then
      PH_STATUS=error
      PH_SUMMARY=$(printf '%s' "$_result" | jq -r '.result // ""' | cut -c1-300)
    else
      PH_STATUS=$(printf '%s' "${_structured:-{\}}" | jq -r '.status // "unknown"')
      PH_SUMMARY=$(printf '%s' "${_structured:-{\}}" | jq -r '.summary // "no summary"')
    fi
    # pr_title comes from the implement phase only (#12) — the phase that knows
    # the change's intent. The review schemas do not carry the field at all.
    _t=$(printf '%s' "${_structured:-{\}}" | jq -r '.pr_title // empty')
    [ -n "$_t" ] && [ -z "$PR_TITLE" ] && PR_TITLE=$_t
    probe "${PHASE}-structured" "${_structured:-ABSENT}"
  fi

  # Issue #31, question 5: does the phase *commit*? Both review skills only
  # report, so the prompt has to ask for the fix and the commit — these two
  # numbers are whether asking was enough. A dirty tree here is work the push
  # step would silently drop.
  PH_COMMITS=$(git rev-list --count "${_base}..HEAD")
  PH_DIRTY=$(git status --porcelain | wc -l | tr -d ' ')

  # Issue #31, question 4: nested subagents. /code-review spawns two of its own
  # and the prompt asks for a third. The tool-use histogram is the evidence.
  PH_TOOLS=$(jq -r 'select(.type=="assistant") | .message.content[]?
    | select(.type=="tool_use") | .name' "$_stream" 2>/dev/null \
    | sort | uniq -c | awk '{printf "%s=%s ", $2, $1}')

  probe "${PHASE}-exit" "rc=${PH_RC} status=${PH_STATUS} elapsed=${PH_ELAPSED}s cost=\$${PH_COST} commits=+${PH_COMMITS} dirty=${PH_DIRTY}"
  probe "${PHASE}-tools" "${PH_TOOLS:-none}"

  COST_TOTAL=$(jq -n --argjson a "$COST_TOTAL" --argjson b "${PH_COST:-0}" '$a + $b')
  PHASES_JSON=$(jq -c --argjson p "$PHASES_JSON" \
    --arg name "$PHASE" --arg status "$PH_STATUS" --arg summary "$PH_SUMMARY" \
    --arg rc "$PH_RC" --argjson cost "${PH_COST:-0}" --argjson elapsed "$PH_ELAPSED" \
    --argjson commits "$PH_COMMITS" --argjson dirty "$PH_DIRTY" --arg tools "$PH_TOOLS" \
    -n '$p + [{name:$name, status:$status, rc:$rc, cost:$cost, elapsed:$elapsed,
              commits:$commits, dirty:$dirty, tools:$tools, summary:$summary}]')
}

# The unattended addendum every phase carries. In `-p` a request for input is a
# dead run rather than an error, and both review skills contain a literal
# "ask the user" step — so this is load-bearing, not politeness.
UNATTENDED='You are running unattended in a container: there is no user, and
nobody can answer a question. Never ask one. If you genuinely cannot proceed,
stop and report status "blocked" instead.'

# --------------------------------------------------------- phase 1: implement ---
run_phase implement "$PROMPT" "$(cat /opt/result.schema.json)"
[ "$PH_STATUS" = completed ] || IMPLEMENT_FAILED=1
IMPLEMENT_STATUS=$PH_STATUS

# ------------------------------------------------ phase 2: code-review + fix ---
# The fixed point is injected, because /code-review step 1 says "If they didn't
# specify one, ask for it" (#12's first unattended-hang trap). And the spec source
# is named with the fetch command, because step 2 resolves it through
# docs/agents/issue-tracker.md — which target repos are explicitly not required to
# carry. The `issues: read` half of the pod token is what makes that possible.
#
# The skill only *reports*: it aggregates two sub-agents and stops. Everything
# after the review header below is the fix half, and without it the phase costs a
# dollar and changes nothing — which is exactly what happened in every green run
# so far, where /implement's own trailing /code-review landed nowhere.
LANG_REVIEWER=''
if [ -n "${TOOLCHAIN:-}" ]; then
  LANG_REVIEWER="Alongside the two sub-agents the skill spawns, spawn one more
general-purpose sub-agent reviewing the same diff as a ${TOOLCHAIN} specialist —
idiom, standard-library fit, and error handling — reporting in the same format.
"
fi
REVIEW_PROMPT="/mattpocock-skills:code-review ${BASE_SHA}

The fixed point for this review is the commit ${BASE_SHA}. It is specified: do
not ask for it. The diff under review is \`git diff ${BASE_SHA}...HEAD\`.

The originating spec is GitHub issue #${ISSUE} in the repository ${REPO}. Fetch
it yourself with \`gh issue view ${ISSUE} --repo ${REPO} --json title,body,comments\`.
This repository carries no Matt Pocock scaffolding — there is no
\`docs/agents/issue-tracker.md\` and there will not be one. Do not run
/setup-matt-pocock-skills, and do not ask where the spec is.
${LANG_REVIEWER}
${UNATTENDED}

The skill only *reports*. When it has reported, act on it:

1. Fix the findings that are real defects or real violations of a standard this
   repository documents. Change the working tree.
2. A finding that would widen the change beyond what issue #${ISSUE} asked for is
   not a fix. Scope creep the review names should be **removed**, not expanded.
3. If you changed anything, run this repository's build and test commands.
4. Commit your work to the current branch. Do not amend or rewrite existing
   commits, and do not push.
5. If nothing was worth acting on, change nothing, commit nothing, and report
   fixed: 0."
run_phase review "$REVIEW_PROMPT" "$(cat /opt/review.schema.json)"
REVIEW_STATUS=$PH_STATUS

# -------------------------------------------- phase 3: ponytail-review + fix ---
# #12's deliberate pair: phase 2 widens the diff, phase 3 narrows it. The skill's
# own SKILL.md ends "Does not apply the fixes, only lists them", so the fix half
# is even more explicitly the prompt's job here than in phase 2.
PONYTAIL_PROMPT="/ponytail:ponytail-review

Review the diff of the current branch against ${BASE_SHA}:
\`git diff ${BASE_SHA}...HEAD\`. That is the whole scope — do not review code the
diff does not touch, and do not ask for a different fixed point.

${UNATTENDED}

The skill only lists what to cut. When it has listed, apply the cuts:

1. Make the diff shorter. That is the whole point of this phase.
2. Do not delete tests, input validation, or error handling. A single smoke test
   or assert-based self-check is the minimum, never bloat.
3. Run this repository's build and test commands afterwards.
4. Commit your work to the current branch. Do not amend or rewrite existing
   commits, and do not push.
5. If the review says \`Lean already. Ship.\`, change nothing, commit nothing,
   and report fixed: 0."
run_phase ponytail "$PONYTAIL_PROMPT" "$(cat /opt/review.schema.json)"
PONYTAIL_STATUS=$PH_STATUS

# The implement phase is the one that cannot be skipped: #12 says a dead *review*
# phase means push anyway with status completed_unreviewed, but nothing to push is
# nothing to push. Checked after all three so a dead phase 1 still measures 2 and 3.
if [ -n "${IMPLEMENT_FAILED:-}" ]; then
  probe "implement-failed" "status=${IMPLEMENT_STATUS} — continuing to measure phases 2/3"
fi

# #12: a dead or failed review phase does not discard the implement phase.
case "${REVIEW_STATUS}:${PONYTAIL_STATUS}" in
  completed:completed) AGENT_STATUS=$IMPLEMENT_STATUS ;;
  *) AGENT_STATUS=completed_unreviewed ;;
esac
[ "$IMPLEMENT_STATUS" = completed ] || AGENT_STATUS=$IMPLEMENT_STATUS

# A phase told to commit and leaving a dirty tree is work the push step would
# silently drop — the question #31 asks. Recorded per phase above; the leftovers
# are committed here rather than lost, because measuring the drop does not require
# suffering it. Deliberately generic: #12 gives the run plan no commit message.
if [ -n "$(git status --porcelain)" ]; then
  probe "dirty-at-push" "$(git status --porcelain | wc -l | tr -d ' ') paths — committing deterministically"
  git add -A && git commit -q -m "chore: uncommitted review-phase changes"
fi

# --------------------------------------------------------------------- push ---
PHASE=push
COMMITS=$(git rev-list --count "${BASE_SHA}..HEAD")
ELAPSED=$(( $(date +%s) - RUN_STARTED ))
probe "commits" "$COMMITS"
probe "run-total" "cost=\$${COST_TOTAL} elapsed=${ELAPSED}s"
if [ "$COMMITS" -eq 0 ]; then
  termlog "$AGENT_STATUS" "no commits from any phase (cost=\$${COST_TOTAL}, ${ELAPSED}s)"
  say "no commits — nothing to push"
  exit 1
fi

say "push ${BRANCH}"
git push -q origin "$BRANCH" || die "push failed"
probe "pushed" "${BRANCH} (+${COMMITS} commits)"

# ------------------------------------------------------------------- report ---
PHASE=done
termlog "$AGENT_STATUS" "branch=${BRANCH} commits=${COMMITS} cost=\$${COST_TOTAL} elapsed=${ELAPSED}s"
say "done — status=${AGENT_STATUS} branch=${BRANCH} cost=\$${COST_TOTAL} elapsed=${ELAPSED}s"
# The per-phase table, on stdout as well as in the blob — this is the measurement.
jq -r '.[] | "  \(.name)\t\(.status)\trc=\(.rc)\t\(.elapsed)s\t$\(.cost)\t+\(.commits) commits\tdirty=\(.dirty)"' \
  <<EOF >&2
$PHASES_JSON
EOF
