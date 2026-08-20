package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// A token source that hands out fixed tokens and counts the asks. Not safe for
// concurrent use, and it does not need to be: the production one is ADC's
// ReuseTokenSource, whose own mutex is what makes a shared credential safe.
type fakeTS struct {
	tok  string
	err  error
	asks int
}

func (f *fakeTS) Token() (*oauth2.Token, error) {
	f.asks++
	if f.err != nil {
		return nil, f.err
	}
	return &oauth2.Token{AccessToken: f.tok, Expiry: time.Now().Add(time.Hour)}, nil
}

// The credential is warmed at boot so an unusable Google identity kills the pod
// rather than 502ing the first `pip install` mid-run.
func TestGARWarmsTheTokenAtBoot(t *testing.T) {
	if _, err := gar(&fakeTS{err: errors.New("no metadata server")}); err == nil {
		t.Error("gar accepted a token source that cannot produce a token")
	}
	ts := &fakeTS{tok: "ya29.fake"}
	if _, err := gar(ts); err != nil {
		t.Fatal(err)
	}
	if ts.asks != 1 {
		t.Errorf("asked for %d tokens at boot, want 1", ts.asks)
	}
	// A token source answering with no error *and* no token is unusable too, and
	// `err == nil` is not the whole of "it works": attached, an empty token sends
	// `Authorization: Bearer ` — an anonymous request logged as a credential
	// attached, which is this component's one unacceptable failure.
	if _, err := gar(&fakeTS{tok: ""}); err == nil {
		t.Error("gar accepted a token source handing out an empty access token")
	}
}

// The same refusal per request, which boot cannot cover: a token source that
// works at boot and later answers with an empty token. A 502 rather than an
// anonymous request.
func TestGAREmptyTokenIsRefusedPerRequest(t *testing.T) {
	ts := &fakeTS{tok: "ya29.fake"}
	cred, err := gar(ts)
	if err != nil {
		t.Fatal(err)
	}
	ts.tok = ""
	req := &http.Request{Header: http.Header{}}
	if _, err := cred.swap(context.Background(), req, "us-central1-go.pkg.dev", testRun); err == nil {
		t.Error("an empty access token was not refused")
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want nothing attached", got)
	}
}

// The attach shape, which is the opposite of the sentinel swap's: unconditional,
// and overwriting anything the sandbox sent. `pip` and `go mod download` present no
// credential and do not retry after a 401, so there is nothing to match on.
func TestGARAttachesUnconditionally(t *testing.T) {
	cred, err := gar(&fakeTS{tok: "ya29.fake"})
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{"", "Bearer ya29.theirs", basic("someone", "hunter2")} {
		req := &http.Request{Header: http.Header{}}
		if in != "" {
			req.Header.Set("Authorization", in)
		}
		swapped, err := cred.swap(context.Background(), req, "europe-west1-python.pkg.dev", testRun)
		if err != nil {
			t.Fatal(err)
		}
		if !swapped {
			t.Errorf("Authorization %q: nothing was attached", in)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer ya29.fake" {
			t.Errorf("Authorization %q became %q", in, got)
		}
	}
}

// A token source that stops working is a refusal, the same as an unreadable
// Secret: proxy.serve turns the error into a 502 rather than forwarding the
// request unauthenticated.
func TestGARTokenFailureIsAnError(t *testing.T) {
	ts := &fakeTS{tok: "ya29.fake"}
	cred, err := gar(ts)
	if err != nil {
		t.Fatal(err)
	}
	ts.err = errors.New("the metadata server went away")
	req := &http.Request{Header: http.Header{}}
	if _, err := cred.swap(context.Background(), req, "europe-west1-go.pkg.dev", testRun); err == nil {
		t.Error("a failed token refresh was not an error")
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want nothing attached", got)
	}
}

// The allowlist, host by host, against the certificate the e2e actually issues:
// `*.pkg.dev` is one wildcard SAN, so every Artifact Registry endpoint is
// intercepted and only the two measured ones are credentialed.
//
// `-docker.pkg.dev` is the load-bearing absence. Its challenge/response is
// unmeasured (#28) and an unconditional bearer may suppress the dance, so it must
// come out tokenless — which is a property of the credential rule alone, because
// nothing on the certificate distinguishes it.
func TestGARHostBinding(t *testing.T) {
	dir := t.TempDir()
	issue(t, dir, "github.test", "*.pkg.dev")
	certs, err := LoadCerts(dir)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := gar(&fakeTS{tok: "ya29.fake"})
	if err != nil {
		t.Fatal(err)
	}
	creds, err := NewCreds(certs, cred)
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]bool{
		"europe-west1-python.pkg.dev": true,
		"us-central1-go.pkg.dev":      true,
		// Multi-region endpoints are the same shape with a shorter region.
		"us-python.pkg.dev": true,
		// Case, because x509 intercepts case-insensitively and a switch that
		// does not is an anonymous request with no error.
		"EUROPE-WEST1-PYTHON.pkg.dev": true,

		// Intercepted, deliberately tokenless.
		"europe-west1-docker.pkg.dev": false,
		"europe-west1-npm.pkg.dev":    false,
		"europe-west1-maven.pkg.dev":  false,
		// The bare suffix is not a region, and a pattern that matched it would be
		// claiming a host nobody meant.
		"-go.pkg.dev": false,
		"go.pkg.dev":  false,
		// A pattern must not reach across a label: this is not intercepted by
		// `*.pkg.dev` either, and the two answers have to agree.
		"sneaky.evil-go.pkg.dev": false,
		// The root label, which x509 trims and so the switch must too.
		"europe-west1-python.pkg.dev.": true,
	} {
		if got := creds.For(host) != nil; got != want {
			t.Errorf("For(%q) has a credential = %v, want %v", host, got, want)
		}
	}

	// GitHub is on the same certificate and is not GAR's: the switch is per-host
	// and the GAR pattern must not spill onto it.
	if creds.For("github.test") != nil {
		t.Error("the GAR credential claimed github.test")
	}
}

