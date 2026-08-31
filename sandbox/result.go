// Package sandbox is the run plan's Go side: the type of the blob phase.sh writes,
// and (in phase_test.go) the test that runs the shipped script against a real git.
//
// It holds no code the sandbox itself runs — the run plan is shell, baked into the
// image — so nothing here is imported by anything the image contains.
package sandbox

// Result is the /dev/termination-log blob, which architecture §3 fixes and
// phase.sh writes exactly once at the end of a run.
//
// It earned a file of its own the moment it had a second reader: the
// orchestrator's Pod informer decodes exactly this off the container's terminated
// `message`. One definition, so the run plan and the thing that reports it cannot
// disagree about the shape.
//
// The kubelet caps the termination message at 4096 bytes and truncates it blind,
// so phase.sh bounds every unbounded field instead of letting that happen. Nothing
// is optional — a run that dies in preflight still writes the whole shape, with
// status "failed" and the phase in Message. **A pod killed by a signal writes it
// not at all**, which is the case the informer exists for and cannot be expressed
// here.
type Result struct {
	// Status is the run's verdict, not the process's: "completed", "blocked",
	// "failed", or "completed_unreviewed" when the implement phase landed and a
	// review phase died.
	Status  string `json:"status"`
	Branch  string `json:"branch"`
	Commits int    `json:"commits"`
	// CostUSD is summed across the phases, ElapsedS measured across the whole run.
	CostUSD  float64 `json:"cost_usd"`
	ElapsedS int     `json:"elapsed_s"`
	// PRTitle comes from the implement phase only — the phase that knows the
	// change's intent. The review schemas do not carry the field at all.
	PRTitle string `json:"pr_title"`
	// Message is the run plan's own last word: what it pushed, or why it did not.
	Message string  `json:"message"`
	Phases  []Phase `json:"phases"`
}

// Phase is one agent phase's status and summary line. A status other than
// "completed" is how the dead phase gets named.
type Phase struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Summary string `json:"summary"`
}
