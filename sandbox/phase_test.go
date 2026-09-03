package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The run plan, run for real, offline. `claude` and `gh` are stubs and `git` is
// not: the clone, the branch, the commits and the push all happen against a bare
// repository in a temp dir, reached by pointing git's own `insteadOf` at the URL
// phase.sh builds — so nothing about the script bends for the test.
//
// What this covers is the half of the acceptance criteria that does not need a
// cluster, a model credential or a dollar: the blob's shape and its escaping, the
// is_error rule, a dead review phase leaving the branch pushed, and the
// commits-and-clean assertion per review phase. The other half is a real run
// against a real repository, which is in CLAUDE.md and cannot live in `go test`.

// The sentinel stands where the GitHub credential would be. Here it only has to be
// the string phase.sh puts in the clone URL, which the rewrite below matches.
const sentinel = "test-sentinel"

// The issue title is deliberately hostile: a double quote, a single quote and a
// newline, which is the escaping the blob has to survive.
const issueTitle = "feat: add \"stats\" helpers\nand a 'second' line"

type stub struct {
	rc     int
	sideFX string         // sh, run in the repo, before the result is emitted
	result map[string]any // the stream's `result` message; nil emits none
}

func TestRunPlan(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stubs  []stub
		env    []string
		exit   int
		before func(t *testing.T, h harness)
		check  func(t *testing.T, r Result, h harness)
	}{{
		name: "three green phases push a branch and report the whole blob",
		stubs: []stub{
			{sideFX: commit("feat.go"), result: result(0.897, map[string]any{
				"status": "completed", "summary": "wrote the thing", "pr_title": issueTitle})},
			{sideFX: commit("fix.go"), result: result(0.811, map[string]any{
				"status": "completed", "summary": "7 findings, fixed 1", "findings": 7, "fixed": 1})},
			{sideFX: commit("trim.go"), result: result(0.322, map[string]any{
				"status": "completed", "summary": "cut 3 lines", "findings": 1, "fixed": 1})},
		},
		check: func(t *testing.T, r Result, h harness) {
			eq(t, "status", r.Status, "completed")
			eq(t, "branch", r.Branch, "implementer/issue-42")
			eq(t, "commits", r.Commits, 3)
			eq(t, "cost", fmt.Sprintf("%.3f", r.CostUSD), "2.030")
			// The wrap defaults off, and this is where that is asserted: rootlesskit
			// is on PATH for every case, so its absence here is the run plan's doing.
			if _, err := os.Stat(filepath.Join(h.stubs, "rootlesskit.args")); err == nil {
				t.Error("the run plan reached for rootlesskit without SANDBOX_DOCKER")
			}
			// The title round-trips byte for byte, quotes and newline included.
			eq(t, "pr_title", r.PRTitle, issueTitle)
			if r.ElapsedS < 0 {
				t.Errorf("elapsed_s = %d", r.ElapsedS)
			}
			eq(t, "phases", phaseLine(r), "implement=completed review=completed ponytail=completed")
			// The branch is on the remote with all three commits.
			eq(t, "pushed commits", h.remoteCommits(t), 3)

			// The findings from #31 that fail silently if dropped.
			implement, review, ponytail := h.args(t, 1), h.args(t, 2), h.args(t, 3)
			contains(t, "implement invokes the prefixed skill", implement, "/mattpocock-skills:implement")
			contains(t, "the brief carries the issue title", implement, issueTitle)
			contains(t, "review invokes the prefixed skill", review, "/mattpocock-skills:code-review")
			// The fixed point, injected: asked for one it does not have,
			// /code-review asks a human, which unattended is a silently
			// successful dead run.
			contains(t, "review carries BASE_SHA", review, h.baseSHA(t))
			contains(t, "ponytail invokes the prefixed skill", ponytail, "/ponytail:ponytail-review")
			contains(t, "ponytail gets both plugin dirs", ponytail, "--plugin-dir\x00"+h.opt+"/ponytail")
			if strings.Contains(implement, "/ponytail") {
				t.Error("the implement phase was handed the ponytail plugin dir")
			}
		},
	}, {
		// A transient 529 is the case this exists for: the review dies, the ~$1
		// implement phase is not discarded, and the orchestrator gets the dead
		// phase named. is_error is set *together with* a completed status, because
		// on error paths structured_output is absent and any status field is stale.
		name: "a review phase carrying is_error fails the phase and still pushes",
		stubs: []stub{
			{sideFX: commit("feat.go"), result: result(0.9, map[string]any{
				"status": "completed", "summary": "done", "pr_title": "feat: done"})},
			// It also got as far as changing a file, which must not be dropped on
			// the way past just because the phase that produced it never reported.
			{rc: 1, sideFX: "printf half-done > fix.go\n",
				result: map[string]any{"type": "result", "subtype": "error_during_execution",
					"is_error": true, "total_cost_usd": 0.05, "result": "API Error: 529 overloaded",
					"structured_output": map[string]any{"status": "completed", "summary": "lies"}}},
			{sideFX: commit("trim.go"), result: result(0.3, map[string]any{
				"status": "completed", "summary": "cut 2 lines", "findings": 1, "fixed": 1})},
		},
		check: func(t *testing.T, r Result, h harness) {
			eq(t, "status", r.Status, "completed_unreviewed")
			// `error` and not `dirty`: the leftover is committed either way, but the
			// status a human reads should say what actually went wrong.
			eq(t, "phases", phaseLine(r), "implement=completed review=error ponytail=completed")
			contains(t, "the dead phase says why", r.Phases[1].Summary, "529")
			eq(t, "commits", r.Commits, 3)
			eq(t, "pushed commits", h.remoteCommits(t), 3)
			eq(t, "the dead phase's work reached the remote", h.remoteHas(t, "fix.go"), true)
		},
	}, {
		name: "a review phase that leaves the tree dirty is caught, not trusted",
		stubs: []stub{
			{sideFX: commit("feat.go"), result: result(0.9, map[string]any{
				"status": "completed", "summary": "done", "pr_title": "feat: done"})},
			// Changes the tree and never commits — work the push step would
			// otherwise drop without saying so.
			{sideFX: "printf leftover > fix.go\n", result: result(0.8, map[string]any{
				"status": "completed", "summary": "fixed 1", "findings": 1, "fixed": 1})},
			{result: result(0.1, map[string]any{
				"status": "completed", "summary": "lean already", "findings": 0, "fixed": 0})},
		},
		check: func(t *testing.T, r Result, h harness) {
			eq(t, "status", r.Status, "completed_unreviewed")
			eq(t, "phases", phaseLine(r), "implement=completed review=dirty ponytail=completed")
			// Caught *and* not dropped: the leftover is on the remote.
			eq(t, "commits", r.Commits, 2)
			eq(t, "the leftover reached the remote", h.remoteHas(t, "fix.go"), true)
		},
	}, {
		name: "a review phase that reports fixes and commits none is caught",
		stubs: []stub{
			{sideFX: commit("feat.go"), result: result(0.9, map[string]any{
				"status": "completed", "summary": "done", "pr_title": "feat: done"})},
			{result: result(0.8, map[string]any{
				"status": "completed", "summary": "fixed 2", "findings": 4, "fixed": 2})},
			{result: result(0.1, map[string]any{
				"status": "completed", "summary": "lean already", "findings": 0, "fixed": 0})},
		},
		check: func(t *testing.T, r Result, h harness) {
			eq(t, "status", r.Status, "completed_unreviewed")
			eq(t, "phases", phaseLine(r), "implement=completed review=not_committed ponytail=completed")
		},
	}, {
		// The kubelet truncates the termination message blind at 4096 bytes, so an
		// agent's runaway summary must be cut by us or the PR builder is handed
		// invalid JSON.
		name: "runaway summaries stay inside the 4 KB cap and stay valid JSON",
		stubs: []stub{
			{sideFX: commit("feat.go"), result: result(0.9, map[string]any{
				"status": "completed", "summary": strings.Repeat("s", 5000),
				"pr_title": strings.Repeat("t", 5000)})},
			{result: result(0.8, map[string]any{
				"status": "completed", "summary": strings.Repeat("r", 5000), "findings": 0, "fixed": 0})},
			{result: result(0.1, map[string]any{
				"status": "completed", "summary": strings.Repeat("p", 5000), "findings": 0, "fixed": 0})},
		},
		check: func(t *testing.T, r Result, h harness) {
			// Parsed at all, which is the assertion the cap exists for.
			eq(t, "status", r.Status, "completed")
			if n := len(h.blob(t)); n > 4096 {
				t.Errorf("the blob is %d bytes, over the kubelet's cap", n)
			}
			eq(t, "pr_title length", len(r.PRTitle), 400)
			eq(t, "summary length", len(r.Phases[0].Summary), 400)
		},
	}, {
		// And the cap is in *bytes* while jq cuts codepoints, so the same lengths
		// in a script where a codepoint is three or four bytes are three or four
		// times the blob. An emoji in a model's summary is routine.
		name: "multibyte summaries stay inside the cap too",
		stubs: []stub{
			{sideFX: commit("feat.go"), result: result(0.9, map[string]any{
				"status": "completed", "summary": strings.Repeat("统计", 2500),
				"pr_title": strings.Repeat("🙂", 2500)})},
			{result: result(0.8, map[string]any{
				"status": "completed", "summary": strings.Repeat("检查", 2500), "findings": 0, "fixed": 0})},
			{result: result(0.1, map[string]any{
				"status": "completed", "summary": strings.Repeat("精简", 2500), "findings": 0, "fixed": 0})},
		},
		check: func(t *testing.T, r Result, h harness) {
			eq(t, "status", r.Status, "completed")
			if n := len(h.blob(t)); n > 4096 {
				t.Errorf("the blob is %d bytes, over the kubelet's cap", n)
			}
			// Re-cut hard rather than handed over to be truncated mid-string.
			if got := len([]rune(r.Phases[0].Summary)); got != 80 {
				t.Errorf("summary is %d codepoints, want the hard cut at 80", got)
			}
		},
	}, {
		// A phase status is richer than a run status. `dirty` belongs in `phases`,
		// where a human reads it; the run itself landed its work and is completed.
		name: "an implement phase that left the tree dirty is still a completed run",
		stubs: []stub{
			{sideFX: "printf uncommitted > feat.go\n", result: result(0.9, map[string]any{
				"status": "completed", "summary": "wrote it, forgot to commit", "pr_title": "feat: done"})},
			{result: result(0.8, map[string]any{
				"status": "completed", "summary": "nothing to fix", "findings": 0, "fixed": 0})},
			{result: result(0.1, map[string]any{
				"status": "completed", "summary": "lean already", "findings": 0, "fixed": 0})},
		},
		check: func(t *testing.T, r Result, h harness) {
			eq(t, "status", r.Status, "completed")
			eq(t, "phases", phaseLine(r), "implement=dirty review=completed ponytail=completed")
			eq(t, "the forgotten work reached the remote", h.remoteHas(t, "feat.go"), true)
		},
	}, {
		// Every phase says `completed` and nothing was committed: the blob must not
		// name a branch that exists nowhere and call the run completed.
		name: "three completed phases that committed nothing are not a completed run",
		stubs: []stub{
			{result: result(0.9, map[string]any{
				"status": "completed", "summary": "nothing needed changing", "pr_title": "chore: noop"})},
			{result: result(0.8, map[string]any{
				"status": "completed", "summary": "nothing to fix", "findings": 0, "fixed": 0})},
			{result: result(0.1, map[string]any{
				"status": "completed", "summary": "lean already", "findings": 0, "fixed": 0})},
		},
		exit: 1,
		check: func(t *testing.T, r Result, h harness) {
			eq(t, "status", r.Status, "failed")
			eq(t, "nothing pushed", h.remoteHasBranch(t), false)
		},
	}, {
		name: "a blocked implement phase pushes nothing and says so",
		stubs: []stub{
			{result: result(0.2, map[string]any{
				"status": "blocked", "summary": "the brief contradicts itself", "pr_title": ""})},
			{result: result(0.1, map[string]any{
				"status": "completed", "summary": "nothing to review", "findings": 0, "fixed": 0})},
			{result: result(0.1, map[string]any{
				"status": "completed", "summary": "nothing to cut", "findings": 0, "fixed": 0})},
		},
		exit: 1,
		check: func(t *testing.T, r Result, h harness) {
			eq(t, "status", r.Status, "blocked")
			eq(t, "commits", r.Commits, 0)
			contains(t, "message", r.Message, "no commits")
			eq(t, "nothing pushed", h.remoteHasBranch(t), false)
		},
	}, {
		// The branch is derived from the issue, so a re-run builds the same name from
		// a fresh clone and cannot fast-forward what the first run pushed. Refused
		// before a phase runs, because the alternative is finding out at the push,
		// after paying for all three.
		name: "a branch a previous run already pushed is refused before anything is spent",
		before: func(t *testing.T, h harness) {
			h.git(t, h.remote, "branch", "implementer/issue-42", "main")
		},
		stubs: []stub{{result: result(0.9, map[string]any{
			"status": "completed", "summary": "should never run", "pr_title": "x"})}},
		exit: 1,
		check: func(t *testing.T, r Result, h harness) {
			eq(t, "status", r.Status, "failed")
			contains(t, "message names the branch", r.Message, "implementer/issue-42 already exists")
			eq(t, "cost", r.CostUSD, 0.0)
			if _, err := os.Stat(filepath.Join(h.stubs, "n")); err == nil {
				t.Error("a phase ran despite the branch already existing")
			}
		},
	}, {
		// The failed-implement path, which no other case reaches: the commits are
		// pushed anyway, because they cost the same money a completed phase's do
		// and `status` is what tells the PR builder not to open a pull request —
		// but the *process* exits 1, because the Job condition is the
		// orchestrator's other result channel and the two must not disagree.
		name: "a failed implement phase pushes its work and still fails the Job",
		stubs: []stub{
			{rc: 1, sideFX: commit("half.go"),
				result: map[string]any{"type": "result", "subtype": "error_during_execution",
					"is_error": true, "total_cost_usd": 1.5, "result": "API Error: 529 overloaded"}},
			{result: result(0.1, map[string]any{
				"status": "completed", "summary": "nothing to review", "findings": 0, "fixed": 0})},
			{result: result(0.1, map[string]any{
				"status": "completed", "summary": "nothing to cut", "findings": 0, "fixed": 0})},
		},
		exit: 1,
		check: func(t *testing.T, r Result, h harness) {
			eq(t, "status", r.Status, "failed")
			eq(t, "phases", phaseLine(r), "implement=error review=completed ponytail=completed")
			eq(t, "commits", r.Commits, 1)
			eq(t, "the work reached the remote", h.remoteHas(t, "half.go"), true)
			// No structured_output on an error path, so there is no title to carry.
			eq(t, "pr_title", r.PRTitle, "")
			eq(t, "cost", fmt.Sprintf("%.1f", r.CostUSD), "1.7")
		},
	}, {
		// A phase whose process died before emitting a result at all: its own exit
		// status is the only evidence, and whatever it spent is unknowable rather
		// than zero-with-a-`($)`-log-line.
		name: "an implement phase that emitted no result is a failed run",
		stubs: []stub{
			{rc: 137},
			{result: result(0.1, map[string]any{
				"status": "completed", "summary": "nothing to review", "findings": 0, "fixed": 0})},
			{result: result(0.1, map[string]any{
				"status": "completed", "summary": "nothing to cut", "findings": 0, "fixed": 0})},
		},
		exit: 1,
		check: func(t *testing.T, r Result, h harness) {
			eq(t, "status", r.Status, "failed")
			eq(t, "phases", phaseLine(r), "implement=no_result review=completed ponytail=completed")
			contains(t, "the dead phase carries its rc", r.Phases[0].Summary, "rc=137")
			eq(t, "cost", fmt.Sprintf("%.1f", r.CostUSD), "0.2")
			eq(t, "nothing pushed", h.remoteHasBranch(t), false)
		},
	}, {
		// The preflight, which nothing else exercises — and it is the one place
		// where the diagnostic has to survive report() failing, because a missing
		// tool may be the very tool report() is built out of.
		name: "a preflight violation writes the blob and spends nothing",
		before: func(t *testing.T, h harness) {
			if err := os.Remove(filepath.Join(h.opt, "ponytail/skills/ponytail-review/SKILL.md")); err != nil {
				t.Fatal(err)
			}
		},
		exit: 1,
		check: func(t *testing.T, r Result, h harness) {
			eq(t, "status", r.Status, "failed")
			contains(t, "message names the file", r.Message, "ponytail-review/SKILL.md is missing")
			contains(t, "message names the phase", r.Message, "preflight")
			eq(t, "cost", r.CostUSD, 0.0)
			eq(t, "phases", phaseLine(r), "")
			if _, err := os.Stat(filepath.Join(h.stubs, "n")); err == nil {
				t.Error("a phase ran despite the preflight failing")
			}
		},
	}, {
		// ADR 0001's per-run flag, on. The whole script is wrapped and not just
		// the daemon: that is what puts the agent, dockerd and inner containers in
		// one network namespace, and it is why this re-enters the run plan rather
		// than backgrounding something.
		name: "SANDBOX_DOCKER=1 wraps the whole run plan and waits for a usable daemon",
		env:  []string{"SANDBOX_DOCKER=1"},
		stubs: []stub{
			{sideFX: commit("feat.go"), result: result(0.5, map[string]any{
				"status": "completed", "summary": "wrote the thing", "pr_title": "feat: a thing"})},
			{result: result(0.1, map[string]any{
				"status": "completed", "summary": "nothing to review", "findings": 0, "fixed": 0})},
			{result: result(0.1, map[string]any{
				"status": "completed", "summary": "nothing to cut", "findings": 0, "fixed": 0})},
		},
		check: func(t *testing.T, r Result, h harness) {
			// The run still completes: the wrap is transparent to everything after it.
			eq(t, "status", r.Status, "completed")
			eq(t, "commits", r.Commits, 1)

			args := h.stubArgs(t, "rootlesskit.args")
			// --net=host is not a preference: it panics dockerd on the first
			// `docker version`, which is the call the gate below makes.
			contains(t, "rootlesskit net", args, "--net=slirp4netns")
			contains(t, "rootlesskit copies /etc up", args, "--copy-up=/etc")
			contains(t, "the wrap re-enters the run plan", args, "IN_ROOTLESSKIT=1")

			d := h.stubArgs(t, "dockerd.args")
			// gVisor cannot serve dockerd's iptables setup, creating a bridge is
			// EPERM in an unprivileged user namespace, and `docker build` needs the
			// containerd snapshotter off. None of the three is a preference.
			for _, f := range []string{"--iptables=false", "--ip6tables=false",
				"--bridge=none", "containerd-snapshotter=false"} {
				contains(t, "dockerd flags", d, f)
			}

			// The gate polled until the daemon answered rather than taking the
			// first no for an answer — the stub says no twice — and the fourth call
			// is the log line naming the version it got.
			n, err := os.ReadFile(filepath.Join(h.stubs, "dockerv"))
			if err != nil {
				t.Fatal(err)
			}
			eq(t, "docker version calls", string(n), "4\n")
		},
	}, {
		// The flag is the run plan's and the stack is the go image's, so asking a
		// node or python image for Docker has to fail *here*, named, before
		// anything is spent — not as an `docker: not found` inside a phase that
		// has already cost a dollar.
		name: "SANDBOX_DOCKER=1 in an image with no rootlesskit fails before anything is spent",
		env:  []string{"SANDBOX_DOCKER=1"},
		before: func(t *testing.T, h harness) {
			if err := os.Remove(filepath.Join(h.stubs, "rootlesskit")); err != nil {
				t.Fatal(err)
			}
		},
		exit: 1,
		check: func(t *testing.T, r Result, h harness) {
			eq(t, "status", r.Status, "failed")
			contains(t, "message names the missing piece", r.Message, "rootlesskit")
			eq(t, "cost", r.CostUSD, 0.0)
			if _, err := os.Stat(filepath.Join(h.stubs, "n")); err == nil {
				t.Error("a phase ran despite the wrap being impossible")
			}
		},
	}, {
		// The ending the whole per-run flag is about: a pod that was handed
		// SANDBOX_DOCKER=1 without the securityContext the wrap needs. rootlesskit
		// dies before it can hand over, and the run plan is the only thing left
		// that can say why — which is why the wrap runs as a child rather than
		// through `exec`, and why the blob still exists here at all.
		name: "a rootlesskit that will not start still leaves a result blob naming the cause",
		env:  []string{"SANDBOX_DOCKER=1"},
		before: func(t *testing.T, h harness) {
			write(t, filepath.Join(h.stubs, "rootlesskit"), `#!/bin/sh
echo "newuidmap: write to uid_map failed: Operation not permitted" >&2
exit 1
`)
		},
		exit: 1,
		check: func(t *testing.T, r Result, h harness) {
			eq(t, "status", r.Status, "failed")
			contains(t, "message names the wrap", r.Message, "rootlesskit wrap did not start")
			// The message has to carry the fix, because the cause is a PodSpec
			// field and the symptom is a message about uid maps.
			contains(t, "message names the fix", r.Message, "allowPrivilegeEscalation")
			eq(t, "cost", r.CostUSD, 0.0)
			if _, err := os.Stat(filepath.Join(h.stubs, "n")); err == nil {
				t.Error("a phase ran despite the wrap never starting")
			}
		},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			h := setup(t, tc.stubs)
			if tc.before != nil {
				tc.before(t, h)
			}
			tc.check(t, h.run(t, tc.exit, tc.env...), h)
		})
	}
}