// The pattern is validated at load like any other host binding: against the
// certificate, and through one sample. A certificate with no `*.pkg.dev` cannot
// present these names, so a proxy bound to them would intercept nothing and hand
// out nothing — silently, months later.
func TestGARRefusedWithoutTheWildcardSAN(t *testing.T) {
	dir := t.TempDir()
	// The exact regional name, which is exactly what crypto/x509 will not let a
	// `*-go.pkg.dev` SAN mean — so a certificate can only cover the pattern via
	// the whole-label wildcard.
	issue(t, dir, "github.test", "europe-west1-go.pkg.dev")
	certs, err := LoadCerts(dir)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := gar(&fakeTS{tok: "ya29.fake"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewCreds(certs, cred); err == nil {
		t.Error("NewCreds bound the GAR pattern to a certificate that cannot present it")
	}
}

// The whole path, faked end to end and so runnable in CI and on a kind cluster
// with no Google credential at all: a real CONNECT, a real interception with the
// proxy's own certificate, a fake token source standing in for Workload Identity,
// and an upstream that reports what actually arrived.
//
// It is the *pair* of assertions that matters. A Python endpoint must come out
// carrying a bearer the sandbox never had, and a Docker endpoint on the same
// wildcard certificate must come out with nothing and its 401 challenge intact —
// the second is the load-bearing one, because nothing on the certificate
// distinguishes the two and only the credential rule does.
//
// There is deliberately no e2e stage doing this against real Artifact Registry
// from kind: that would need a service account key in a Secret, which is the
// long-lived credential Workload Identity exists to delete.
func TestGARThroughARealInterception(t *testing.T) {
	dir := t.TempDir()
	pool := issue(t, dir, "*.pkg.dev")
	certs, err := LoadCerts(dir)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := tls.LoadX509KeyPair(dir+"/tls.crt", dir+"/tls.key")
	if err != nil {
		t.Fatal(err)
	}

	// Artifact Registry's half: unauthenticated requests get a 401 with a
	// challenge — which is exactly what makes the Docker dance unmeasured — and
	// credentialed ones get the credential echoed back.
	seen := make(chan string, 4)
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		seen <- auth
		if auth == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://europe-west1-docker.pkg.dev/v2/token"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, auth)
	}))
	up.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	up.StartTLS()
	defer up.Close()

	cred, err := gar(&fakeTS{tok: "ya29.fake"})
	if err != nil {
		t.Fatal(err)
	}
	creds, err := NewCreds(certs, cred)
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
	defer px.Close()

	// `pip install` from a private repository, and the sandbox holds nothing: no
	// token, no sentinel, no Authorization header at all.
	ask := func(host, path string) (int, string) {
		t.Helper()
		raw, _ := connect(t, strings.TrimPrefix(px.URL, "http://"), host+":443", "")
		tc := tls.Client(raw, &tls.Config{RootCAs: pool, ServerName: host})
		if err := tc.Handshake(); err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		defer tc.Close()
		if _, err := io.WriteString(tc, "GET "+path+" HTTP/1.1\r\nHost: "+host+"\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
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

	code, body := ask("europe-west1-python.pkg.dev", "/my-project/my-repo/simple/private-wheel/")
	if code != http.StatusOK {
		t.Fatalf("the Python endpoint answered %d — the sandbox sent no credential and none was attached", code)
	}
	if body != "Bearer ya29.fake" {
		t.Errorf("Artifact Registry saw %q, want the proxy's own bearer", body)
	}
	if got := <-seen; got != "Bearer ya29.fake" {
		t.Errorf("upstream saw %q", got)
	}

	// The same certificate, the same interception, and deliberately no credential.
	code, _ = ask("europe-west1-docker.pkg.dev", "/v2/")
	if code != http.StatusUnauthorized {
		t.Errorf("the Docker endpoint answered %d, want the 401 challenge through untouched", code)
	}
	if got := <-seen; got != "" {
		t.Errorf("the Docker endpoint was handed %q — its challenge/response is unmeasured (#28)", got)
	}
}
