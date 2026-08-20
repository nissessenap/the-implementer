package proxy

import (
	"context"
	"fmt"
	"log"

	"golang.org/x/oauth2"
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

// GAR is the Artifact Registry credential: the proxy's own Google identity, as a
// plain bearer, on the Go and Python endpoints.
//
// Unlike GitHub there is no sentinel and nothing to swap. `pip` and
// `go mod download` reach a private repository with no credential at all, get a
// 401 or a 404, and do not retry — so the proxy has to supply one unconditionally.
// The sandbox therefore holds nothing for GAR, not even a worthless string.
//
// The token source is GoogleIdentity's, shared with the model route, and it is
// warmed here at boot: a missing or unusable credential kills the pod rather than
// 502ing the first `pip install` twenty minutes into a run.
func GAR(ts oauth2.TokenSource) (*Credential, error) {
	cred := &Credential{
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
				return "", fmt.Errorf("Google token: %w", err)
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
	}
	// Warmed through the credential's own Token, so boot refuses exactly what a
	// request would — a source that errors, and one that answers with no error and
	// no token — and the two refusals cannot drift apart.
	if _, err := cred.Token(context.Background(), Run{}); err != nil {
		return nil, err
	}
	log.Printf("creds: attaching the proxy's own Google identity to %v", garHosts)
	return cred, nil
}
