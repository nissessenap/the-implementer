package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// A token the length of a real one, so the padding assertion below is not
// self-referential: `ghs_` plus 36.
const testToken = "ghs_" + "0123456789abcdef0123456789abcdef0123"

// The padding is the whole reason the sentinel is not just "proxy-injected": a
// swap that changes the credential's length changes the request's, and
// Content-Length with it.
func TestSentinelIsTokenLength(t *testing.T) {
	if len(Sentinel) != len(testToken) {
		t.Errorf("sentinel is %d bytes, a GitHub token is %d", len(Sentinel), len(testToken))
	}
	if !strings.HasPrefix(Sentinel, SentinelPrefix) {
		t.Error("the padded sentinel no longer starts with the prefix the swap matches on")
	}
}

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// staticCred is a credential handing out tok, counting how often it was asked.
// The count is an assertion of its own: the token must not be fetched for a
// request that carries no sentinel.
func staticCred(tok string, reads *int) *Credential {
	return &Credential{Name: "github", Token: func(context.Context, Run) (string, error) {
		*reads++
		return tok, nil
	}}
}

// Both shapes, as assertions rather than a paragraph. Two of the four shipping
// credential-substituting proxies — OpenAI's `codex` and Anthropic's
// `sandbox-runtime` — silently no-op on git's, because base64 hides the sentinel
// from a substring match. That failure is invisible: no error, no log, just an
// unauthenticated push.
func TestSwapSentinelShapes(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		swapped        bool
	}{
		{"git's clone-URL userinfo", basic("x-access-token", Sentinel), basic("x-access-token", testToken), true},
		{"the unpadded sentinel still swaps", basic("x-access-token", SentinelPrefix), basic("x-access-token", testToken), true},
		{"the sentinel as the username", basic(Sentinel, ""), basic(testToken, ""), true},
		{"gh's token scheme", "token " + Sentinel, "token " + testToken, true},
		{"a Bearer keeps its scheme", "Bearer " + Sentinel, "Bearer " + testToken, true},

		// Untouched, and logged: something the sandbox brought itself. Swallowing
		// it would only hide it.
		{"the sandbox's own basic credential", basic("someone", "hunter2"), basic("someone", "hunter2"), false},
		{"the sandbox's own bearer", "Bearer ghp_theirs", "Bearer ghp_theirs", false},
		{"no credential at all", "", "", false},
		{"unparseable", "Basic", "Basic", false},
		{"undecodable base64", "Basic !!!!", "Basic !!!!", false},
		{"basic with no colon", "Basic " + base64.StdEncoding.EncodeToString([]byte(Sentinel)), "Basic " + base64.StdEncoding.EncodeToString([]byte(Sentinel)), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &http.Request{Header: http.Header{}}
			if tc.in != "" {
				req.Header.Set("Authorization", tc.in)
			}
			reads := 0
			got, err := staticCred(testToken, &reads).swap(context.Background(), req, "github.test", testRun)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.swapped {
				t.Errorf("swapped = %v, want %v", got, tc.swapped)
			}
			if got := req.Header.Get("Authorization"); got != tc.want {
				t.Errorf("Authorization = %q, want %q", got, tc.want)
			}
			// A request with no sentinel in it must not reach for the real
			// token: git's anonymous first leg is exactly that request, and a
			// transient Secret read failure there would refuse it for nothing.
			if want := map[bool]int{true: 1, false: 0}[tc.swapped]; reads != want {
				t.Errorf("fetched the token %d times, want %d", reads, want)
			}
		})
	}
}

// credFor is a per-host switch, and each credential is bound to its own host set.
// The exclusions are the load-bearing half: the certificate is deliberately wider
// than the credential rule, so anything not named here must come out tokenless.
func TestCredsAreBoundPerHost(t *testing.T) {
	dir := t.TempDir()
	issue(t, dir, "github.test", "api.github.test", "objects.github.test", "*.pkg.test")
	certs, err := LoadCerts(dir)
	if err != nil {
		t.Fatal(err)
	}
	gh := &Credential{Name: "github", Hosts: []string{"github.test", "api.github.test"}}

	creds, err := NewCreds(certs, gh)
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]bool{
		"github.test":     true,
		"api.github.test": true,
		// x509 matches hostnames case-insensitively, so this host *is*
		// intercepted. An exact map lookup would hand it no credential and push
		// anonymously with no error — the silent no-op this whole file is about.
		"GitHub.test": true,
		// The same disagreement by a different normalisation: x509 trims the root
		// label, so `CONNECT github.test.:443` is intercepted too.
		"github.test.":     true,
		"API.github.test.": true,
		// Intercepted, and deliberately tokenless: pre-signed blob storage
		// carries its own authorization, and `-docker.pkg.dev` is the same rule.
		"objects.github.test": false,
		"eu-docker.pkg.test":  false,
		"eu-go.pkg.test":      false,
	} {
		if !certs.Intercepts(host) {
			t.Fatalf("%s is not even intercepted — the test proves nothing", host)
		}
		if got := creds.For(host) != nil; got != want {
			t.Errorf("For(%q) has a credential = %v, want %v", host, got, want)
		}
	}

	// Bound with a capitalised host, matched with a lowercase one — the same
	// disagreement from the other side.
	mixed, err := NewCreds(certs, &Credential{Name: "gh", Hosts: []string{"API.github.test"}})
	if err != nil {
		t.Fatal(err)
	}
	if mixed.For("api.github.test") == nil {
		t.Error("a credential bound to a capitalised host does not match the lowercase one")
	}

	// A nil switch is a proxy holding no credentials, which is stages 30 and 40.
	if Creds(nil).For("github.test") != nil {
		t.Error("a nil Creds handed out a credential")
	}
}

