package orchestrator

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nissessenap/the-implementer/proxy"
)

const testSecret = "not-a-real-webhook-secret"

// payload is an `issues` delivery in the shape GitHub actually sends, trimmed to
// the fields this path reads plus the whole of the **required** set: `action`,
// `issue`, `repository`, `sender`. `label` is deliberately *not* in that set, which
// is the one thing about this payload worth knowing — the `nil` case below is a
// real delivery and not a hypothetical.
//
// A literal rather than a builder: what is under test is a decision about a wire
// format, and a builder would let the test and GitHub drift apart in the one place
// they must not.
func payload(action, label, senderLogin, senderType string) string {
	lbl := "null"
	if label != "" {
		lbl = fmt.Sprintf(`{"id":208045946,"node_id":"MDU6TGFiZWwy","name":%q,"color":"0e8a16","default":false}`, label)
	}
	return fmt.Sprintf(`{
  "action": %q,
  "label": %s,
  "issue": {
    "number": 73,
    "title": "Orchestrator: the webhook front-end",
    "state": "open",
    "user": {"login": "someone", "type": "User"},
    "labels": [{"name": "ready-for-agent"}]
  },
  "repository": {
    "id": 1296269,
    "name": "the-implementer",
    "full_name": "nissessenap/the-implementer",
    "private": false,
    "owner": {"login": "nissessenap", "type": "User"}
  },
  "sender": {"login": %q, "type": %q},
  "installation": {"id": 4242}
}`, action, lbl, senderLogin, senderType)
}

// recorder is a Webhook wired for a test: the secret every signature below is
// computed with, and a Start that records the run rather than creating a Job. The
// wiring is here and not in deliver(), which only delivers — a helper that quietly
// rewrote the handler it was handed is how a test ends up asserting the helper.
func recorder() (*Webhook, *[]proxy.Run) {
	var started []proxy.Run
	return &Webhook{
		Secret: []byte(testSecret),
		Start: func(_ context.Context, r proxy.Run) error {
			started = append(started, r)
			return nil
		},
	}, &started
}

// sign is what GitHub does: HMAC-SHA256 over the raw body.
func sign(body string) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// deliver POSTs one delivery at the handler and returns the response. Signed
// correctly unless a signature is given, because every case but one needs a valid
// one.
func deliver(t *testing.T, h *Webhook, event, body string, sig ...string) *http.Response {
	t.Helper()
	s := sign(body)
	if len(sig) == 1 {
		s = sig[0]
	}
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", "72d3162e-cc78-11e3-81ab-4c9367dc0958")
	if s != "" {
		req.Header.Set("X-Hub-Signature-256", s)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Result()
}

// The trigger itself: a labelled issue becomes exactly one run, carrying the
// identity the Job builder needs — and **no UID**, because picking one is the
// caller's job and is what keeps a re-run from inheriting the last run's
// credential.
func TestLabelStartsARun(t *testing.T) {
	h, started := recorder()
	resp := deliver(t, h, "issues", payload("labeled", readyLabel, "a-maintainer", "User"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d, want 202", resp.StatusCode)
	}
	if len(*started) != 1 {
		t.Fatalf("started %d runs, want 1", len(*started))
	}
	want := proxy.Run{Owner: "nissessenap", Repo: "the-implementer", Issue: "73"}
	if (*started)[0] != want {
		t.Errorf("started %#v, want %#v", (*started)[0], want)
	}
}

// Redelivery and a second label are the same request twice, and the handler adds
// no dedupe state — which is the requirement: idempotency is the Job name plus a
// swallowed AlreadyExists, and this must not defeat it by, say, keying anything on
// the delivery id. So both deliveries start a run and both runs have the same name.
func TestRedeliveryResolvesToOneJobName(t *testing.T) {
	body := payload("labeled", readyLabel, "a-maintainer", "User")
	h, started := recorder()
	deliver(t, h, "issues", body)
	deliver(t, h, "issues", body)
	if len(*started) != 2 {
		t.Fatalf("started %d runs across two deliveries, want 2", len(*started))
	}
	if a, b := JobName((*started)[0]), JobName((*started)[1]); a != b {
		t.Errorf("redelivery derived %q, first delivery derived %q", b, a)
	}
}

// Everything that is not a run, and none of it is an error: a non-2xx marks the
// delivery failed on the App's own page and invites a redelivery of an event we
// will ignore again.
func TestIgnored(t *testing.T) {
	for _, tc := range []struct {
		name, event, body string
	}{
		// The payload trap: `label` is not in the required set, so this is a real
		// delivery shape and it must be ignored rather than dereferenced.
		{"labeled with no label object", "issues", payload("labeled", "", "a-maintainer", "User")},
		{"some other label", "issues", payload("labeled", "needs-triage", "a-maintainer", "User")},
		{"unlabeled", "issues", payload("unlabeled", readyLabel, "a-maintainer", "User")},
		{"opened", "issues", payload("opened", "", "a-maintainer", "User")},
		// The ping arrives the moment the webhook is created, and a 4xx on it is
		// a red delivery page for something entirely correct.
		{"a ping", "ping", `{"zen":"Non-blocking is better than blocking."}`},
		{"another event type", "pull_request", payload("labeled", readyLabel, "a-maintainer", "User")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, started := recorder()
			resp := deliver(t, h, tc.event, tc.body)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status %d, want 200", resp.StatusCode)
			}
			if len(*started) != 0 {
				t.Errorf("started %d runs, want 0", len(*started))
			}
		})
	}
}

