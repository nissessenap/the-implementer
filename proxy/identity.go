package proxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// The annotations run identity lives in, on the Pod. ADR 0004 writes them in two
// places — the Job's own metadata and spec.template's — because a Job's metadata
// does not propagate to its pods, and it is the Pod copy the proxy reads.
//
// Exported so the orchestrator writes what this reads from one set of constants.
// That is the only direction the seam allows: the proxy imports nothing of theirs.
const (
	AnnOwner = "implementer.dev/owner"
	AnnRepo  = "implementer.dev/repo"
	AnnIssue = "implementer.dev/issue"

	// AnnRunUID makes the run secret per-*run* rather than per-issue, which is
	// the whole point of the second factor: a re-run of the same issue must not
	// inherit the previous run's credential.
	//
	// ADR 0005 says "job-UID", and that cannot be built: the apiserver assigns a
	// Job's UID at create time, spec.template is immutable afterwards, and the
	// sandbox cannot derive the secret itself (it holds no shared key) — so
	// nothing can ever put a real Job UID into the pod's environment. A value the
	// creator picks has the freshness property the UID was chosen for and can
	// actually be written. Amend the ADR, do not "fix" this back.
	AnnRunUID = "implementer.dev/run-uid"
)

// Run is one run's identity, as the orchestrator claims it in the proxy
// credential and as the proxy reads it back off the Pod. The two must agree.
type Run struct {
	Owner, Repo, Issue, UID string
}

// String is the identity as everything else in the system spells it, and as the
// mint scope reads: mint for this repository, never for the one the URL names.
func (r Run) String() string { return r.Owner + "/" + r.Repo + "#" + r.Issue }

// Cred derives the run's proxy credential from the one long-lived shared key both
// components mount. The orchestrator injects it as userinfo in the sandbox's
// https_proxy URL — every client derives Proxy-Authorization from that unaided —
// and the proxy recomputes it here rather than being told it, which is what keeps
// the "no per-run Secret, no orchestrator->proxy channel" property.
//
// The username is the claim itself, comma-separated: no field can contain a comma
// (GitHub allows only [A-Za-z0-9._-] in owners and repos), and commas are legal in
// URL userinfo where '/', '#' and '@' would need percent-encoding every client
// would have to agree on. The HMAC covers that same wire string, so there is one
// encoding rather than two that can drift.
func Cred(key []byte, r Run) (user, pass string) {
	user = strings.Join([]string{r.Owner, r.Repo, r.Issue, r.UID}, ",")
	m := hmac.New(sha256.New, key)
	m.Write([]byte(user))
	return user, hex.EncodeToString(m.Sum(nil))
}

// complete is the one definition of "this is a run identity", used by both ends
// of the comparison. Empty fields are refused rather than compared: "acme,,5,uid"
// must not match a Pod carrying no repo annotation.
func (r Run) complete() bool {
	return r.Owner != "" && r.Repo != "" && r.Issue != "" && r.UID != ""
}

// parseRun reads the claim back out of the userinfo username.
func parseRun(user string) (Run, bool) {
	f := strings.Split(user, ",")
	if len(f) != 4 {
		return Run{}, false
	}
	r := Run{Owner: f[0], Repo: f[1], Issue: f[2], UID: f[3]}
	return r, r.complete()
}
