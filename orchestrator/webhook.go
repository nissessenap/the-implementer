package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/google/go-github/v88/github"

	"github.com/nissessenap/the-implementer/proxy"
)

// readyLabel is the label that means "run this". It is the one `/triage` already
// produces (docs/agents/triage-labels.md), so the readiness contract is inherited
// rather than reinvented — a second vocabulary would be a second thing to keep in
// step with the tracker.
//
// ponytail: a constant and not a knob. #73 defers configurability to "per
// installation later", and an installation is a GitHub App installation — so the
// thing that eventually reads this is a per-installation lookup, not a value on a
// Deployment. A single env var would be neither that nor this, and shipping one
// first would be a knob to keep working while the real mechanism replaced it.
const readyLabel = "ready-for-agent"

// Webhook is ADR 0004's front-end half: an `issues` webhook arrives, one Job is
// created, and the handler is done. It does not wait for the run, hand it to a
// worker, or hold anything about it — the HTTP response is sent minutes before the
// pull request exists.
//
// **It holds no GitHub client, and that is a property rather than an omission.**
// Two acceptance criteria are structural because of it: there is no call to the
// collaborator-permission endpoint anywhere in this path, and the refusal below is
// silent because there is nothing here that *could* write to GitHub. Adding a
// client to report a refusal is the vulnerability, not the fix — see refuse().
type Webhook struct {
	// Secret is the webhook secret, and an empty one is refused at every request
	// rather than tolerated: go-github validates nothing when the secret is empty
	// *and* no signature arrives, which turns this endpoint into an open trigger
	// for anyone who can reach the Service. Fail-closed, because the failure is
	// otherwise invisible — every legitimate delivery still works.
	Secret []byte

	// Start creates the run. The Run it is handed has **no UID**: picking one is
	// what makes a re-run a new run rather than an inheritor of the last one's
	// credential, and that decision belongs with whoever builds the Job — the same
	// place the command line makes it.
	//
	// Called synchronously, and the create is the only thing waited on: it is one
	// apiserver round-trip, and its error is worth a 500 GitHub will redeliver.
	// A goroutine here would trade that for a dropped run nobody is told about,
	// which is the failure mode this whole system is built against.
	Start func(context.Context, proxy.Run) error
}