// --- the harness ------------------------------------------------------------

type harness struct {
	dir, opt, remote, termLog, stubs string
}

func setup(t *testing.T, stubs []stub) harness {
	t.Helper()
	// Fatal rather than Skip: `go test ./...` without -v prints `ok` for a skipped
	// package, so a runner image that loses one of these makes the whole run plan
	// silently untested. All three are on every CI image and every dev box.
	for _, tool := range []string{"git", "jq", "sh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required to run the run plan", tool)
		}
	}
	dir := t.TempDir()
	h := harness{
		dir:     dir,
		opt:     filepath.Join(dir, "opt"),
		remote:  filepath.Join(dir, "origin.git"),
		termLog: filepath.Join(dir, "termination-log"),
		stubs:   filepath.Join(dir, "stubs"),
	}

	// $OPT stands in for the image's read-only /opt: the three SKILL.md files
	// preflight asserts, plus the two schemas the phases are handed. The schemas
	// are the real ones — they are in this directory.
	for _, skill := range []string{
		"skills/skills/engineering/implement", "skills/skills/engineering/code-review",
		"ponytail/skills/ponytail-review",
	} {
		write(t, filepath.Join(h.opt, skill, "SKILL.md"), "stub\n")
	}
	for _, schema := range []string{"result.schema.json", "review.schema.json"} {
		b, err := os.ReadFile(schema)
		if err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(h.opt, schema), string(b))
	}

	// The stubs. `claude` counts its own invocations, so each phase gets its own
	// canned stream, its own exit status and its own side effects on the repo —
	// and records the prompt it was handed, which is where the injected BASE_SHA
	// and the prefixed skill names are asserted.
	write(t, filepath.Join(h.stubs, "claude"), `#!/bin/sh
n=$(( $(cat "$STUBS/n" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$STUBS/n"
printf '%s\0' "$@" > "$STUBS/$n.args"
[ -f "$STUBS/$n.sh" ] && sh "$STUBS/$n.sh"
cat "$STUBS/$n.jsonl"
exit "$(cat "$STUBS/$n.rc")"
`)
	write(t, filepath.Join(h.stubs, "gh"), "#!/bin/sh\ncat \"$STUBS/issue.json\"\n")
	// bubblewrap is a contract requirement the run plan only checks for; nothing
	// here shells out to it.
	write(t, filepath.Join(h.stubs, "bwrap"), "#!/bin/sh\nexit 0\n")

	// The rootless Docker stack, stubbed. On PATH for every case, so "the wrap is
	// off by default" is an assertion about the run plan rather than about what
	// the harness happened to install — the go image really does carry all three.
	//
	// rootlesskit records that it ran and its own flags, drops them, and execs the
	// rest: what it is handed is `env IN_ROOTLESSKIT=1 /bin/sh <the run plan>`, so
	// the re-entry is the real one and the second pass is a real second pass.
	write(t, filepath.Join(h.stubs, "rootlesskit"), `#!/bin/sh
printf '%s\0' "$@" > "$STUBS/rootlesskit.args"
while [ "${1#--}" != "$1" ]; do shift; done
exec "$@"
`)
	write(t, filepath.Join(h.stubs, "dockerd"), `#!/bin/sh
printf '%s\0' "$@" > "$STUBS/dockerd.args"
`)
	// `docker version` fails twice before it answers, because the readiness gate
	// is the point: `docker info` answers about six seconds before the daemon is
	// usable, so a gate that takes the first answer is a race the run loses later.
	write(t, filepath.Join(h.stubs, "docker"), `#!/bin/sh
[ "$1" = version ] || exit 0
n=$(( $(cat "$STUBS/dockerv" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$STUBS/dockerv"
[ "$n" -ge 3 ] || exit 1
[ "$2" = --format ] && echo 29.7.2
exit 0
`)

	issue, err := json.Marshal(map[string]any{
		"title": issueTitle, "body": "cover count, sum, min, max.\nEasy to extend.",
		"comments": []any{map[string]any{
			"author": map[string]any{"login": "someone"}, "body": "and a \"comment\""}},
	})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(h.stubs, "issue.json"), string(issue))

	for i, s := range stubs {
		n := filepath.Join(h.stubs, fmt.Sprint(i+1))
		stream := `{"type":"system","subtype":"init","session_id":"stub"}` + "\n"
		if s.result != nil {
			b, err := json.Marshal(s.result)
			if err != nil {
				t.Fatal(err)
			}
			stream += string(b) + "\n"
		}
		write(t, n+".jsonl", stream)
		write(t, n+".rc", fmt.Sprint(s.rc))
		if s.sideFX != "" {
			write(t, n+".sh", s.sideFX)
		}
	}

	// The repository the run clones, and the bare remote it pushes to.
	seed := filepath.Join(dir, "seed")
	write(t, filepath.Join(seed, "README.md"), "# seed\n")
	h.git(t, seed, "init", "-q", "-b", "main")
	h.git(t, seed, "add", "-A")
	h.git(t, seed, "commit", "-q", "-m", "seed")
	h.git(t, dir, "clone", "-q", "--bare", seed, h.remote)
	return h
}

