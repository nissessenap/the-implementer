package proxy

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"regexp"
	"strings"

	"golang.org/x/oauth2"
)

// vertexPrefix is what `ANTHROPIC_VERTEX_BASE_URL` ends in, and the only path this
// proxy answers a non-CONNECT request on. Not a knob: the sandbox is handed the
// whole URL, so a second place to spell the prefix is a second place to get it
// wrong. See ADR 0005.
const vertexPrefix = "/vertex/"

// The model route is the one credential the sandbox reaches over plain HTTP
// rather than through an interception, and it is deliberate: the sandbox holds no
// model credential at all — not blanked, absent — so a cluster-internal plaintext
// hop carries nothing worth stealing. Everything else about it is the same
// pattern as the GAR credential: the proxy's own Google identity, attached here,
// as a bearer the sandbox never sees.

// vertexLocation is what a location may look like, and it is a trust boundary:
// the location comes out of the request path, which the sandbox writes, and it
// decides the upstream host below. Constrained to one DNS label's alphabet so a
// crafted location cannot leave `googleapis.com` — with `europe-west1` allowed
// and `evil.com/x`, `1.2.3.4:80` and `x@y` all refused before anything is dialled.
var vertexLocation = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// vertexModelCall is the **whole** of what this route forwards: one inference call
// on one publisher model. Not "a Vertex path" — that distinction is the difference
// between a model proxy and a remote-code-execution service.
//
// `roles/aiplatform.user`, the grant the chart tells the operator to add, carries
// `customJobs.create`, `pipelineJobs.create` and `endpoints.deploy` along with
// model inference. All of those are POSTs to the same host, under the same
// `/v1/projects/{p}/locations/{l}/` prefix, and every one of them would have
// arrived here with the proxy's own credential attached — so a sandbox that never
// held a Google token could still have started arbitrary containers in the
// operator's project by asking this route nicely. The host confines the credential
// to Vertex; only this shape confines it to *talking to a model*.
//
// The three verbs are what Claude Code calls and nothing else. A model listing or
// a tuning call is a 400, deliberately: widen this when something the agent
// actually needs is refused, and never to "whatever Vertex accepts".
//
// The location is a **capture** and not just a shape check: it is what picks the
// upstream host below, and reading it back out of the path with a second parse is
// how the validated location and the used one come to disagree.
var vertexModelCall = regexp.MustCompile(
	`^/v1/projects/[^/]+/locations/([^/]+)/publishers/[^/]+/models/[^/:]+:(?:rawPredict|streamRawPredict|countTokens)$`)

// vertexHost maps a Vertex location to its hostname. Claude Code puts the
// location in the request path, so the proxy needs no region config of its own —
// it reads back whatever `CLOUD_ML_REGION` the sandbox was given, which is the
// same value the operator set. The three shapes are from the Claude Code Vertex
// docs: global, multi-region (eu/us), and regional.
func vertexHost(loc string) string {
	switch loc {
	case "global":
		return "aiplatform.googleapis.com"
	case "eu", "us":
		return "aiplatform." + loc + ".rep.googleapis.com"
	default:
		return loc + "-aiplatform.googleapis.com"
	}
}