func (h *Webhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if len(h.Secret) == 0 {
		http.Error(w, "no webhook secret configured", http.StatusInternalServerError)
		log.Print("webhook: refusing every delivery: no webhook secret is configured")
		return
	}
	// The signature, over the raw body, in constant time — go-github's, because
	// the one already in this binary is the one least likely to be subtly wrong.
	body, err := github.ValidatePayload(r, h.Secret)
	if err != nil {
		// 401 and a fixed string: what failed is a secret comparison, and the
		// caller is not owed which half of it.
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		log.Printf("webhook: rejected delivery %s: %v", github.DeliveryID(r), err)
		return
	}

	// Every other event type GitHub is configured to send, including the `ping`
	// that arrives the moment a webhook is created. 200, or the App's delivery
	// page is red for events we deliberately do not act on.
	if t := github.WebHookType(r); t != "issues" {
		ignore(w, "event %s", t)
		return
	}

	var e github.IssuesEvent
	if err := json.Unmarshal(body, &e); err != nil {
		http.Error(w, "malformed payload", http.StatusBadRequest)
		return
	}

	if e.GetAction() != "labeled" {
		ignore(w, "action %s", e.GetAction())
		return
	}
	// The payload trap. `label` is **not** in this payload's required set — that is
	// [action, issue, repository, sender], against the published schema — so a
	// `labeled` delivery can arrive with no label object at all and this must
	// ignore it rather than dereference it. `GetLabel().GetName()` would answer ""
	// here, which is a coincidence and not a check: it happens to be safe today
	// and says nothing about the field being optional.
	if e.Label == nil {
		ignore(w, "labeled with no label object")
		return
	}
	if e.Label.GetName() != readyLabel {
		ignore(w, "label %q", e.Label.GetName())
		return
	}

	// Authorization, and it is these two clauses rather than a permission API call.
	//
	// The flatt.tech disclosure is usually cited as proof a write-access check is
	// mandatory; it says the opposite. `claude-code-action` **had** one and was
	// bypassed anyway, because it opened by returning true for any actor whose
	// login ended in `[bot]` — and the attack needs no access to the target
	// repository at all: create a GitHub App, install it on your *own* repository,
	// use its installation token against the target. The fix was to assert the
	// actor's type is `User`. So the clause usually omitted is the one that was
	// exploited, and the clause usually insisted on is the one that failed to help.
	//
	// `ghost` is here beside it because GitHub substitutes that account for
	// unresolvable actors and **its type is `User`**, so the type assertion alone
	// lets it through. Folded, because logins are matched case-insensitively.
	//
	// No write check, deliberately: applying a label needs Triage, not write, so
	// the event proves triage and nothing more — and triage is trust enough here.
	// The escalation ceiling is a branch plus a pull request **no component in this
	// system can merge**, and the residual cost is agent budget and branch noise,
	// spent by someone a maintainer deliberately granted triage. It also deletes an
	// unverifiable dependency: which fine-grained permission the
	// collaborator-permission endpoint requires could not be established, because
	// GitHub's public OpenAPI encodes no fine-grained permissions for it.
	//
	// What this does *not* cover is the edit-after-label window — authorization is
	// here, at webhook time, while the pod fetches the issue text at run time, so
	// the text is mutable in between by someone never authorized. That is #32, it
	// survives both clauses because neither looks at the text, and it is
	// deliberately not fixed here.
	sender := e.GetSender()
	if sender.GetType() != "User" || strings.EqualFold(sender.GetLogin(), "ghost") {
		refuse(w, &e)
		return
	}

	// Through ParseIssue rather than field by field, for its alphabet check: the
	// run credential's claim is comma-joined, so a comma in an owner or a repo
	// makes the proxy see five fields and answer 407 for the rest of the run's
	// deadline. GitHub cannot produce one, which is exactly why a payload carrying
	// one is worth refusing here rather than debugging there.
	run, err := ParseIssue(fmt.Sprintf("%s/%s#%d",
		e.GetRepo().GetOwner().GetLogin(), e.GetRepo().GetName(), e.GetIssue().GetNumber()))
	if err != nil {
		http.Error(w, "unusable repository or issue", http.StatusBadRequest)
		log.Printf("webhook: delivery %s: %v", github.DeliveryID(r), err)
		return
	}

	// Idempotency is entirely the Job name plus a swallowed AlreadyExists (ADR
	// 0004): redelivery, a second label and a restart mid-run all resolve to that
	// one mechanism. Nothing here adds dedupe state, and nothing here may defeat it
	// — which is why the delivery id is not part of anything.
	if err := h.Start(r.Context(), run); err != nil {
		http.Error(w, "could not start the run", http.StatusInternalServerError)
		log.Printf("webhook: starting %s: %v", run, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, "started %s\n", run)
}

// ignore is the ordinary "not for us": 200, because a non-2xx marks the delivery
// failed on the App's own page and invites GitHub to redeliver an event we will
// ignore again.
func ignore(w http.ResponseWriter, format string, args ...any) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "ignored: "+format+"\n", args...)
}

// refuse is the authorization refusal, and **the silence is the security
// property**. It logs, answers GitHub, and does nothing else.
//
// There is deliberately no "sorry, you're not allowed" comment. On a public
// repository that would hand an unauthorized actor an on-demand way to make the App
// write to issues — precisely the `issues: write` plus untrusted-input combination
// the disclosure flags. A friendly refusal here is the vulnerability, so the type
// above holds no GitHub client at all and there is nothing to write with.
//
// The only refusals a human ever sees in v1 are the toolchain ones, which arrive
// with ADR 0003's detection.
func refuse(w http.ResponseWriter, e *github.IssuesEvent) {
	log.Printf("webhook: ignoring %s#%d: sender %q is type %q",
		e.GetRepo().GetFullName(), e.GetIssue().GetNumber(),
		e.GetSender().GetLogin(), e.GetSender().GetType())
	ignore(w, "sender")
}
