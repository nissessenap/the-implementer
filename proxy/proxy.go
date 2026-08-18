// Package proxy is the credential proxy: the one component that holds
// credentials, so the sandbox can hold none. See ADR 0005.
//
// # The one rule
//
// This package, and cmd/proxy, import nothing from the orchestrator. That rule is
// the entire cost of extracting the proxy to its own repository later, which is
// the plan if it works out. Shared types are worth less than the seam.
//
// What is here so far is the skeleton: an https_proxy that terminates TLS for the
// hosts its own certificate names and tunnels everything else opaquely. The
// credentials it exists to attach land on top of it — the sentinel swap (#52),
// GAR (#54) and Vertex (#55) — as is caller authentication (#51).
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
	"time"
)

const (
	dialTimeout      = 15 * time.Second
	handshakeTimeout = 30 * time.Second
)

// Server is the https_proxy. One handler, and it serves exactly one method:
// CONNECT is either intercepted or tunnelled, and nothing else is served at all.
type Server struct {
	certs *Certs

	// dial reaches an upstream. A field so a test can assert *what address the
	// proxy dialled* — the pin below is a one-line bug otherwise.
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
	tr   *http.Transport
}

// New builds the proxy around a certificate. Everything it will later attach a
// credential to hangs off this — the certificate is what decides which hosts it
// may terminate at all.
func New(certs *Certs) *Server {
	s := &Server{
		certs: certs,
		dial:  (&net.Dialer{Timeout: dialTimeout}).DialContext,
	}
	s.tr = &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return s.dial(ctx, network, addr)
		},
		// HTTP/1.1 upstream on purpose: the response is written back to the
		// client verbatim, and "HTTP/2.0 200" is not a valid HTTP/1.1 status line.
		ForceAttemptHTTP2: false,
		TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}},
	}
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		// r.Host is the CONNECT authority, and from here on it is the only
		// thing that decides where the bytes go.
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			http.Error(w, "CONNECT needs host:port", http.StatusBadRequest)
			return
		}
		if s.certs.Intercepts(host) {
			s.intercept(w, r)
		} else {
			s.tunnel(w, r)
		}
		return
	}
	http.Error(w, "this proxy serves CONNECT", http.StatusNotImplemented)
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

	done := make(chan struct{})
	go func() { _, _ = io.Copy(up, down); close(done) }()
	_, _ = io.Copy(down, up)
	<-done
}

// intercept terminates TLS with our own certificate for the name the client asked
// for. The client only gets here because it trusts our CA (the trust bundle); one
// that does not fails the handshake, by name, in a single log line.
func (s *Server) intercept(w http.ResponseWriter, r *http.Request) {
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
	s.serve(tc, authority)
}

// serve is a hand-rolled HTTP/1.1 loop rather than httputil.ReverseProxy because
// the connection is already hijacked: there is no Listener to hand to a Server,
// and the one-connection-Listener adapters all leak either a goroutine or the
// connection. Keep-alive framing still comes from net/http.
func (s *Server) serve(c net.Conn, authority string) {
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
		if port == "443" {
			req.Host = host // the default port belongs in the dial, not the header
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