// rewriteVertex turns the path Claude Code appended to `ANTHROPIC_VERTEX_BASE_URL`
// into an upstream path and host. Claude Code may or may not include the API
// version in what it appends, so it is normalised rather than guessed at.
//
// The refusals are the interesting half, and all of them are about what the
// sandbox can name. The credential attached below is a `cloud-platform`-scoped
// Google token: the host is what confines it to Vertex, vertexModelCall is what
// confines it to inference, and the location alphabet is what keeps the host
// honest. A path that does not clean to itself is refused rather than cleaned,
// because `..` is only ever a client trying to reach something the prefix was
// meant to exclude.
//
// ponytail: the project in the path is not checked against one the operator named,
// so a run reaches models in every project the proxy's service account may invoke.
// Bounded by that grant — `roles/aiplatform.user` on one project is the documented
// shape — and closing it properly means the project becoming proxy configuration
// as well as sandbox configuration. Add that when a deployment grants the identity
// more than one project.
func rewriteVertex(inPath string) (upPath, host string, err error) {
	upPath = strings.TrimPrefix(inPath, strings.TrimSuffix(vertexPrefix, "/"))
	// Leading slashes collapsed, and only leading ones: a base URL written with a
	// trailing slash is the operator typo this route is most likely to meet, and
	// it would otherwise 400 every model call of every run. `..` is still refused
	// below rather than cleaned.
	upPath = "/" + strings.TrimLeft(upPath, "/")
	if !strings.HasPrefix(upPath, "/v1/") {
		upPath = "/v1" + upPath
	}
	if upPath != path.Clean(upPath) {
		return "", "", fmt.Errorf("path %q does not clean to itself", inPath)
	}
	// One parse, so the location that is checked below is the location that
	// picks the host. Scanning the path for a `locations` segment instead reads
	// the *project* when a project is called that — and then the host and the
	// path name different regions, silently.
	m := vertexModelCall.FindStringSubmatch(upPath)
	if m == nil {
		return "", "", fmt.Errorf("path %q is not a model inference call", inPath)
	}
	loc := m[1]
	if !vertexLocation.MatchString(loc) {
		return "", "", fmt.Errorf("location %q is not a location", loc)
	}
	return upPath, vertexHost(loc), nil
}

// stubToken is what the e2e seam below attaches instead of a Google one. Named
// for what it is, because it travels upstream and would otherwise be a puzzle in
// a mock's log.
const stubToken = "not-a-real-google-token"

// Vertex is the model half of the proxy: the sandbox's model calls arrive here
// unsigned and leave with the proxy's own Google identity on them. A
// httputil.ReverseProxy and not the interception path, because it needs neither —
// the sandbox is *configured* to come here by base URL, so there is no TLS to
// terminate, no certificate to issue and no name to impersonate.
type Vertex struct {
	ts oauth2.TokenSource

	// upstream is the e2e seam: set, every request goes there instead of to
	// Google. See NewVertex.
	upstream *url.URL

	rp *httputil.ReverseProxy
}

// tokenKey carries the access token from the handler, which can refuse, to the
// Rewrite hook, which cannot. The token is fetched before anything is forwarded
// so that a token source that has stopped working is a 502 rather than an
// unsigned request that reaches Vertex and 401s.
type tokenKey struct{}

