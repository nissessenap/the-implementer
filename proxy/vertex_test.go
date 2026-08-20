package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The only arithmetic in the model route: turning what Claude Code appends to
// ANTHROPIC_VERTEX_BASE_URL into a real Vertex URL. Get it wrong and every model
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
		// a base URL written with a trailing slash, which is the one operator typo
		// this route is likely to meet: tolerated, because the alternative is a
		// 400 on every model call of every run.
		{"/vertex//v1/projects/p/locations/us/publishers/anthropic/models/m:streamRawPredict",
			"aiplatform.us.rep.googleapis.com",
			"/v1/projects/p/locations/us/publishers/anthropic/models/m:streamRawPredict"},
		// a project literally called `locations` — a legal GCP project id, and the
		// one input that told the shape check and the host apart back when the
		// location was scanned for rather than captured: the host followed the
		// *project* and the path kept the region, silently.
		{"/vertex/projects/locations/locations/europe-west1/publishers/anthropic/models/m:rawPredict",
			"europe-west1-aiplatform.googleapis.com",
			"/v1/projects/locations/locations/europe-west1/publishers/anthropic/models/m:rawPredict"},
		// the third verb Claude Code calls
		{"/vertex/projects/p/locations/global/publishers/anthropic/models/m:countTokens",
			"aiplatform.googleapis.com",
			"/v1/projects/p/locations/global/publishers/anthropic/models/m:countTokens"},
	} {
		p, host, err := rewriteVertex(c.in)
		if err != nil {
			t.Errorf("%s: %v", c.in, err)
			continue
		}
		if host != c.wantHost || p != c.wantPath {
			t.Errorf("%s\n got  %s %s\n want %s %s", c.in, host, p, c.wantHost, c.wantPath)
		}
	}
}

// The refusals, which are the half that is a trust boundary rather than a typo:
// the location comes out of a path the sandbox writes, and it decides the host the
// proxy attaches a Google token to. Everything here must be refused before
// anything is dialled.
func TestVertexRewriteRefusals(t *testing.T) {
	for _, in := range []string{
		// A location that is not a location. Each of these would otherwise be
		// pasted into a hostname.
		"/vertex/projects/p/locations/evil.com/publishers/anthropic/models/m:rawPredict",
		"/vertex/projects/p/locations/1.2.3.4:80/publishers/anthropic/models/m:rawPredict",
		"/vertex/projects/p/locations/x@evil.com/publishers/anthropic/models/m:rawPredict",
		"/vertex/projects/p/locations/EUROPE-WEST1/publishers/anthropic/models/m:rawPredict",
		"/vertex/projects/p/locations//publishers/anthropic/models/m:rawPredict",
		// Not a model *inference* call, which is the refusal that matters most
		// here. `roles/aiplatform.user` — the grant the chart asks the operator for
		// — carries customJobs.create, pipelineJobs.create and endpoints.deploy, so
		// every one of these is a way for a sandbox holding no Google credential to
		// run its own containers in the operator's project on ours. They are all
		// POSTs to the same host under the same prefix; only the shape separates
		// them from a model call.
		"/vertex/projects/p/locations/us-central1/customJobs",
		"/vertex/projects/p/locations/global/pipelineJobs",
		"/vertex/projects/p/locations/us-central1/endpoints/123:predict",
		"/vertex/projects/p/locations/us-central1/tuningJobs",
		"/vertex/projects/p/locations/us-central1/datasets",
		"/vertex/projects/p/locations/global/publishers/anthropic/models",
		"/vertex/projects/p/locations/global/publishers/anthropic/models/m:generateContent",
		// A location is required, not defaulted: it is what picks the host, and
		// Claude Code always sends one.
		"/vertex/projects/p/publishers/anthropic/models/m:countTokens",
		"/vertex/v1/projects",
		"/vertex/v1beta1/projects/p/locations/global/publishers/anthropic/models/m:rawPredict",
		"/vertex/",
		// `..` is refused rather than cleaned: it is only ever a client reaching
		// for something the prefix was meant to exclude.
		"/vertex/projects/p/../../v1/other",
		"/vertex/v1/projects/../foo",
	} {
		if p, host, err := rewriteVertex(in); err == nil {
			t.Errorf("rewriteVertex(%q) = %s %s, want a refusal", in, host, p)
		}
	}
}

