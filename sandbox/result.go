// Package sandbox is the sandbox image's half of the matched pair: the Dockerfile
// that is ADR 0001's BYO contract, the run plan that is the pod's command, and the
// shape of what that run plan reports back.
package sandbox

// Result is the /dev/termination-log blob, which architecture §3 fixes and
// phase.sh writes exactly once at the end of a run. It is an interface rather than
// a convenience: the orchestrator's PR builder reads it to write the pull request
// and the issue comment, and both outlive the pod.
//
// The kubelet caps the termination message at 4096 bytes and truncates it blind,
// so phase.sh bounds every unbounded field instead of letting that happen. Nothing
// here is optional — a run that dies in preflight still writes the whole shape,
// with status "failed" and the phase in Message.
type Result struct {
	// Status is the run's verdict, not the process's: "completed", "blocked",
	// "failed", or "completed_unreviewed" when the implement phase landed and a
	// review phase died. Nothing gates the pull request, so the last of those is a
	// pushed branch plus a comment naming the phase — which is Phases, below.
	Status string `json:"status"`

	Branch  string `json:"branch"`
	Commits int    `json:"commits"`

	// CostUSD is summed across the phases, ElapsedS measured across the whole run.
	CostUSD  float64 `json:"cost_usd"`
	ElapsedS int     `json:"elapsed_s"`

	// PRTitle comes from the implement phase only — the phase that knows the
	// change's intent. The review schemas do not carry the field at all, so this is
	// empty for a run whose implement phase never reported.
	PRTitle string `json:"pr_title"`

	// Message is the run plan's own last word: what it pushed, or why it did not.
	Message string `json:"message"`

	Phases []Phase `json:"phases"`
}

// Phase is one agent phase's status and summary line. A status other than
// "completed" is how the dead phase gets named.
type Phase struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Summary string `json:"summary"`
}
