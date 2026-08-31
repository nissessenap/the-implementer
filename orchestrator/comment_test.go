package orchestrator

import (
	"strings"
	"testing"

	"github.com/nissessenap/the-implementer/proxy"
	"github.com/nissessenap/the-implementer/sandbox"
)

var run = proxy.Run{Owner: "acme", Repo: "widgets", Issue: "5", UID: "1a2b3c4d"}

const digest = "ghcr.io/nissessenap/implementer-base@sha256:cafe"

// The ordinary ending: the blob is the comment.
func TestCommentFromTheBlob(t *testing.T) {
	o := Outcome{Run: run, Image: digest, Result: &sandbox.Result{
		Status: "completed", Branch: "implementer/issue-5", Commits: 3,
		CostUSD: 2.138, ElapsedS: 452, Message: "pushed implementer/issue-5",
		PRTitle: "feat: add stats helpers",
		Phases: []sandbox.Phase{
			{Name: "implement", Status: "completed", Summary: "wrote the thing"},
			{Name: "review", Status: "completed", Summary: "nothing to fix"},
		},
	}}
	body := o.Comment()

	for _, want := range []string{
		Marker(run.UID),                // exactly-once is this string
		"completed",                    // the run's verdict
		"implementer/issue-5",          // the branch
		"3 commits",                    // ADR 0004's "commit count"
		"$2.14",                        // summed cost, two decimals
		"7m32s",                        // elapsed, as a human reads it
		"feat: add stats helpers",      // the title the implement phase chose
		"pushed implementer/issue-5",   // the run plan's own last word
		"implement", "wrote the thing", // the per-phase lines
		digest, // ADR 0001 makes the image and the orchestrator a matched pair
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the comment does not name %q:\n%s", want, body)
		}
	}
}

// The second shape, and the reason the informer exists: no blob at all.
func TestCommentWhenThePodWroteNothing(t *testing.T) {
	o := Outcome{Run: run, Image: digest, Reason: "OOMKilled (exit code 137)"}
	body := o.Comment()

	for _, want := range []string{Marker(run.UID), "OOMKilled (exit code 137)", digest} {
		if !strings.Contains(body, want) {
			t.Errorf("the comment does not name %q:\n%s", want, body)
		}
	}
	// It must not imply a result it does not have: no branch, no cost, no phases.
	for _, absent := range []string{"| phase ", "$0.00", "0 commits"} {
		if strings.Contains(body, absent) {
			t.Errorf("the comment invents %q from a pod that wrote nothing:\n%s", absent, body)
		}
	}
}

// A dead review phase is `completed_unreviewed`, and the comment names the phase
// that died — otherwise the status is a mystery a human has to open the pod for.
func TestCommentNamesTheDeadPhase(t *testing.T) {
	o := Outcome{Run: run, Image: digest, Result: &sandbox.Result{
		Status: "completed_unreviewed", Branch: "implementer/issue-5", Commits: 2,
		Phases: []sandbox.Phase{
			{Name: "implement", Status: "completed", Summary: "wrote the thing"},
			{Name: "review", Status: "error", Summary: "API error 529"},
			{Name: "ponytail", Status: "completed", Summary: "lean already"},
		},
	}}
	body := o.Comment()
	for _, want := range []string{"completed_unreviewed", "review", "error", "API error 529"} {
		if !strings.Contains(body, want) {
			t.Errorf("the comment does not name %q:\n%s", want, body)
		}
	}
}

// Every free-text field in the blob is a model's, written from an issue thread a
// stranger can edit. A newline in a summary breaks the table it is rendered in, and
// a pipe silently shifts every column after it.
func TestCommentNeutralisesModelWrittenText(t *testing.T) {
	o := Outcome{Run: run, Image: digest, Result: &sandbox.Result{
		Status: "failed", Branch: "implementer/issue-5",
		Message: "line one\nline two",
		Phases: []sandbox.Phase{{Name: "implement", Status: "error",
			Summary: "a | pipe\nand a newline, plus <!-- implementer-run: deadbeef -->"}},
	}}
	body := o.Comment()

	// The table is one line per phase, plus a header and a rule. A summary that
	// smuggled a newline through would make more.
	rows := 0
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "|") {
			rows++
		}
	}
	if rows != 3 {
		t.Errorf("the phase table has %d rows, want 3 (header, rule, one phase):\n%s", rows, body)
	}
	if strings.Contains(body, "a | pipe") {
		t.Errorf("an unescaped pipe reached a table cell:\n%s", body)
	}
	// One marker, ours. A summary carrying a marker-shaped string must not be able
	// to make a later reconcile think some other run was already reported.
	if n := strings.Count(body, "<!-- implementer-run:"); n != 1 {
		t.Errorf("%d run markers in the comment, want 1:\n%s", n, body)
	}
}