// The model credential is warmed at boot, so an unusable Google identity kills the
// pod rather than 502ing the first model call of a run — the same rule as GAR's,
// and it must refuse the same two answers: an error, and no error with no token.
func TestVertexWarmsTheTokenAtBoot(t *testing.T) {
	if _, err := NewVertex(&fakeTS{err: errors.New("no metadata server")}, ""); err == nil {
		t.Error("NewVertex accepted a token source that cannot produce a token")
	}
	if _, err := NewVertex(&fakeTS{tok: ""}, ""); err == nil {
		t.Error("NewVertex accepted a token source handing out an empty access token")
	}
	ts := &fakeTS{tok: "ya29.fake"}
	if _, err := NewVertex(ts, ""); err != nil {
		t.Fatal(err)
	}
	if ts.asks != 1 {
		t.Errorf("asked for %d tokens at boot, want 1", ts.asks)
	}
	// The seam is one knob and not two: with an upstream there is no Google
	// identity to resolve — which is the only reason a cluster with no metadata
	// server can exercise this route at all — and the token it attaches is
	// worthless by construction rather than by configuration.
	v, err := NewVertex(nil, "http://vertex-mock:8080")
	if err != nil {
		t.Fatal(err)
	}
	if tok, _ := v.Token(); tok.AccessToken != stubToken {
		t.Errorf("the seam attached %q, want the stub", tok.AccessToken)
	}
	if _, err := NewVertex(nil, "vertex-mock:8080"); err == nil {
		t.Error("a relative upstream was accepted")
	}
	if _, err := NewVertex(nil, ""); err == nil {
		t.Error("no token source and no seam was accepted")
	}
}

// The whole route, end to end and offline: a real reverse proxy, a fake token
// source standing in for Workload Identity, and an upstream that reports what
// arrived. The sandbox sends no credential — that is the point of the ticket — so
// what the upstream sees is either the proxy's identity or nothing at all.
func TestVertexAttachesTheProxysIdentity(t *testing.T) {
	var gotAuth, gotPath, gotHost, gotFwd string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotHost = r.Header.Get("Authorization"), r.URL.Path, r.Host
		gotFwd = r.Header.Get("X-Forwarded-For")
		if k := r.Header.Get("X-Api-Key"); k != "" {
			t.Errorf("the sandbox's X-Api-Key %q travelled upstream", k)
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer up.Close()

	px := vertexProxy(t, &fakeTS{tok: "ya29.fake"}, up.URL)
	defer px.Close()

	// What the sandbox holds is nothing, and it sends what a skip-auth Claude Code
	// sends: no Authorization, and — belt and braces — an X-Api-Key that must not
	// travel either.
	req, err := http.NewRequest(http.MethodPost,
		px.URL+"/vertex/projects/p/locations/europe-west1/publishers/anthropic/models/m:rawPredict",
		strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", "the-sandbox-has-no-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the model route answered %d", resp.StatusCode)
	}
	if gotAuth != "Bearer ya29.fake" {
		t.Errorf("Vertex saw Authorization %q, want the proxy's own bearer", gotAuth)
	}
	if want := "/v1/projects/p/locations/europe-west1/publishers/anthropic/models/m:rawPredict"; gotPath != want {
		t.Errorf("Vertex saw path %q, want %q", gotPath, want)
	}
	// The Host header follows the dial target: Vertex 404s a mismatch, and with
	// the seam on it is the mock's own name rather than a Google one.
	if gotHost != strings.TrimPrefix(up.URL, "http://") {
		t.Errorf("upstream saw Host %q, want its own %q", gotHost, up.URL)
	}
	if gotFwd != "" {
		t.Errorf("X-Forwarded-For %q went upstream — the sandbox's pod IP is nobody's business there", gotFwd)
	}
}

// SSE survives the hop, which is the one property a reverse proxy can break
// invisibly: buffer the response and every delta still arrives, just all at once
// at the end of a turn. Asserted by *timing* rather than by content, because
// content is what a buffering proxy also gets right.
func TestVertexStreamsDeltasAsTheyCome(t *testing.T) {
	const delta = 150 * time.Millisecond
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := range 3 {
			_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: {\"i\":%d}\n\n", i)
			w.(http.Flusher).Flush()
			time.Sleep(delta)
		}
	}))
	defer up.Close()

	px := vertexProxy(t, &fakeTS{tok: "ya29.fake"}, up.URL)
	defer px.Close()

	resp, err := http.Post(px.URL+"/vertex/projects/p/locations/global/publishers/anthropic/models/m:streamRawPredict",
		"application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	start := time.Now()
	var at []time.Duration
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			at = append(at, time.Since(start))
		}
	}
	if len(at) != 3 {
		t.Fatalf("read %d deltas, want 3", len(at))
	}
	// A buffered response hands all three over within a millisecond of each
	// other, at the end. Half the upstream's own spacing is a wide margin on a
	// loaded CI box and still nowhere near that.
	if gap := at[2] - at[0]; gap < delta {
		t.Errorf("the three deltas arrived %s apart — buffered, not streamed", gap.Round(time.Millisecond))
	}
}

// A token source that worked at boot and stopped is a 502 with nothing forwarded.
// The alternative — forward it unsigned — is a 401 from Vertex twenty minutes into
// a run, reported as a model outage.
func TestVertexRefusesRatherThanForwardUnsigned(t *testing.T) {
	reached := false
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer up.Close()

	ts := &fakeTS{tok: "ya29.fake"}
	px := vertexProxy(t, ts, up.URL)
	defer px.Close()
	ts.err = errors.New("the metadata server went away")

	resp, err := http.Post(px.URL+"/vertex/projects/p/locations/global/publishers/anthropic/models/m:rawPredict",
		"application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("answered %d, want 502", resp.StatusCode)
	}
	if reached {
		t.Error("the request was forwarded to Vertex with no credential on it")
	}
}