// The authorization, and the assertion that matters is the *silence*: no run, and
// nothing said to GitHub. The second half is structural — Webhook holds no GitHub
// client, so there is nothing here that could comment — and it is deliberate: on a
// public repository a friendly refusal is an on-demand way to make the App write to
// issues, which is the `issues: write` plus untrusted-input combination the
// disclosure flags.
func TestSenderRefusals(t *testing.T) {
	for _, tc := range []struct{ name, login, typ string }{
		// The clause that was actually exploited. The attack needs no access to the
		// target repository: install your own App on your own repository and use its
		// installation token here.
		{"a bot", "some-app[bot]", "Bot"},
		{"an organization", "acme", "Organization"},
		// Substituted by GitHub for an unresolvable actor, and **its type is
		// `User`** — so the type assertion alone lets it through.
		{"ghost", "ghost", "User"},
		{"ghost, differently cased", "Ghost", "User"},
		// Fail-closed on a type nobody has documented, rather than on a list of
		// types to reject.
		{"a type nobody has seen", "mystery", "Mannequin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logged strings.Builder
			restore := log.Writer()
			log.SetOutput(&logged)
			defer log.SetOutput(restore)

			h, started := recorder()
			resp := deliver(t, h, "issues", payload("labeled", readyLabel, tc.login, tc.typ))
			if len(*started) != 0 {
				t.Fatalf("started %d runs, want 0", len(*started))
			}
			if resp.StatusCode != http.StatusOK {
				t.Errorf("status %d, want 200", resp.StatusCode)
			}
			// Logged is the whole of the refusal, so an operator can still find out
			// it happened. It is the only channel: see refuse().
			if !strings.Contains(logged.String(), tc.login) {
				t.Errorf("the refusal did not name the sender: %q", logged.String())
			}
		})
	}
}

// The webhook secret, in the three shapes that are not "correct".
func TestSignature(t *testing.T) {
	body := payload("labeled", readyLabel, "a-maintainer", "User")

	for _, tc := range []struct{ name, sig string }{
		{"a wrong signature", "sha256=" + strings.Repeat("00", sha256.Size)},
		// Not "unsigned means unverified": with a secret configured, go-github
		// requires the header.
		{"no signature at all", ""},
		{"a signature that is not hex", "sha256=not-hex-at-all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, started := recorder()
			resp := deliver(t, h, "issues", body, tc.sig)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status %d, want 401", resp.StatusCode)
			}
			if len(*started) != 0 {
				t.Errorf("started %d runs, want 0", len(*started))
			}
		})
	}
}

// An unconfigured secret is refused at every request rather than tolerated. Without
// this the endpoint is an open trigger for anything that can reach the Service, and
// nothing about a working installation would show it: go-github validates nothing
// when the secret is empty and no signature arrives.
func TestNoSecretConfiguredRefusesEverything(t *testing.T) {
	h, started := recorder()
	// Unwired on purpose, and with no signature either — which is the shape
	// go-github validates nothing at all for.
	h.Secret = nil
	resp := deliver(t, h, "issues", payload("labeled", readyLabel, "a-maintainer", "User"), "")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", resp.StatusCode)
	}
	if len(*started) != 0 {
		t.Errorf("started %d runs with no secret configured, want 0", len(*started))
	}
}

// A create that fails is a 500 GitHub will redeliver — deliberately not a swallowed
// error behind a 202. A dropped run nobody is told about is the failure this whole
// system is built against.
func TestStartFailureIsRedeliverable(t *testing.T) {
	h, _ := recorder()
	h.Start = func(context.Context, proxy.Run) error { return fmt.Errorf("apiserver said no") }
	resp := deliver(t, h, "issues", payload("labeled", readyLabel, "a-maintainer", "User"))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", resp.StatusCode)
	}
	if b, _ := io.ReadAll(resp.Body); strings.Contains(string(b), "apiserver said no") {
		t.Errorf("the response repeats the internal error: %q", b)
	}
}
