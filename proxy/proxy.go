// Package proxy is the credential proxy: the one component that holds
// credentials, so the sandbox can hold none. See ADR 0005.
//
// # The one rule
//
// This package, and cmd/proxy, import nothing from the orchestrator. That rule is
// the entire cost of extracting the proxy to its own repository later, which is
// the plan if it works out. Shared types are worth less than the seam.
//
// What is here so far: an https_proxy that authenticates the run calling it,
// resolves that run's identity from Kubernetes, terminates TLS for the hosts its
// own certificate names while tunnelling everything else opaquely, and attaches
// each host's credential — swapping the sentinel for a GitHub token, and putting
// the proxy's own Google identity on Artifact Registry.
//
// Plus one route that is not a CONNECT at all: the model calls, which arrive as
// ordinary requests to `ANTHROPIC_VERTEX_BASE_URL` and are reverse-proxied to
// Vertex with the same Google identity attached. It needs no interception because
// the sandbox is *configured* to come here, so there is no name to impersonate —
// and no credential to leak on a plaintext in-cluster hop, because the sandbox
// holds none. See vertex.go.
//
// Egress is unrestricted on purpose: the allowlist and the NetworkPolicy are
// post-MVP with #16. Every CONNECT is logged, which is the inventory that ticket
// starts from.
package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	dialTimeout      = 15 * time.Second
	handshakeTimeout = 30 * time.Second

	// A stalling upstream must not hold an inner request forever. The request
	// http.ReadRequest hands us carries no context, so context.Background() is the
	// only deadline it would otherwise have.
	responseHeaderTimeout = 30 * time.Second

	// Bounded rather than net/http's zero value, which is "keep idle connections
	// forever": the intercept list is small and fixed, so a pool that never
	// expires is only somewhere for connections to accumulate.
	idleConnTimeout = 90 * time.Second
)

// Server is the https_proxy. One handler, and it serves exactly one method:
// CONNECT is either intercepted or tunnelled, and nothing else is served at all.
type Server struct {
	certs *Certs

	// Vertex, when set, serves the model route: the one non-CONNECT request this
	// proxy answers, and the only credential the sandbox reaches by base URL
	// rather than through an interception. Nil is a proxy that serves CONNECT and
	// nothing else, which is every deployment with no Vertex configured — and
	// every test that is not about the model route.
	//
	// A field set at boot rather than a New parameter: it is one of four optional
	// credentials, and threading each through a constructor is how a signature
	// grows a nil argument per ticket.
	Vertex http.Handler

	// credFor: which credential, if any, an intercepted host is due. Nil is a
	// proxy that holds none, which is every test that is not about credentials.
	creds Creds

	// The shared key the run secret is derived from, and the source-IP to run
	// lookup it must agree with. A field rather than a *Pods, so a test needs no
	// apiserver — and so the two identity halves stay swappable independently.
	key     []byte
	resolve func(ctx context.Context, ip string) (Run, error)
	fails   failLimiter

	// dial reaches an upstream. A field so a test can assert *what address the
	// proxy dialled* — the pin below is a one-line bug otherwise.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	tr   *http.Transport
}

// New builds the proxy around a certificate, the shared run key, a resolver from
// source IP to run, and the credentials it attaches. The certificate decides which
// hosts it may terminate, the key and resolver decide who may ask it to, and creds
// decides what each intercepted host is handed.
func New(certs *Certs, key []byte, resolve func(ctx context.Context, ip string) (Run, error), creds Creds) *Server {
	s := &Server{
		certs:   certs,
		creds:   creds,
		key:     key,
		resolve: resolve,
		dial:    (&net.Dialer{Timeout: dialTimeout}).DialContext,
	}
	s.tr = &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return s.dial(ctx, network, addr)
		},
		// HTTP/1.1 upstream on purpose: the response is written back to the
		// client verbatim, and "HTTP/2.0 200" is not a valid HTTP/1.1 status line.
		ForceAttemptHTTP2:     false,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}},
		ResponseHeaderTimeout: responseHeaderTimeout,
		IdleConnTimeout:       idleConnTimeout,
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		s.model(w, r)
		return
	}

	// First, and before the hijack: past that point the client has been told
	// "200 Connection Established" and there is no way left to refuse it.
	run, code, err := s.authenticate(r)
	if err != nil {
		log.Printf("%s %s %s refused: %v", r.RemoteAddr, r.Method, r.Host, err)
		if code == http.StatusProxyAuthRequired {
			w.Header().Set("Proxy-Authenticate", `Basic realm="the-implementer"`)
		}
		http.Error(w, http.StatusText(code), code)
		return
	}

	// r.Host is the CONNECT authority, and from here on it is the only thing that
	// decides where the bytes go.
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, "CONNECT needs host:port", http.StatusBadRequest)
		return
	}
	// The run is logged with the destination because that pairing is what the
	// credential mints against: this run's repository, never the URL's.
	log.Printf("CONNECT %s for %s", r.Host, run)
	if s.certs.Intercepts(host) {
		s.intercept(w, r, run)
	} else {
		s.tunnel(w, r)
	}
}

