package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
)

// SentinelPrefix is what makes a credential recognisably worthless, and the swap
// matches on it rather than on the whole of Sentinel: a sandbox holding the
// unpadded string must still swap, because the one failure this component may not
// have is a silent anonymous push.
const SentinelPrefix = "proxy-injected"

// Sentinel is what the sandbox actually carries — the prefix padded to a GitHub
// token's 40 bytes, so a swap never changes a request's length. Exported so the
// orchestrator writes exactly what this matches; TestSentinelIsTokenLength has
// the why.
//
// ponytail: 40 is a classic PAT and an installation token (`ghp_`/`ghs_` plus 36),
// which is every token this proxy hands out. A fine-grained PAT is ~93 bytes and
// would defeat the equal-length property silently — harmless while the swap only
// rewrites a header, and the ticket that mounts one has to widen this.
const Sentinel = SentinelPrefix + "--------------------------"

// Credential is one credential and the exact hosts it may be attached to. The host set
// is per-credential rather than global because the certificate is deliberately
// wider than the credential rule: `*.pkg.dev` is one SAN, and `-docker.pkg.dev`
// must still come out tokenless.
type Credential struct {
	// Name only ever appears in logs and load errors.
	Name string

	// Hosts are the names this credential may be attached to, each validated at
	// load to be a host the certificate intercepts. Either an exact name, or one
	// `*` standing for part of the leftmost label — `*-go.pkg.dev`, because
	// Artifact Registry's endpoints are regional and pinning a region here would
	// be a second place to configure one. A name no entry covers gets nothing,
	// which is the property the wide certificate leans on.
	Hosts []string

	// Attach makes the credential unconditional: the request leaves with
	// `Bearer <token>` whether or not it arrived with anything, and whatever it
	// did arrive with is overwritten.
	//
	// Per-credential and not global, because the two shapes are opposites and
	// each is wrong on the other's hosts. `pip` and `go mod download` send
	// Artifact Registry no credential at all and do not retry on a 401, so there
	// is no sentinel to match and nothing to swap; on GitHub a credential that is
	// not the sentinel may legitimately be the user's own, so it travels on
	// untouched. And on `api.github.com` an unconditional Basic is *ignored*
	// (measured: 200, limit 60, no error) — a silent anonymous request.
	Attach bool

	// Token answers "what is this run due on this host". The Run is the whole
	// point of the signature: MintedGitHub scopes the token it mints to the
	// repository the run's *annotations* name, never the one the request URL
	// does, and a Token that could not see the run could not do that.
	Token func(ctx context.Context, run Run) (string, error)
}

// Creds is credFor: the per-host switch deciding which credential, if any, a
// request to an intercepted host is due.
type Creds map[string]*Credential

// NewCreds binds credentials to hosts and refuses at load — not at request time —
// anything the certificate does not cover. A credential naming a host we do not
// intercept can never fire, so it is a config error that would otherwise show up
// as an unauthenticated request months later.
func NewCreds(certs *Certs, creds ...*Credential) (Creds, error) {
	c := Creds{}
	for _, cr := range creds {
		if len(cr.Hosts) == 0 {
			return nil, fmt.Errorf("credential %q is bound to no hosts", cr.Name)
		}
		for _, h := range cr.Hosts {
			// Lowercased at both ends, because the two halves of the decision
			// disagree otherwise: x509's VerifyHostname is case-insensitive, so
			// `CONNECT GitHub.com:443` is intercepted — and an exact map lookup
			// would then hand it no credential and push anonymously, silently.
			h = strings.TrimSuffix(strings.ToLower(h), ".")
			// A pattern is checked through one sample host — `x-go.pkg.dev` for
			// `*-go.pkg.dev` — which is sound for the reason matchHost gives, and
			// is a no-op on an exact name. An exact SAN fails the sample and the
			// credential is refused, which is the conservative answer.
			if !certs.Intercepts(strings.Replace(h, "*", "x", 1)) {
				return nil, fmt.Errorf("credential %q names %s, which is not on the certificate", cr.Name, h)
			}
			if prev, ok := c[h]; ok {
				return nil, fmt.Errorf("%s is claimed by both %q and %q", h, prev.Name, cr.Name)
			}
			c[h] = cr
		}
	}
	log.Printf("creds: %d host(s) carry a credential: %v", len(c), slices.Sorted(maps.Keys(c)))
	return c, nil
}