// run executes the shipped run plan and returns the blob it wrote.
func (h harness) run(t *testing.T, wantExit int, extra ...string) Result {
	t.Helper()
	ws := filepath.Join(h.dir, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "phase.sh")
	cmd.Env = append(h.env(),
		"REPO=owner/repo", "ISSUE=42", "GH_TOKEN="+sentinel,
		"WORKSPACE="+ws, "TERM_LOG="+h.termLog, "OPT="+h.opt,
		"STUBS="+h.stubs, "PATH="+h.stubs+":"+os.Getenv("PATH"))
	cmd.Env = append(cmd.Env, extra...)
	out, err := cmd.CombinedOutput()
	exit := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		exit = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running phase.sh: %v\n%s", err, out)
	}
	if exit != wantExit {
		t.Fatalf("phase.sh exited %d, want %d\n%s", exit, wantExit, out)
	}
	var r Result
	if err := json.Unmarshal(h.blob(t), &r); err != nil {
		t.Fatalf("the blob is not the contract: %v\n%s\n--- run output:\n%s", err, h.blob(t), out)
	}
	return r
}

// env is the run's environment minus the machine's: git must not read the
// developer's own config, and the clone URL phase.sh builds is rewritten at
// git's own `insteadOf` rather than made a knob in the script.
func (h harness) env() []string {
	url := fmt.Sprintf("https://x-access-token:%s@github.com/owner/repo.git", sentinel)
	return []string{
		"HOME=" + h.dir, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=the-implementer", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=the-implementer", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=url.file://" + h.remote + ".insteadOf",
		"GIT_CONFIG_VALUE_0=" + url,
	}
}