// model serves the one route that is not a CONNECT: the sandbox's model calls,
// which arrive as ordinary requests to `ANTHROPIC_VERTEX_BASE_URL` and leave with
// the proxy's own Google identity attached. Anything else is a 501, as it was
// before this route existed.
//
// Identified by source pod and not by the run secret, which is a real weakening
// and is bounded: see identify.
func (s *Server) model(w http.ResponseWriter, r *http.Request) {
	// Origin-form only: `r.URL.Host` is set when a client sends the proxied
	// absolute form — `POST http://elsewhere/vertex/…`, which the sandbox can do
	// because `https_proxy` names this port. Serving it would silently reroute a
	// request the client addressed to another host into Vertex. The model route is
	// the base URL the sandbox was handed, and nothing else.
	if s.Vertex == nil || r.URL.Host != "" || !strings.HasPrefix(r.URL.Path, vertexPrefix) {
		http.Error(w, "this proxy serves CONNECT", http.StatusNotImplemented)
		return
	}
	run, code, err := s.identify(r)
	if err != nil {
		// %q on the path, and it is not cosmetic: URL.Path is percent-decoded, so
		// `%0A` in a request target arrives as a real newline — and this line is
		// written *before* the caller is identified, so anything in the cluster
		// could otherwise forge proxy log lines. Same rule as authenticate's.
		log.Printf("%s %s %q refused: %v", r.RemoteAddr, r.Method, r.URL.Path, err)
		http.Error(w, http.StatusText(code), code)
		return
	}
	log.Printf("%s %q for %s", r.Method, r.URL.Path, run)
	s.Vertex.ServeHTTP(w, r)
}

// bufConn is a hijacked connection that reads through the buffer net/http parsed
// the CONNECT with. Discarding that buffer — the obvious spelling — silently eats
// whatever the client pipelined behind the request without waiting for the 200,
// which for a TLS client is its ClientHello.
type bufConn struct {
	net.Conn
	r io.Reader
}

func (c bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// hijack answers the CONNECT and hands back the client connection.
func hijack(w http.ResponseWriter) (net.Conn, bool) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return nil, false
	}
	c, rw, err := hj.Hijack()
	if err != nil {
		log.Printf("hijack: %v", err)
		return nil, false
	}
	if _, err := c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		c.Close()
		return nil, false
	}
	return bufConn{Conn: c, r: rw.Reader}, true
}