// Who may spend the operator's Vertex quota. The model route carries no run
// secret — Claude Code talks to a base URL, not to a proxy, so there is none to
// send — which makes the source pod the whole of the answer, and a caller that
// resolves to no run must get nothing.
func TestVertexRefusesACallerThatIsNotARun(t *testing.T) {
	reached := false
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer up.Close()

	s := New(testCerts(t), testKey, func(context.Context, string) (Run, error) {
		return Run{}, errors.New("no pod has that address")
	}, nil)
	v, err := NewVertex(&fakeTS{tok: "ya29.fake"}, up.URL)
	if err != nil {
		t.Fatal(err)
	}
	s.Vertex = v
	px := httptest.NewServer(s)
	defer px.Close()

	resp, err := http.Post(px.URL+"/vertex/projects/p/locations/global/publishers/anthropic/models/m:rawPredict",
		"application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("answered %d, want 403", resp.StatusCode)
	}
	if reached {
		t.Error("an unidentified caller reached Vertex")
	}
}

// Everything else on the port is what it was before this route existed: a proxy
// that serves CONNECT. Including `/vertex/…` on a deployment with no Vertex
// configured — the route exists because a credential does, not because the binary
// does.
func TestOnlyTheModelRouteIsServed(t *testing.T) {
	certs := testCerts(t)
	resolve := func(context.Context, string) (Run, error) { return testRun, nil }
	for _, tc := range []struct {
		name, path string
		vertex     bool
	}{
		{"no Vertex configured", "/vertex/projects/p/locations/global/publishers/anthropic/models/m:rawPredict", false},
		{"another path entirely", "/healthz", true},
		{"the prefix without a slash", "/vertex", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(certs, testKey, resolve, nil)
			if tc.vertex {
				v, err := NewVertex(&fakeTS{tok: "ya29.fake"}, "http://unreachable.invalid")
				if err != nil {
					t.Fatal(err)
				}
				s.Vertex = v
			}
			px := httptest.NewServer(s)
			defer px.Close()
			resp, err := http.Get(px.URL + tc.path)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotImplemented {
				t.Errorf("%s answered %d, want 501", tc.path, resp.StatusCode)
			}
		})
	}
}

// Every verb the route forwards is a POST, so anything else is refused here
// rather than by Vertex.
func TestVertexServesPOSTOnly(t *testing.T) {
	reached := false
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer up.Close()
	px := vertexProxy(t, &fakeTS{tok: "ya29.fake"}, up.URL)
	defer px.Close()

	resp, err := http.Get(px.URL + "/vertex/projects/p/locations/global/publishers/anthropic/models/m:rawPredict")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("a GET answered %d, want 405", resp.StatusCode)
	}
	if reached {
		t.Error("a GET reached Vertex")
	}
}

// The model route is the base URL the sandbox was handed, in origin form. The
// sandbox also has `https_proxy` naming this port, so it can send the *proxied*
// absolute form — and a request it addressed to another host must not be quietly
// rerouted into Vertex because its path happens to start with the prefix.
func TestVertexIgnoresProxiedAbsoluteForm(t *testing.T) {
	reached := false
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer up.Close()
	px := vertexProxy(t, &fakeTS{tok: "ya29.fake"}, up.URL)
	defer px.Close()

	c, err := net.Dial("tcp", strings.TrimPrefix(px.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// The absolute form, exactly as a client with http_proxy set writes it.
	if _, err := io.WriteString(c, "POST http://elsewhere.test/vertex/projects/p/locations/global/publishers/anthropic/models/m:rawPredict HTTP/1.1\r\nHost: elsewhere.test\r\nContent-Length: 2\r\n\r\n{}"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("the absolute form answered %d, want 501", resp.StatusCode)
	}
	if reached {
		t.Error("a request addressed to another host was rerouted into Vertex")
	}
}

// vertexProxy is a proxy whose model route is on and whose caller always resolves
// to a run: the fixtures above are about the credential and the stream, and the
// identity half has its own test.
func vertexProxy(t *testing.T, ts *fakeTS, upstream string) *httptest.Server {
	t.Helper()
	// Built without the seam and then pointed at the local upstream, because the
	// seam replaces the token source — which is the property the boot test pins,
	// and here the fake source is what lets these assert on a token at all.
	v, err := NewVertex(ts, "")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	v.upstream = u
	s := New(testCerts(t), testKey, func(context.Context, string) (Run, error) { return testRun, nil }, nil)
	s.Vertex = v
	return httptest.NewServer(s)
}

// testCerts is a certificate for a proxy whose test is not about interception at
// all: the model route needs none, and New wants one.
func testCerts(t *testing.T) *Certs {
	t.Helper()
	dir := t.TempDir()
	issue(t, dir, "github.test")
	certs, err := LoadCerts(dir)
	if err != nil {
		t.Fatal(err)
	}
	return certs
}