// matchHost answers whether host — lowercased already — is covered by pat, where a
// single leading `*` stands for one or more characters within the leftmost label.
//
// Deliberately not x509's rule, which only matches a wildcard occupying the
// *whole* leftmost label and is why the certificate has to carry `*.pkg.dev`.
// This is the narrower rule layered on top, and the no-dot check is what keeps it
// narrower — it is also what makes NewCreds's one-sample validation sound, since
// every host a pattern can match then differs from that sample in the leftmost
// label alone.
//
// Only a *leading* `*` is a pattern: `foo*.pkg.dev` is taken literally and so
// matches nothing. Not refused at load, because Hosts is never operator input —
// it is garHosts, in this package, and TestGARHostBinding fails on that typo.
func matchHost(pat, host string) bool {
	suffix, ok := strings.CutPrefix(pat, "*")
	if !ok {
		return pat == host
	}
	label, ok := strings.CutSuffix(host, suffix)
	return ok && label != "" && !strings.Contains(label, ".")
}

// For is the switch itself. A nil Creds answers "no credential" for every host,
// which is what a proxy configured with none is.
//
// Exact names first, so a host written out by name always beats a pattern that
// would also cover it.
// ponytail: then linear over the whole map. Two patterns that could both match a
// host would flap between credentials in map order — there is one
// pattern-bearing credential today and its own two patterns cannot overlap, so a
// second one wants a load-time overlap check rather than a cleverer loop here.
func (c Creds) For(host string) *Credential {
	// Lowercased *and* stripped of a trailing dot, because both are things x509
	// normalises away and the switch has to agree with it: VerifyHostname trims
	// the root label, so `CONNECT github.com.:443` is intercepted — and a lookup
	// that missed it would hand it no credential and push anonymously, with no
	// error. The same silent no-op as the casing, by a different normalisation.
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if cr, ok := c[host]; ok {
		return cr
	}
	for pat, cr := range c {
		if strings.HasPrefix(pat, "*") && matchHost(pat, host) {
			return cr
		}
	}
	return nil
}

// isSentinel is the one definition of "this credential is worthless". A prefix
// rather than equality on Sentinel: see SentinelPrefix.
func isSentinel(v string) bool { return strings.HasPrefix(v, SentinelPrefix) }