// tunnel is the opaque half: a byte pipe for every host we hold no certificate
// for. The sandbox's whole egress still funnels through here, so the log line is
// an egress inventory even where the payload is none of our business.
func (s *Server) tunnel(w http.ResponseWriter, r *http.Request) {
	up, err := s.dial(r.Context(), "tcp", r.Host)
	if err != nil {
		log.Printf("CONNECT %s dial: %v", r.Host, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer up.Close()

	down, ok := hijack(w)
	if !ok {
		return
	}
	defer down.Close()
	log.Printf("CONNECT %s tunnelled", r.Host)

	// Whichever direction finishes first closes the connection the *other* one is
	// blocked reading, so a half-close ends the pair instead of parking it: the
	// goroutine reads down, so it closes up; this side reads up, so it closes down.
	// Leaving both to the defers above deadlocks on <-done whenever the upstream
	// hangs up first and the client does not — one goroutine and two file
	// descriptors, held for as long as the client cares to hold them.
	done := make(chan struct{})
	go func() { _, _ = io.Copy(up, down); up.Close(); close(done) }()
	_, _ = io.Copy(down, up)
	down.Close()
	<-done
}

// intercept terminates TLS with our own certificate for the name the client asked
// for. The client only gets here because it trusts our CA (the trust bundle); one
// that does not fails the handshake, by name, in a single log line.
func (s *Server) intercept(w http.ResponseWriter, r *http.Request, run Run) {
	authority := r.Host
	down, ok := hijack(w)
	if !ok {
		return
	}
	defer down.Close()

	tc := tls.Server(down, &tls.Config{
		// Per-handshake, so a renewed Secret is served without a pod restart.
		GetCertificate: s.certs.GetCertificate,
		NextProtos:     []string{"http/1.1"}, // http.ReadRequest speaks HTTP/1.1 only
	})
	_ = tc.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := tc.Handshake(); err != nil {
		log.Printf("CONNECT %s handshake rejected: %v", authority, err)
		return
	}
	_ = tc.SetDeadline(time.Time{})
	log.Printf("CONNECT %s intercepted", authority)
	s.serve(tc, authority, run)
}

// serve is a hand-rolled HTTP/1.1 loop rather than httputil.ReverseProxy because
// the connection is already hijacked: there is no Listener to hand to a Server,
// and the one-connection-Listener adapters all leak either a goroutine or the
// connection. Keep-alive framing still comes from net/http.
func (s *Server) serve(c net.Conn, authority string, run Run) {
	defer c.Close()
	host, port, _ := net.SplitHostPort(authority)
	br := bufio.NewReader(c)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			if err != io.EOF {
				log.Printf("%s read: %v", authority, err)
			}
			return
		}

		// THE PIN. The upstream target comes from the CONNECT authority and
		// never from the inner request, whose Host header the sandbox controls
		// and could otherwise use to redirect an intercepted tunnel — reaching a
		// host of its choosing with whatever credential this host is due. Both
		// fields, because URL.Host is the dial target and Host is the header.
		req.URL.Scheme, req.URL.Host, req.RequestURI = "https", authority, ""
		req.Host = authority
		// Ours, and it stops here: the run credential authenticates the caller to
		// this proxy and has no business travelling upstream. net/http only *sets*
		// this header for its own proxied requests and never strips one a client
		// put there itself. Singled out deliberately — the rest of the hop-by-hop
		// set is either harmless or handled by req.Close/resp.Close below.
		req.Header.Del("Proxy-Authorization")
		if port == "443" {
			req.Host = host // the default port belongs in the dial, not the header
		}

		// The swap, and it hangs off `host` — the CONNECT authority pinned above,
		// never the inner request — so the credential a host is due cannot be
		// claimed by naming that host in a header.
		if cred := s.creds.For(host); cred != nil {
			// `run` is the authenticated caller from ServeHTTP, and it is what
			// the credential mints against: this run's repository, never the one
			// the URL — or the pinned authority — names.
			switch swapped, err := cred.swap(req.Context(), req, host, run); {
			case err != nil:
				// Refused rather than forwarded: a request that goes on without
				// its credential sends the sentinel to GitHub, which is both a
				// leak and an anonymous request that looks like a rate limit.
				log.Printf("%s: credential %q unavailable: %v", authority, cred.Name, err)
				// Hand-written: the connection is past net/http's reach, so a
				// status line is all that is left to refuse a request with.
				_, _ = io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")
				return
			case swapped:
				// "attached" covers both shapes: a sentinel swapped for the real
				// thing, and a GAR bearer put on a request that carried nothing.
				log.Printf("%s: attached the %q credential", host, cred.Name)
			}
		}

		start := time.Now()
		resp, err := s.tr.RoundTrip(req)
		if err != nil {
			log.Printf("%s %s %s -> %v", authority, req.Method, req.URL.Path, err)
			return
		}
		log.Printf("%s %s %s -> %d %s", authority, req.Method, req.URL.Path,
			resp.StatusCode, time.Since(start).Round(time.Millisecond))
		werr := resp.Write(c)
		resp.Body.Close()
		if werr != nil || req.Close || resp.Close {
			return
		}
	}
}
