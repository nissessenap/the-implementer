package proxy

import (
	"context"
	"fmt"
	"log"

	"cloud.google.com/go/compute/metadata"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// GoogleIdentity resolves the proxy's own Google identity, once, at boot. One
// source for both credentials that need one — the model route and Artifact
// Registry — because they are the same identity: no new secret, no new mount, and
// one hourly token refresh rather than two caches that could disagree about it.
//
// The identity is Application Default Credentials, which in production means
// Workload Identity and so the metadata server. Nothing it finds here can go
// stale: there is no key to rotate — that is the point of Workload Identity — and
// the access token is the token source's business, asked for per request and
// refetched whenever the cached one is inside the refresh margin. (A key *file*
// would be different: its bytes are read once, here, and a rotated one would need
// a pod restart. It is one more reason the chart offers nowhere to mount one.)
func GoogleIdentity(ctx context.Context) (oauth2.TokenSource, error) {
	// Refused rather than left to ADC, whose search order is wider than the one
	// path this supports: `GOOGLE_APPLICATION_CREDENTIALS`, then gcloud's
	// well-known file, and only then the metadata server. Either of the first two
	// is a long-lived key — the credential Workload Identity exists to delete —
	// and either would also silence the identity log below, which is the metadata
	// server's answer or nothing. Unreachable from this chart today (one `env:`
	// block, no extraEnv, no gcloud in the image), which is what makes one line
	// enough to keep "Workload Identity only" enforced rather than merely written.
	if !metadata.OnGCE() {
		return nil, fmt.Errorf("no metadata server: the proxy's Google identity is Workload Identity, and there is deliberately no key to mount")
	}
	// Named in the log before anything is attached, because the one way left to
	// get this wrong is invisible otherwise: **the metadata server hands out the
	// node pool's service account** when a GKE cluster has no Workload Identity
	// binding, and it works — the proxy comes up, and every request then carries
	// the Compute Engine default identity instead of the one the operator granted
	// `roles/aiplatform.user` and `roles/artifactregistry.reader` to. Not refused
	// here, because the proxy has nothing to compare the answer against: only the
	// operator knows which account they meant. So it goes in the log, once, where
	// the wrong answer is legible.
	if email, err := metadata.EmailWithContext(ctx, "default"); err == nil {
		log.Printf("creds: the proxy's Google identity is %s — if that is a node pool's default service account, Workload Identity is not bound", email)
	}
	// cloud-platform because that is the scope a GKE metadata token comes back
	// with anyway; the *authorization* is `roles/aiplatform.user` and
	// `roles/artifactregistry.reader` on the proxy's Google service account, which
	// is where to narrow this.
	creds, err := google.FindDefaultCredentialsWithParams(ctx, google.CredentialsParams{
		Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
	})
	if err != nil {
		return nil, fmt.Errorf("no Application Default Credentials: %w", err)
	}
	return creds.TokenSource, nil
}
