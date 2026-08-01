package main

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

// The only non-trivial logic in the proxy is turning what Claude Code appends to
// ANTHROPIC_VERTEX_BASE_URL into a real Vertex URL. Get that wrong and every model
// call 404s from inside a cluster, which is an expensive way to find a typo.
func TestVertexRewrite(t *testing.T) {
	for _, c := range []struct{ in, wantHost, wantPath string }{
		// what the docs say Claude Code appends (no /v1)
		{"/vertex/projects/p/locations/global/publishers/anthropic/models/claude-opus-5:streamRawPredict",
			"aiplatform.googleapis.com",
			"/v1/projects/p/locations/global/publishers/anthropic/models/claude-opus-5:streamRawPredict"},
		// ...and if it already includes it, don't double up
		{"/vertex/v1/projects/p/locations/europe-west1/publishers/anthropic/models/m:rawPredict",
			"europe-west1-aiplatform.googleapis.com",
			"/v1/projects/p/locations/europe-west1/publishers/anthropic/models/m:rawPredict"},
		// multi-region gets the .rep. host, not the {loc}- prefix
		{"/vertex/projects/p/locations/eu/publishers/anthropic/models/m:streamRawPredict",
			"aiplatform.eu.rep.googleapis.com",
			"/v1/projects/p/locations/eu/publishers/anthropic/models/m:streamRawPredict"},
	} {
		p, host := rewriteVertex(c.in)
		if host != c.wantHost || p != c.wantPath {
			t.Errorf("%s\n got  %s %s\n want %s %s", c.in, host, p, c.wantHost, c.wantPath)
		}
	}
}

// The other non-trivial bit: the sentinel swap (issue #34). It is the only place
// the real GitHub token exists, and getting it wrong is either a broken run or a
// leaked credential — neither of which a cluster is a pleasant place to discover.
func TestSwapAuth(t *testing.T) {
	const sent, tok = "proxy-injected", "ghs_REALTOKEN"
	basic := func(u, p string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(u+":"+p))
	}
	for _, c := range []struct{ name, in, wantVerdict, wantOut string }{
		// git, from `git clone https://x-access-token:SENTINEL@github.com/...`
		{"git basic password", basic("x-access-token", sent), "basic-swapped", basic("x-access-token", tok)},
		// git also accepts the token as the username
		{"git basic username", basic(sent, ""), "basic-swapped", basic("x-access-token", tok)},
		// gh, from GH_TOKEN
		{"gh token", "token " + sent, "token-swapped", "token " + tok},
		{"bearer", "Bearer " + sent, "bearer-swapped", "Bearer " + tok},
		// anything that is not the sentinel passes through untouched
		{"foreign basic", basic("x-access-token", "ghp_SMUGGLED"), "basic-not-sentinel", basic("x-access-token", "ghp_SMUGGLED")},
		{"foreign token", "token ghp_SMUGGLED", "token-not-sentinel", "token ghp_SMUGGLED"},
		{"absent", "", "none", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			if c.in != "" {
				h.Set("Authorization", c.in)
			}
			if got := swapAuth(h, sent, tok); got != c.wantVerdict {
				t.Errorf("verdict = %q, want %q", got, c.wantVerdict)
			}
			if got := h.Get("Authorization"); got != c.wantOut {
				t.Errorf("header = %q, want %q", got, c.wantOut)
			}
		})
	}

	// The sentinel must never reach GitHub silently, and the real token must never
	// be logged. Both are properties of the verdict string, so assert on it.
	h := http.Header{"Authorization": []string{"token " + sent}}
	if v := swapAuth(h, sent, tok); strings.Contains(v, tok) || strings.Contains(v, sent) {
		t.Errorf("verdict %q leaks a credential", v)
	}
}

// Issue #34. The cert carries `*.pkg.dev` so the proxy needs no region config, and
// the wildcard has to match the way crypto/x509 matches it or the proxy and its
// clients disagree about which hosts it can serve.
func TestIntercepts(t *testing.T) {
	ic := &interceptor{hosts: map[string]bool{"github.com": true, "*.pkg.dev": true}}
	for _, c := range []struct {
		host string
		want bool
	}{
		{"github.com", true},
		{"europe-north1-go.pkg.dev", true},
		{"us-central1-docker.pkg.dev", true}, // on the cert, but see credFor
		{"pkg.dev", false},                   // a wildcard does not match the bare name
		{"a.b.pkg.dev", false},               // ...nor more than one label
		{"pkg.go.dev", false},                // the public docs site is a different host
		{"proxy.golang.org", false},
	} {
		if got := ic.intercepts(c.host); got != c.want {
			t.Errorf("intercepts(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// Which credential a host gets. The `-docker.pkg.dev` row is the load-bearing one:
// the cert is deliberately wider than the credential rule, and OCI auth is a
// challenge/response dance a header injection would get wrong.
func TestCredFor(t *testing.T) {
	for host, want := range map[string]string{
		"github.com":                 "github",
		"api.github.com":             "github",
		"raw.githubusercontent.com":  "github",
		"europe-north1-go.pkg.dev":   "gar",
		"us-central1-docker.pkg.dev": "none",
	} {
		if got := credFor(host); got != want {
			t.Errorf("credFor(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestAttachGCP(t *testing.T) {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "ya29.FAKE", TokenType: "Bearer"})

	h := http.Header{}
	if v := attachGCP(h, ts); v != "gcp-attached" {
		t.Errorf("verdict = %q, want gcp-attached", v)
	}
	if got := h.Get("Authorization"); got != "Bearer ya29.FAKE" {
		t.Errorf("header = %q", got)
	}

	// Unlike swapAuth, a credential the sandbox smuggled in is overwritten, not
	// passed through — but it is still reported.
	h = http.Header{"Authorization": []string{"Bearer ya29.SMUGGLED"}}
	if v := attachGCP(h, ts); v != "gcp-replaced" {
		t.Errorf("verdict = %q, want gcp-replaced", v)
	}

	// The verdict is an audit line, so it must never carry the token.
	if v := attachGCP(http.Header{}, ts); strings.Contains(v, "ya29") {
		t.Errorf("verdict %q leaks a credential", v)
	}
	if v := attachGCP(http.Header{}, nil); v != "unconfigured" {
		t.Errorf("verdict = %q, want unconfigured", v)
	}
}