// NewVertex builds the model route around a Google token source, warming it here
// so an unusable identity kills the pod at boot rather than 502ing the first model
// call of a run — the same rule, and for the same reason, as the GAR credential.
//
// upstream is the **e2e seam**, and it is one knob rather than two on purpose:
// set, every request is sent there instead of to Google *and* the token source is
// replaced by a worthless stub. A cluster with no metadata server can therefore
// exercise this whole route — chart, environment, rewrite, attach, streaming —
// while the seam remains structurally unusable as a way to point a real
// credential at a host of someone's choosing. Which is also why ts may be nil
// exactly when upstream is set: with the seam on there is no Google identity to
// resolve, and on kind there is none to be had.
func NewVertex(ts oauth2.TokenSource, upstream string) (*Vertex, error) {
	v := &Vertex{ts: ts}
	if upstream != "" {
		u, err := url.Parse(upstream)
		if err != nil {
			return nil, fmt.Errorf("VERTEX_UPSTREAM=%s: %w", upstream, err)
		}
		if u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("VERTEX_UPSTREAM=%s is not an absolute URL", upstream)
		}
		v.upstream, v.ts = u, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: stubToken})
		log.Printf("vertex: THE E2E SEAM IS ON — every model call goes to %s carrying a worthless token, and no Google identity is used", u.Redacted())
	}
	if v.ts == nil {
		return nil, fmt.Errorf("vertex: no token source and no upstream seam")
	}
	// Warmed here for GAR's reason, and refusing the same two answers a request
	// does: a source that errors, and one that answers with no error and no token.
	if _, err := v.token(); err != nil {
		return nil, err
	}
	v.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// pr.Out.URL is a clone of the inbound one, which the handler has
			// already pinned to the upstream it validated. Host follows it: the
			// header and the dial target must name the same host, and Vertex
			// answers 404 on a mismatch.
			pr.Out.Host = pr.Out.URL.Host
			// THE POINT OF THE WHOLE TICKET: the credential is attached here, and
			// the sandbox holds nothing that resembles it.
			// Comma-ok and not an assertion, for what the failure would look
			// like rather than for what could cause it: ServeHTTP always sets
			// this, and nothing else reaches rp. But a panic here is recovered by
			// net/http as a closed connection with no status line at all, which
			// is strictly worse to debug than the 401 an unsigned request earns.
			tok, _ := pr.In.Context().Value(tokenKey{}).(string)
			pr.Out.Header.Set("Authorization", "Bearer "+tok)
			// Whatever the sandbox sent as a model credential is meaningless
			// upstream, and forwarding it would only be one more thing to explain
			// in a Google audit log. `Authorization` is overwritten above;
			// net/http strips the hop-by-hop `Proxy-Authorization` itself.
			pr.Out.Header.Del("X-Api-Key")
			// Not pr.SetXForwarded(): the sandbox's pod IP is nobody's business
			// upstream, and Rewrite without it drops whatever the caller claimed.
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// %q for the reason ServeHTTP gives: a decoded newline in the path.
			log.Printf("vertex %q: %v", r.URL.Path, err)
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
		},
		// No Transport either, and it is not the interception path's: that one
		// forces HTTP/1.1 because it writes responses back onto a hijacked
		// connection verbatim, and caps ResponseHeaderTimeout at 30s. Here
		// http.DefaultTransport is what we want on both counts — HTTP/2 to Google,
		// and no header timeout, because a model turn's first token legitimately
		// takes several seconds and a cap would cut off exactly the slow ones.
		//
		// No FlushInterval, deliberately, and the streaming test is what keeps
		// that honest: ReverseProxy flushes each write as it comes for a
		// `text/event-stream` response and for any response of unknown length,
		// which is every streaming model call. Setting an interval here could only
		// make deltas arrive *later*.
	}
	return v, nil
}

// token asks the source and refuses the two unusable answers, exactly as the GAR
// credential does: an error, and no error with no token — the second being the one
// that would otherwise send `Authorization: Bearer ` and log it as a success.
func (v *Vertex) token() (string, error) {
	t, err := v.ts.Token()
	if err != nil {
		return "", fmt.Errorf("Google token: %w", err)
	}
	if t.AccessToken == "" {
		return "", fmt.Errorf("the Google token source returned an empty access token")
	}
	return t.AccessToken, nil
}

// ServeHTTP validates the path, resolves the credential, and only then forwards.
// Both refusals happen before anything is dialled, because a request forwarded
// without its credential reaches Vertex unsigned, which is a 401 twenty minutes
// into a run rather than an error here.
func (v *Vertex) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Every verb vertexModelCall allows is a POST, so anything else is refused
	// here rather than by Vertex — one less request shape to reason about.
	if r.Method != http.MethodPost {
		http.Error(w, "the model route serves POST", http.StatusMethodNotAllowed)
		return
	}
	upPath, host, err := rewriteVertex(r.URL.Path)
	if err != nil {
		// The error quotes the path, and every log line that carries one must:
		// URL.Path is percent-decoded, so `%0A` in a request target arrives here as
		// a real newline and would otherwise let any caller forge log lines.
		log.Printf("vertex: refused: %v", err)
		http.Error(w, "not a model inference call", http.StatusBadRequest)
		return
	}
	tok, err := v.token()
	if err != nil {
		log.Printf("vertex: no credential to attach: %v", err)
		http.Error(w, "no model credential", http.StatusBadGateway)
		return
	}

	r.URL.Scheme, r.URL.Host, r.URL.Path = "https", host, upPath
	if v.upstream != nil {
		r.URL.Scheme, r.URL.Host = v.upstream.Scheme, v.upstream.Host
	}
	v.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tokenKey{}, tok)))
}
