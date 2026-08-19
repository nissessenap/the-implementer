package proxy

import (
	"context"
	"fmt"
	"log"
	"time"

	"cloud.google.com/go/compute/metadata"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// garHosts are the Artifact Registry endpoints a Google bearer is attached to, and
// they are an **allowlist of auth shapes that were actually measured** rather than
// a denylist of the one that was not.
//
// `{region}-docker.pkg.dev` is the absence that matters. Its `/v2/` returns 401
// with a `WWW-Authenticate: Bearer realm=…` challenge and the client fetches a
// scoped token from that realm — a dance that fires *only* on unauthenticated
// requests, so attaching a bearer unconditionally may suppress it. Plausible,
// untested, and complicated further by blob fetches redirecting to storage URLs
// that must not carry our token. It belongs to #28, and until then it is
// intercepted and tokenless — which is the whole reason the credential rule is
// narrower than the `*.pkg.dev` certificate rather than read off it.
var garHosts = []string{"*-go.pkg.dev", "*-python.pkg.dev"}

// garScope is cloud-platform because that is the scope a GKE metadata token comes
// back with anyway; the *authorization* is `roles/artifactregistry.reader` on the
// proxy's Google service account, which is where to narrow this.
const garScope = "https://www.googleapis.com/auth/cloud-platform"

// GAR is the Artifact Registry credential: the proxy's own Google identity, as a
// plain bearer, on the Go and Python endpoints.
//
// Unlike GitHub there is no sentinel and nothing to swap. `pip` and
// `go mod download` reach a private repository with no credential at all, get a
// 401 or a 404, and do not retry — so the proxy has to supply one unconditionally.
// The sandbox therefore holds nothing for GAR, not even a worthless string.
//
// The identity is Application Default Credentials, which in production means
// Workload Identity and so the metadata server. Resolved and warmed here at boot,
// so a missing or unusable credential kills the pod rather than 502ing the first
// `pip install` twenty minutes into a run.
//
// Boot is only where the *discovery* happens, and nothing it finds can go stale:
// there is no key to rotate — that is the point of Workload Identity — and the
// hourly access token is the token source's business, not ours. It is asked for one
// per request below, and refetches from the metadata server whenever the cached one
// is inside the refresh margin. (A key *file* would be different: its bytes are
// read once, here, and a rotated one would need a pod restart. It is one more
// reason the chart offers nowhere to mount one.)
func GAR(ctx context.Context) (*Credential, error) {
	// Named in the log before anything is attached, because the one way to get
	// this wrong is invisible otherwise: **ADC falls back to the node pool's
	// service account** when a GKE cluster has no Workload Identity binding, and
	// it works — the proxy comes up, and every sandbox request to Artifact
	// Registry then carries the Compute Engine default identity instead of the one
	// the operator granted `roles/artifactregistry.reader` to. Not refused here,
	// because the proxy has nothing to compare the answer against: only the
	// operator knows which account they meant. So it goes in the log, once, where
	// the wrong answer is legible.
	if email, err := metadata.EmailWithContext(ctx, "default"); err == nil {
		log.Printf("creds: the proxy's Google identity is %s — if that is a node pool's default service account, Workload Identity is not bound", email)
	}
	creds, err := google.FindDefaultCredentialsWithParams(ctx, google.CredentialsParams{
		Scopes: []string{garScope},
		// Set explicitly, because the default is *zero* — which falls back to a
		// 10-second margin and would hand out tokens with seconds left on them.
		// 3m45s is what x/oauth2's own ComputeTokenSource uses, for its reason:
		// the metadata server's cache is at least four minutes, so refreshing
		// earlier than this buys nothing and refreshing later cuts it fine. Only
		// the metadata path reads this field, which is the only path we support.
		EarlyTokenRefresh: 225 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("no Application Default Credentials: %w", err)
	}
	return gar(creds.TokenSource)
}

// gar is GAR with the token source injected, which is how the test gets one
// without a Google to ask.
func gar(ts oauth2.TokenSource) (*Credential, error) {
	tok, err := ts.Token()
	if err != nil {
		return nil, fmt.Errorf("Google token: %w", err)
	}
	log.Printf("creds: attaching the proxy's own Google identity to %v (first token expires %s)",
		garHosts, tok.Expiry.UTC().Format(time.RFC3339))
	return &Credential{
		Name:   "gar",
		Hosts:  garHosts,
		Attach: true,
		// Refreshed by the token source, not by us: what ADC hands back is
		// already a ReuseTokenSource, so this is a mutex and a clock comparison
		// on all but one call an hour — and that one call is what makes token
		// rotation a non-event. There is deliberately no cache of our own here;
		// a second one could only disagree with that one.
		// The run is ignored — a GAR token is the proxy's own identity and is not
		// scoped to anything a run says, which is exactly why `pip` in the
		// sandbox can only reach repositories the operator granted the proxy.
		Token: func(context.Context, Run) (string, error) {
			t, err := ts.Token()
			if err != nil {
				return "", err
			}
			// Refused rather than attached, for StaticGitHub's reason: an empty
			// token sends `Authorization: Bearer ` — an anonymous request — and
			// the log would report it as a credential attached. ReuseTokenSource
			// hands a freshly minted token straight back without calling
			// Valid(), so an STS or metadata response carrying an empty
			// access_token arrives here unchecked.
			if t.AccessToken == "" {
				return "", fmt.Errorf("the Google token source returned an empty access token")
			}
			return t.AccessToken, nil
		},
	}, nil
}