// Validated at load, not at request time: a credential bound to a host we cannot
// intercept can never fire, and the symptom months later is an unauthenticated
// request rather than an error.
func TestNewCredsRefusesUnintercepted(t *testing.T) {
	dir := t.TempDir()
	// Exact SANs only, which is what the pattern row below leans on: `*-go.pkg.test`
	// is a name this certificate cannot present.
	issue(t, dir, "github.test", "europe-west1-go.pkg.test")
	certs, err := LoadCerts(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		creds []*Credential
	}{
		{"a host the certificate does not cover", []*Credential{{Name: "gh", Hosts: []string{"github.test", "elsewhere.test"}}}},
		{"no hosts at all", []*Credential{{Name: "gh"}}},
		{"two credentials claiming one host", []*Credential{
			{Name: "a", Hosts: []string{"github.test"}},
			{Name: "b", Hosts: []string{"github.test"}},
		}},
		// A pattern is validated like any other host binding, through one sample
		// host. Only a whole-label wildcard SAN can present these names, so without
		// one the proxy would intercept nothing and hand out nothing — silently.
		{"a pattern the certificate cannot present", []*Credential{{Name: "gar", Hosts: []string{"*-go.pkg.test"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewCreds(certs, tc.creds...); err == nil {
				t.Error("NewCreds accepted it")
			}
		})
	}
}

// The static token is re-read per request, so a rotated Secret needs no restart.
func TestStaticGitHubRereads(t *testing.T) {
	f := t.TempDir() + "/token"
	// With the trailing newline a file-mounted Secret carries, which GitHub
	// answers 401 to if it survives.
	if err := os.WriteFile(f, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := StaticGitHub(f)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Token(context.Background(), testRun); got != testToken {
		t.Errorf("token = %q, want %q", got, testToken)
	}
	if err := os.WriteFile(f, []byte("ghs_rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Token(context.Background(), testRun); got != "ghs_rotated" {
		t.Errorf("a rotated Secret was not picked up: %q", got)
	}
	// Rotated to an empty value, which the *per-request* read has to refuse. Left
	// to the load-time check alone it swaps in nothing and sends
	// `Basic base64(x-access-token:)` — an anonymous request, logged as a swap.
	if err := os.WriteFile(f, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, err := c.Token(context.Background(), testRun); err == nil {
		t.Errorf("an empty rotation was handed out as the token %q", tok)
	}

	if _, err := StaticGitHub(t.TempDir() + "/absent"); err == nil {
		t.Error("StaticGitHub accepted a missing file")
	}
	empty := t.TempDir() + "/empty"
	if err := os.WriteFile(empty, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := StaticGitHub(empty); err == nil {
		t.Error("StaticGitHub accepted an empty file")
	}
}

// swapFixture is the whole path in one object: a client, a real interception, a
// credential, and an upstream that plays GitHub's half of git's dance — anonymous
// requests get a 401 with a Basic challenge, credentialed ones get the credential
// echoed back so a test can assert what actually arrived.
type swapFixture struct {
	tc   *tls.Conn
	br   *bufio.Reader
	seen *string
}

func newSwapFixture(t *testing.T, tweak func(*Credential)) *swapFixture {
	t.Helper()
	dir := t.TempDir()
	pool := issue(t, dir, "github.test")
	certs, err := LoadCerts(dir)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := tls.LoadX509KeyPair(dir+"/tls.crt", dir+"/tls.key")
	if err != nil {
		t.Fatal(err)
	}

	seen := new(string)
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		*seen = auth
		if auth == "" {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, auth)
	}))
	up.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	up.StartTLS()
	t.Cleanup(up.Close)

	tokenFile := t.TempDir() + "/token"
	if err := os.WriteFile(tokenFile, []byte(testToken), 0o600); err != nil {
		t.Fatal(err)
	}
	gh, err := StaticGitHub(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	gh.Hosts = []string{"github.test"}
	if tweak != nil {
		tweak(gh)
	}
	creds, err := NewCreds(certs, gh)
	if err != nil {
		t.Fatal(err)
	}

	s := New(certs, testKey, func(context.Context, string) (Run, error) { return testRun, nil }, creds)
	s.tr.TLSClientConfig.RootCAs = pool
	real := (&net.Dialer{}).DialContext
	s.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return real(ctx, network, strings.TrimPrefix(up.URL, "https://"))
	}
	px := httptest.NewServer(s)
	t.Cleanup(px.Close)

	raw, _ := connect(t, strings.TrimPrefix(px.URL, "http://"), "github.test:443", "")
	tc := tls.Client(raw, &tls.Config{RootCAs: pool, ServerName: "github.test"})
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	return &swapFixture{tc: tc, br: bufio.NewReader(tc), seen: seen}
}

// ask sends one request down the intercepted connection — the same connection
// every time, which is what makes the challenge round-trip below meaningful.
func (f *swapFixture) ask(t *testing.T, auth string) (int, string) {
	t.Helper()
	h := ""
	if auth != "" {
		h = "Authorization: " + auth + "\r\n"
	}
	if _, err := io.WriteString(f.tc, "GET /o/r.git/info/refs?service=git-receive-pack HTTP/1.1\r\n"+
		"Host: github.test\r\n"+h+"\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(f.br, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// The 401 challenge round-trip, end to end through a real interception. `git` does
// not send credentials preemptively when the URL carries no userinfo: it makes an
// anonymous request, takes the 401, and only then retries with HTTP basic — on the
// same connection. A proxy that only looks at the first request of a tunnel breaks
// `git push` while `gh` keeps working.
func TestSwapSurvivesTheChallenge(t *testing.T) {
	f := newSwapFixture(t, nil)

	// 1. git's anonymous first request. The 401 must reach it intact — swallowing
	// the challenge is the other way to break the dance.
	if code, _ := f.ask(t, ""); code != http.StatusUnauthorized {
		t.Fatalf("the anonymous request got %d, want 401 — git never learns to retry", code)
	}
	// 2. its retry, on the same connection, carrying the sentinel from the URL's
	// userinfo. This is the request the swap has to catch.
	code, body := f.ask(t, basic("x-access-token", Sentinel))
	if code != http.StatusOK {
		t.Fatalf("the retry got %d", code)
	}
	if body != basic("x-access-token", testToken) {
		t.Errorf("upstream saw %q — the sentinel survived the swap", body)
	}
	// 3. and gh's shape, third on the same connection.
	if _, body := f.ask(t, "token "+Sentinel); body != "token "+testToken {
		t.Errorf("upstream saw %q, want the real token", body)
	}
}

// A credential we cannot read is a refusal, not a pass-through. Forwarding the
// request without it sends the sentinel to GitHub — a leak, and an anonymous
// request that reads downstream as a rate limit rather than as this failure.
func TestUnreadableCredentialRefuses(t *testing.T) {
	f := newSwapFixture(t, func(cr *Credential) {
		cr.Token = func(context.Context, Run) (string, error) { return "", errors.New("the Secret went away") }
	})
	if code, _ := f.ask(t, basic("x-access-token", Sentinel)); code != http.StatusBadGateway {
		t.Errorf("status %d, want 502", code)
	}
	if strings.Contains(*f.seen, SentinelPrefix) {
		t.Errorf("upstream saw %q — the sentinel was forwarded", *f.seen)
	}
}

// The sentinel exists twice — here and in e2e/lib.sh, which cannot import Go. This
// is the only thing tying the copies together: stage 50 skips itself without a real
// token, so drift would not surface there either.
func TestShellSentinelMatches(t *testing.T) {
	b, err := os.ReadFile("../e2e/lib.sh")
	if err != nil {
		t.Fatal(err)
	}
	if want := "SENTINEL='" + Sentinel + "'"; !bytes.Contains(b, []byte(want)) {
		t.Errorf("e2e/lib.sh no longer carries %s", want)
	}
}

// The plumbing behind the non-negotiable rule: what reaches the credential is the
// run the *proxy authenticated*, and nothing the request can say changes it. The
// fixture asks for a path naming another owner and repository entirely — which is
// exactly what a compromised sandbox would do — and the credential must still be
// asked for `testRun`.
//
// Worth a test of its own because the mint scope is only as good as this: a
// Credential.Token handed the wrong Run mints a token for the wrong repository,
// and the swap itself would look perfectly correct while it happened.
func TestTheSwapSeesTheAuthenticatedRun(t *testing.T) {
	var asked []Run
	f := newSwapFixture(t, func(cr *Credential) {
		inner := cr.Token
		cr.Token = func(ctx context.Context, run Run) (string, error) {
			asked = append(asked, run)
			return inner(ctx, run)
		}
	})

	if _, err := io.WriteString(f.tc,
		"GET /someone-else/their-repo.git/info/refs?service=git-receive-pack HTTP/1.1\r\n"+
			"Host: github.test\r\n"+
			"Authorization: "+basic("x-access-token", Sentinel)+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(f.br, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if len(asked) != 1 || asked[0] != testRun {
		t.Errorf("the credential was asked for %v, want exactly [%v]", asked, testRun)
	}
}
