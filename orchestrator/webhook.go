package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

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
// client to report a refusal is the vulnerability, not the fix.
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
	// apiserver round-trip, and its error is worth a 500 — GitHub records the
	// delivery as failed and it stays redeliverable by hand for three days.
	// GitHub does **not** retry on its own, so a 500 here is a lost run unless a
	// human notices a red delivery page; that is still strictly better than a
	// goroutine, which would hide the same loss behind a 202.
	Start func(context.Context, proxy.Run) error
}

// maxBody is the cap on a delivery, and it is the *only* one that does anything:
// go-github's own limit is GitHub's 25MB payload ceiling, which this endpoint has
// no use for — a real `issues` payload is tens of KB. The body is read in full
// before the signature can be checked, so an unsigned POST costs this much memory
// against the Deployment's 256Mi and its single replica.
const maxBody = 1 << 20

// Routes is the endpoint, mux included, so that the mux is a tested thing rather
// than four lines in main() that no test ever reaches. A method pattern, so a GET
// at /webhook is a 405 and not a signature failure that reads like a secret
// mismatch.
func Routes(h http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("POST /webhook", http.MaxBytesHandler(h, maxBody))
	// The readiness probe. "The listener is up" is the whole of this component's
	// health: everything it depends on — the apiserver, the template, the run key —
	// is either resolved at boot or per request.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	return mux
}

func (h *Webhook) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if len(h.Secret) == 0 {
		http.Error(w, "no webhook secret configured", http.StatusInternalServerError)
		log.Print("webhook: refusing every delivery: no webhook secret is configured")
		return
	}
	// The signature, over the raw body, in constant time — go-github's, because
	// the one already in this binary is the one least likely to be subtly wrong.
	// It accepts `X-Hub-Signature` (HMAC-SHA1) as well as the SHA-256 header, and
	// that is left alone deliberately: SHA-1 HMAC is not broken, and pinning the
	// header would also drop the form-encoded content type GitHub's webhook UI
	// offers, which the same function handles for free.
	body, err := github.ValidatePayload(r, h.Secret)
	if err != nil {
		// An oversize body arrives here too, and must not be reported as a
		// signature failure: the one thing a 401 is for is a secret mismatch, and
		// a payload cap is a misconfiguration a human has to be able to tell apart.
		if errors.As(err, new(*http.MaxBytesError)) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			log.Printf("webhook: oversize delivery %s", github.DeliveryID(r))
			return
		}
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
	// The same reasoning as the label guard, one field over: `issue` is required by
	// the schema, so an absent one is GitHub sending something impossible — but
	// `GetNumber()` answers 0 for it, `owner/repo#0` passes ParseIssue's numeric
	// check, and the run would be a Job for an issue that cannot exist.
	if e.GetIssue().GetNumber() <= 0 {
		ignore(w, "no issue number")
		return
	}
	// Closed issues, which the trigger contract did not mention and should have:
	// labelling one is ordinary housekeeping, and a run is ~450s, ~$2, a branch and
	// a pull request for work that is already done. Compared against "open" rather
	// than against "closed" so that a state GitHub adds later ignores by default.
	if s := e.GetIssue().GetState(); s != "open" {
		ignore(w, "issue state %q", s)
		return
	}

	// Authorization: two clauses on the payload, and no permission API call. ADR
	// 0004 says why the obvious design has it backwards in both directions — the
	// clause usually omitted is the one the flatt.tech bypass turned on, and the one
	// usually insisted on is the one that failed to help. `ghost` is folded in
	// case-insensitively because GitHub substitutes it for unresolvable actors and
	// **its type is `User`**, so the type assertion alone lets it through. The
	// edit-after-label window it does not cover is #32, and deliberately open.
	//
	// **The silence is the security property**: logged, and nothing else. A "sorry,
	// you're not allowed" comment on a public repository would hand an unauthorized
	// actor an on-demand way to make the App write to issues, so the type above holds
	// no GitHub client and there is nothing here to write with. The only refusals a
	// human ever sees in v1 are the toolchain ones, with ADR 0003's detection.
	if sender := e.GetSender(); sender.GetType() != "User" || strings.EqualFold(sender.GetLogin(), "ghost") {
		log.Printf("webhook: ignoring %s#%d: sender %q is type %q", e.GetRepo().GetFullName(),
			e.GetIssue().GetNumber(), sender.GetLogin(), sender.GetType())
		ignore(w, "sender")
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
	// Detached from the request, and then bounded: GitHub gives up on the delivery
	// at 10s, and on the request context the apiserver Create would be cancelled
	// with it — losing the one thing this component exists to do, for a client that
	// had already stopped listening. WithoutCancel alone would leave it unbounded,
	// because the in-cluster rest.Config sets no timeout of its own.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	if err := h.Start(ctx, run); err != nil {
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
