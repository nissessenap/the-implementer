package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
)

// SentinelPrefix is what makes a credential recognisably worthless. The sandbox's
// GH_TOKEN starts with it, and anything presented to a credentialed host that
// starts with it is swapped for the real thing.
//
// A prefix rather than an equality test on Sentinel below, deliberately: the one
// failure mode this component must not have is a *silent* no-op, and a sandbox
// holding the unpadded string would otherwise push anonymously with no error and
// no log. Matching loosely costs nothing — anyone holding the sentinel already
// gets a token by presenting it correctly.
const SentinelPrefix = "proxy-injected"

// Sentinel is the value the sandbox actually carries: SentinelPrefix padded to 40
// bytes, which is what every GitHub token this proxy swaps in measures (`ghs_` or
// `ghp_` plus 36).
//
// The swap only rewrites a header today, and no framing depends on a header's
// length — so the padding buys nothing *yet*, and that is the point of doing it
// now rather than later. The moment a credential travels in a body, an unequal
// swap shifts Content-Length, and the symptom is a broken request nowhere near
// this code. Equal lengths make that impossible for free.
//
// Exported so the orchestrator writes into the sandbox exactly what this matches,
// from one constant — the same seam as the annotation names in identity.go.
// A token of some other length is not an error, only a warning; see StaticGitHub.
const Sentinel = SentinelPrefix + "--------------------------"

// Credential is one credential and the exact hosts it may be attached to. The host set
// is per-credential rather than global because the certificate is deliberately
// wider than the credential rule: `*.pkg.dev` is one SAN, and `-docker.pkg.dev`
// must still come out tokenless.
type Credential struct {
	// Name only ever appears in logs and load errors.
	Name string

	// Hosts are exact names, each validated at load to be a host the certificate
	// intercepts. Exact and not patterns: a name that is not written here gets
	// nothing, which is the property the wide certificate leans on.
	// ponytail: #54's regional `{region}-go.pkg.dev` set may not be enumerable —
	// that ticket adds a pattern if it is not, and inherits the same validation.
	Hosts []string

	// Token is read per request rather than cached, so a rotated Secret takes
	// effect without a restart. It is a file read from a tmpfs mount next to a
	// network round-trip; #53 replaces it with a minted, cached token.
	Token func(ctx context.Context) (string, error)
}

// Creds is credFor: the per-host switch deciding which credential, if any, a
// request to an intercepted host is due.
type Creds struct {
	byHost map[string]*Credential
}

// NewCreds binds credentials to hosts and refuses at load — not at request time —
// anything the certificate does not cover. A credential naming a host we do not
// intercept can never fire, so it is a config error that would otherwise show up
// as an unauthenticated request months later.
func NewCreds(certs *Certs, creds ...*Credential) (*Creds, error) {
	c := &Creds{byHost: map[string]*Credential{}}
	for _, cr := range creds {
		if len(cr.Hosts) == 0 {
			return nil, fmt.Errorf("credential %q is bound to no hosts", cr.Name)
		}
		for _, h := range cr.Hosts {
			// Lowercased at both ends, because the two halves of the decision
			// disagree otherwise: x509's VerifyHostname is case-insensitive, so
			// `CONNECT GitHub.com:443` is intercepted — and an exact map lookup
			// would then hand it no credential and push anonymously, silently.
			h = strings.ToLower(h)
			if !certs.Intercepts(h) {
				return nil, fmt.Errorf("credential %q names %s, which is not on the certificate", cr.Name, h)
			}
			if prev, ok := c.byHost[h]; ok {
				return nil, fmt.Errorf("%s is claimed by both %q and %q", h, prev.Name, cr.Name)
			}
			c.byHost[h] = cr
		}
	}
	log.Printf("creds: %d host(s) carry a credential: %v", len(c.byHost), c.hosts())
	return c, nil
}

func (c *Creds) hosts() []string {
	hs := make([]string, 0, len(c.byHost))
	for h := range c.byHost {
		hs = append(hs, h)
	}
	return hs
}

// For is the switch itself. A nil *Creds answers "no credential" for every host,
// which is what a proxy configured with none is.
func (c *Creds) For(host string) *Credential {
	if c == nil {
		return nil
	}
	return c.byHost[strings.ToLower(host)]
}

// isSentinel is the one definition of "this credential is worthless". A prefix
// rather than equality on Sentinel: see SentinelPrefix.
func isSentinel(v string) bool { return strings.HasPrefix(v, SentinelPrefix) }

// swap rewrites req's Authorization header in place, and reports whether it
// swapped anything. The real token is fetched only once a sentinel has actually
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
func (cr *Credential) swap(ctx context.Context, req *http.Request, host string) (bool, error) {
	h := req.Header.Get("Authorization")
	if h == "" {
		// Nothing to swap. Not an error and not an attach: every client in the
		// sandbox that wants GitHub holds the sentinel, and git retries with it
		// after a 401 because the phase script puts it in the URL's userinfo.
		// ponytail: `go` sends nothing and does not retry, so private modules
		// need an unconditional attach — with a per-host shape, because
		// api.github.com *ignores* Basic (measured: 200, limit 60, no error).
		// The ticket that needs `go` against a private repo adds it.
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
			log.Printf("%s: Basic credential without a colon passed through", host)
			return false, nil
		}
		if !isSentinel(user) && !isSentinel(pass) {
			log.Printf("%s: the sandbox's own Basic credential passed through", host)
			return false, nil
		}
		tok, err := cr.Token(ctx)
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
	tok, err := cr.Token(ctx)
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
// #53 replaces where the token comes from with a minted, per-repository one. This
// path stays after it: it is the seam that keeps the swap itself testable with no
// App, no signer and no KMS.
func StaticGitHub(file string) (*Credential, error) {
	read := func() (string, error) {
		b, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		// Trimmed, because a token that arrives from a file rather than
		// --from-literal carries a trailing newline and GitHub answers 401.
		return strings.TrimSpace(string(b)), nil
	}
	tok, err := read()
	if err != nil {
		return nil, err
	}
	if tok == "" {
		return nil, fmt.Errorf("%s is empty", file)
	}
	if len(tok) != len(Sentinel) {
		log.Printf("creds: github token is %d bytes and the sentinel is %d — "+
			"the swap still works, but Content-Length shifts by %d",
			len(tok), len(Sentinel), len(tok)-len(Sentinel))
	}
	return &Credential{
		Name:  "github",
		Hosts: githubHosts,
		Token: func(context.Context) (string, error) { return read() },
	}, nil
}