func (h harness) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir, cmd.Env = dir, h.env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (h harness) blob(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(h.termLog)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// stubArgs is the NUL-joined argv a stub recorded, rendered readable.
func (h harness) stubArgs(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.stubs, name))
	if err != nil {
		t.Fatalf("%s recorded nothing: %v", name, err)
	}
	return strings.ReplaceAll(string(b), "\x00", " ")
}

// args is the prompt and flags the nth `claude` invocation was handed, NUL-joined.
func (h harness) args(t *testing.T, n int) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(h.stubs, fmt.Sprint(n)+".args"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (h harness) baseSHA(t *testing.T) string {
	return h.git(t, h.remote, "rev-parse", "main")
}

func (h harness) remoteHasBranch(t *testing.T) bool {
	t.Helper()
	return h.git(t, h.remote, "for-each-ref", "--format=%(refname:short)", "refs/heads/implementer") != ""
}

func (h harness) remoteCommits(t *testing.T) int {
	t.Helper()
	var n int
	if _, err := fmt.Sscan(h.git(t, h.remote, "rev-list", "--count", "main..implementer/issue-42"), &n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (h harness) remoteHas(t *testing.T, path string) bool {
	t.Helper()
	return strings.Contains(h.git(t, h.remote, "ls-tree", "--name-only", "implementer/issue-42"), path)
}

// --- fixtures and assertions -------------------------------------------------

func result(cost float64, structured map[string]any) map[string]any {
	return map[string]any{"type": "result", "subtype": "success", "is_error": false,
		"total_cost_usd": cost, "structured_output": structured}
}

func commit(file string) string {
	return fmt.Sprintf("printf changed > %s\ngit add -A\ngit commit -q -m 'phase work'\n", file)
}

func phaseLine(r Result) string {
	var parts []string
	for _, p := range r.Phases {
		parts = append(parts, p.Name+"="+p.Status)
	}
	return strings.Join(parts, " ")
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func contains(t *testing.T, what, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("%s: %q is not in %q", what, needle, haystack)
	}
}