// swap rewrites req's Authorization header in place, and reports whether it
// touched anything. Two shapes of credential live here: Attach puts one on
// unconditionally, and everything below swaps a sentinel for the real thing.
//
// For the swap, the real token is fetched only once a sentinel has actually
// been found — git's anonymous first request carries none, and it must not be
// refused because a Secret read happened to fail on a request that wanted nothing.
//
// Both shapes, because a proxy built to either alone breaks the other silently —
// and that is not hypothetical: OpenAI's `codex` and Anthropic's `sandbox-runtime`
// both no-op on git's, with no error and no log, because they look for the
// sentinel as a substring of the header value and base64 hides it. See ADR 0005.
//
//   - `Basic base64(x-access-token:SENTINEL)` — git, from the clone URL's
//     userinfo. Either half may be the sentinel: `https://SENTINEL@github.com/`
//     puts it in the username with an empty password.
//   - `token SENTINEL` / `Bearer SENTINEL` — `gh`, and the REST API generally.
//
// A credential that is not the sentinel travels on untouched and logged: it is
// something the sandbox brought itself, and swallowing it would only hide it.
func (cr *Credential) swap(ctx context.Context, req *http.Request, host string, run Run) (bool, error) {
	if cr.Attach {
		tok, err := cr.Token(ctx, run)
		if err != nil {
			return false, err
		}
		// Overwritten and not passed through — the opposite of the sentinel rule
		// below, and deliberate: Artifact Registry has no equivalent of "a
		// credential that is not ours might still be legitimately the user's",
		// and leaving one there would only 401.
		req.Header.Set("Authorization", "Bearer "+tok)
		return true, nil
	}

	h := req.Header.Get("Authorization")
	if h == "" {
		// Nothing to swap, and this credential is not an Attach one. Not an
		// error: every client in the sandbox that wants GitHub holds the
		// sentinel, and git retries with it after a 401 because the phase script
		// puts it in the URL's userinfo. A host that genuinely needs a credential
		// out of nothing — `go` against a private module, `pip` against Artifact
		// Registry — sets Attach, which is why that decision is per-credential
		// and not a fallback here.
		return false, nil
	}
	scheme, rest, ok := strings.Cut(h, " ")
	if !ok {
		log.Printf("%s: unparseable Authorization passed through", host)
		return false, nil
	}

	if strings.EqualFold(scheme, "basic") {
		raw, err := base64.StdEncoding.DecodeString(rest)
		if err != nil {
			log.Printf("%s: undecodable Basic credential passed through", host)
			return false, nil
		}
		user, pass, ok := strings.Cut(string(raw), ":")
		if !ok {
			// Logged and not swapped: RFC 7617 Basic always carries the colon,
			// even with an empty password, so no client `git` or `gh` use emits
			// this. But the whole decoded value is checkable here, so the log can
			// say which of the two this is — an ordinary pass-through, or a
			// sentinel that went upstream unswapped and made the request
			// anonymous. The second is this component's one unacceptable failure,
			// and it must not read like the first.
			if isSentinel(user) {
				log.Printf("%s: THE SENTINEL PASSED THROUGH UNSWAPPED — a Basic credential with no colon; this request is anonymous", host)
			} else {
				log.Printf("%s: Basic credential without a colon passed through", host)
			}
			return false, nil
		}
		if !isSentinel(user) && !isSentinel(pass) {
			log.Printf("%s: the sandbox's own Basic credential passed through", host)
			return false, nil
		}
		tok, err := cr.Token(ctx, run)
		if err != nil {
			return false, err
		}
		// The password is git's shape; the username is the
		// `https://TOKEN@host/` spelling, which GitHub accepts with no password.
		if isSentinel(pass) {
			pass = tok
		}
		if isSentinel(user) {
			user = tok
		}
		req.Header.Set("Authorization", "Basic "+
			base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
		return true, nil
	}

	if !isSentinel(rest) {
		log.Printf("%s: the sandbox's own %s credential passed through", host, scheme)
		return false, nil
	}
	tok, err := cr.Token(ctx, run)
	if err != nil {
		return false, err
	}
	// The scheme is kept as sent: GitHub accepts `token` and `Bearer`, and
	// rewriting one to the other is a difference we would have to defend.
	req.Header.Set("Authorization", scheme+" "+tok)
	return true, nil
}

// githubHosts are the hosts a GitHub token is attached to. A subset of the
// certificate's GitHub SANs, and the missing one is the point:
// objects.githubusercontent.com serves release and LFS blobs from pre-signed
// URLs, which already carry their own authorization and must not also carry ours.
// It is the same rule that keeps `-docker.pkg.dev` tokenless.
var githubHosts = []string{
	"github.com",
	"api.github.com",
	"codeload.github.com",
	"raw.githubusercontent.com",
}

// StaticGitHub is the GitHub credential read from a file — a mounted Secret
// holding a PAT or an installation token.
//
// Not the production path: MintedGitHub is, and it scopes what it hands out to the
// calling run's repository where this hands out whatever is in the Secret. This
// stays as the seam that keeps the swap itself testable with no App, no signer and
// no KMS. cmd/proxy refuses to boot with both.
func StaticGitHub(file string) (*Credential, error) {
	read := func() (string, error) {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		// Trimmed, because a token that arrives from a file rather than
		// --from-literal carries a trailing newline and GitHub answers 401.
		tok := strings.TrimSpace(string(b))
		// Empty is refused *here* rather than only at load, because this closure
		// runs per request: a Secret rotated to an empty value would otherwise
		// swap in nothing, send `Basic base64(x-access-token:)` — an anonymous
		// request — and log it as a successful swap. Refused, it is a 502.
		if tok == "" {
			return "", fmt.Errorf("%s is empty", file)
		}
		return tok, nil
	}
	if _, err := read(); err != nil {
		return nil, err
	}
	return &Credential{
		Name:  "github",
		Hosts: githubHosts,
		// The run is ignored here, and that is the seam: a static token is not
		// scoped to anything, which is exactly why #53 replaced it in production.
		Token: func(context.Context, Run) (string, error) { return read() },
	}, nil
}
