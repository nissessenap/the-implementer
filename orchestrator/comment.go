package orchestrator

import (
	"fmt"
	"strings"
	"time"

	"github.com/nissessenap/the-implementer/proxy"
	"github.com/nissessenap/the-implementer/sandbox"
)

// Outcome is one finished run, as the informer read it off the Pod — and off the
// Job when the Pod is already gone.
//
// The two shapes are Result and Reason, and exactly one of them is set. The second
// is the whole reason this component exists — see Reporter in informer.go, which is
// where that argument is written down, and CONTEXT.md's "Silent death".
type Outcome struct {
	// Run is the identity off the annotations the Job builder writes in two places.
	Run proxy.Run

	// Image is the resolved digest — `containerStatuses[].imageID`. ADR 0001 makes
	// the image and the orchestrator a matched pair, and this is how a confusing
	// run gets diagnosed, so it is in the comment either way. Empty when the image
	// never pulled, which is itself the answer.
	Image string

	// Result is the blob the run plan wrote, or nil when the pod wrote nothing.
	Result *sandbox.Result

	// Reason is the Kubernetes-level ending, set only when Result is nil: all the
	// information that exists about a run nobody has any other way to learn about.
	Reason string
}

// Marker is the exactly-once record, and it lives in the comment because there is
// nowhere else for it to live: ADR 0004 has no database, and the orchestrator's
// RBAC is read-only on Pods and Jobs so it cannot annotate the run either. State
// lives in Kubernetes objects and GitHub — this is the GitHub half.
//
// Keyed on the run uid rather than the issue: a re-run of the same issue is a new
// run and gets its own comment, which is the same reason the credential is per-run.
func Marker(runUID string) string { return "<!-- implementer-run: " + runUID + " -->" }

// Comment is the one issue comment a run ends with. Built here, as a pure function
// of the outcome, so what the informer decides and what it says are separately
// testable — and so the body can be read in a test without a GitHub at all.
func (o Outcome) Comment() string {
	var b strings.Builder
	// First line, and machine-read: Marker() is what a later reconcile scans the
	// thread for, so it must not be behind anything that could be truncated.
	fmt.Fprintf(&b, "%s\n", Marker(o.Run.UID))

	if o.Result == nil {
		// The silent death, said plainly. Nothing here is invented: no branch, no
		// cost, no phases, because none of that was ever written.
		fmt.Fprintf(&b, "### the run ended without writing a result\n\n**%s**\n\n", cell(o.Reason))
		b.WriteString("Nothing in the pod ran to report this — no trap fired and no phase " +
			"script executed — so the line above is all the information that exists. The " +
			"transcript, if the pod is still there, is in `kubectl logs`.\n")
		o.footer(&b)
		return b.String()
	}

	r := o.Result
	fmt.Fprintf(&b, "### %s\n\n", cell(r.Status))
	if r.PRTitle != "" {
		fmt.Fprintf(&b, "**%s**\n\n", cell(r.PRTitle))
	}

	// The facts line: branch, commit count, summed cost, elapsed. Elapsed through
	// time.Duration so it reads as 7m32s rather than 452.
	facts := []string{fmt.Sprintf("`%s`", cell(r.Branch)), fmt.Sprintf("%d commits", r.Commits),
		fmt.Sprintf("$%.2f", r.CostUSD)}
	if r.ElapsedS > 0 {
		facts = append(facts, (time.Duration(r.ElapsedS) * time.Second).String())
	}
	fmt.Fprintf(&b, "%s\n\n", strings.Join(facts, " · "))

	if r.Message != "" {
		fmt.Fprintf(&b, "%s\n\n", cell(r.Message))
	}
	if len(r.Phases) > 0 {
		// A table, because the per-phase status is the field a human actually reads:
		// `completed_unreviewed` says a review phase died and this says which.
		b.WriteString("| phase | status | summary |\n| --- | --- | --- |\n")
		for _, p := range r.Phases {
			fmt.Fprintf(&b, "| %s | %s | %s |\n", cell(p.Name), cell(p.Status), cell(p.Summary))
		}
	}
	o.footer(&b)
	return b.String()
}

func (o Outcome) footer(b *strings.Builder) {
	img := o.Image
	if img == "" {
		img = "never resolved — the image did not pull"
	}
	fmt.Fprintf(b, "\n<sub>run `%s` · image `%s`</sub>\n", cell(o.Run.UID), cell(img))
}

// cell makes one field of the blob safe to put in the comment. Every free-text
// field in it is a model's, written from an issue thread a stranger can edit, so
// this is a trust boundary and not tidying: a newline in a summary ends the table
// it is rendered in, and a pipe silently shifts every column after it.
//
// The marker is neutralised for the same reason — a summary that could contain a
// comment marker could make a later reconcile believe some other run was already
// reported, and a run reported by a string in somebody else's summary is a run
// nobody hears about.
func cell(s string) string {
	s = strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ", "|", "\\|", "<!--", "<! --").Replace(s)
	return strings.TrimSpace(s)
}
