package proxy

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/google/go-github/v88/github"
	"github.com/isometry/ghait"
)

// GitHubApp is the App the proxy mints installation tokens as. The private key
// never exists here: Provider and Key name something that signs on our behalf —
// a KMS crypto key version in production, a PEM file in the e2e.
type GitHubApp struct {
	// AppID is the GitHub App's numeric id.
	AppID int64

	// Provider is a ghait provider name: "file" or "gcp". Naming it is not the
	// same as *having* it — a provider is only in the registry if it was linked
	// in by build tag. An unlinked name fails at boot, by name.
	Provider string

	// Key is whatever that provider takes: a path to a PKCS#1 PEM for "file", a
	// `projects/…/cryptoKeyVersions/1` resource name for "gcp".
	Key string

	// BaseURL retargets the GitHub API the mint path talks to, and is empty for
	// api.github.com. The seam the e2e points at an in-cluster mock: a stage that
	// needs a real App credential is a stage that skips, and a skipped stage proves
	// nothing. Used as given — no `/api/v3/` is appended — so one environment
	// variable means the same thing here and on the orchestrator's own client.
	BaseURL string
}

// refreshSkew is how much of an installation token's life we refuse to use.
// GitHub gives them an hour; a run that starts a clone at 59 minutes must not
// have it die mid-transfer, and re-minting is one API call.
const refreshSkew = 5 * time.Minute

// MintedGitHub is the GitHub credential the proxy mints for itself, per run.
//
// The whole reason it exists rather than a Secret full of a token: the token is
// scoped to **the repository the run's annotations name**, so a compromised
// sandbox holds a credential for its own repository and nothing else. Get that
// wrong — scope it to the repository the request URL names — and the blast
// radius is every repository the App is installed on.
//
// Signing is external and per-mint: GitHub caps a GitHub App JWT's `exp` at ten
// minutes, so there is nothing worth caching between the key and the API. The
// installation token that comes back is what is cached, per run.
func MintedGitHub(ctx context.Context, app GitHubApp) (*Credential, error) {
	return mintedGitHub(ctx, app)
}

// mintedGitHub is MintedGitHub with ghait's options exposed, which is how the
// test points it at an httptest GitHub. Unexported so ghait stays an
// implementation detail of this package rather than part of its API.
func mintedGitHub(ctx context.Context, app GitHubApp, opts ...ghait.Option) (*Credential, error) {
	if app.AppID == 0 || app.Provider == "" || app.Key == "" {
		return nil, fmt.Errorf("GitHub App needs an id, a provider and a key (got id=%d provider=%q key set=%v)",
			app.AppID, app.Provider, app.Key != "")
	}
	// WithValidateKey, because it defaults to *false* and without it nothing
	// checks the key until the first run needs a token — at which point the
	// symptom is a failed run rather than a pod that never went ready. For "gcp"
	// it is the ENABLED + RSA_SIGN_PKCS1_2048_SHA256 check, and it needs
	// cloudkms.cryptoKeyVersions.get on top of the signer role.
	//
	// Installation id 0: this proxy has no single installation. It resolves one
	// per run, from the run's own repository, below.
	cfg := ghait.NewConfig(app.AppID, 0, app.Provider, app.Key).WithValidateKey(true)
	// No transport of ours, deliberately: NewGHAIT builds on http.DefaultTransport,
	// so an operator behind a corporate egress proxy or a private CA gets
	// https_proxy and SSL_CERT_FILE/SSL_CERT_DIR honoured with no code at all.
	// Handing it one would take that away silently.
	if app.BaseURL != "" {
		// Prepended, so an option a caller passed explicitly still wins — which is
		// how mint_test points this at its own httptest GitHub.
		opts = append([]ghait.Option{ghait.WithURLs(app.BaseURL, app.BaseURL)}, opts...)
	}
	g, err := ghait.NewGHAIT(ctx, cfg, opts...)
	if err != nil {
		return nil, fmt.Errorf("GitHub App %d via %s: %w", app.AppID, app.Provider, err)
	}
	m := &minter{app: g, api: g.Client, tokens: map[Run]mintedToken{}}
	log.Printf("creds: minting GitHub installation tokens as App %d, signed by %s", app.AppID, app.Provider)
	return &Credential{Name: "github-app", Hosts: githubHosts, Token: m.token}, nil
}

type mintedToken struct {
	tok string
	exp time.Time
}

// minter caches one installation token per run and mints a new one when it is
// close enough to expiry to be no use.
type minter struct {
	// app mints installation tokens, signing each App JWT through the external
	// key. ghait.GHAIT is an interface and deliberately narrow: it does not
	// expose the client below, which is why both fields exist.
	app ghait.GHAIT
	// api is ghait's own GitHub client, which already carries the App JWT
	// transport — the only thing that may ask "which installation has this
	// repository". Taken rather than built, so there is one signer and one rate
	// limiter rather than two.
	api *github.Client

	// ponytail: one lock over every run, held across the mint's round-trip to
	// GitHub, so a slow mint stalls every other run's swap for its duration —
	// and a *failing* mint is not cached at all, so a run whose repository the
	// App cannot see pays that round-trip on every request. Mints are once an
	// hour per run and the map is never swept, so it is bounded by the runs one
	// proxy sees in its lifetime. Per-run locks, a negative cache and a sweep if
	// any of the three stops being true.
	mu     sync.Mutex
	tokens map[Run]mintedToken
}

func (m *minter) token(ctx context.Context, run Run) (string, error) {
	if !run.Complete() {
		// Unreachable through the proxy — authenticate() refuses an incomplete
		// claim before anything gets this far — but a mint scoped to "" is the
		// one mistake here that is worth failing loudly rather than defensively.
		return "", fmt.Errorf("refusing to mint for an incomplete run %q", run)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tokens[run]; ok && time.Now().Before(t.exp.Add(-refreshSkew)) {
		return t.tok, nil
	}

	// THE SCOPE, and it reads off `run` — the annotations the proxy resolved from
	// the calling pod — and never off the request. Both halves matter: the
	// installation is the one that has *this* repository, and Repositories narrows
	// the token to that one repository within it.
	//
	// Note where the refusal happens: the proxy never compares the run against the
	// repository a request URL names, because it does not have to. The token it
	// attaches simply cannot open anything else, and GitHub is the one that says
	// so. One place to get right rather than two that can disagree.
	inst, _, err := m.api.Apps.GetRepositoryInstallation(ctx, run.Owner, run.Repo)
	if err != nil {
		return "", fmt.Errorf("no installation for %s: %w", run, err)
	}
	tok, err := m.app.NewInstallationToken(ctx, inst.GetID(), &github.InstallationTokenOptions{
		Repositories: []string{run.Repo},
	})
	if err != nil {
		return "", fmt.Errorf("minting for %s: %w", run, err)
	}
	exp := tok.GetExpiresAt().Time
	log.Printf("minted an installation token for %s (installation %d), good until %s",
		run, inst.GetID(), exp.UTC().Format(time.RFC3339))
	m.tokens[run] = mintedToken{tok: tok.GetToken(), exp: exp}
	return tok.GetToken(), nil
}
