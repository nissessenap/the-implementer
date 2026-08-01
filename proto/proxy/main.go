// PROTOTYPE — throwaway. The credential proxy from issues #33 and #34.
//
// Three jobs, one port, because they are facets of the same question — "can the
// sandbox hold zero credentials?":
//
//	POST /vertex/...  reverse proxy to Google Cloud's Agent Platform (Vertex), with
//	                  a GCP access token attached here. The sandbox sends none. (#33)
//	CONNECT github…   TLS *interception*: terminate with a cert-manager-issued cert
//	                  for the name the sandbox asked for, swap the sentinel it holds
//	                  for the real GitHub token, re-originate upstream. (#34)
//	CONNECT other     plain tunnel, so https_proxy still funnels the rest of the
//	                  sandbox's egress through us. Every host is logged, which
//	                  doubles as an egress inventory for the future allowlist.
//
// Deliberately noisy: every request logs status, time-to-first-byte and total, so
// "what does one more hop cost" has a number rather than an opinion.
package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
)

// vertexHost maps a Vertex location to its hostname. Claude Code puts the
// location in the request path, so the proxy needs no region config of its own —
// it reads whatever CLOUD_ML_REGION the sandbox was given. The three shapes are
// from the Claude Code Vertex docs: global, multi-region (eu/us), and regional.
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

func locationFromPath(p string) string {
	seg := strings.Split(p, "/")
	for i, s := range seg {
		if s == "locations" && i+1 < len(seg) {
			return seg[i+1]
		}
	}
	return "global"
}

// rewriteVertex turns the path Claude Code appended to ANTHROPIC_VERTEX_BASE_URL
// into an upstream path + host. Claude Code may or may not include the API version
// in what it appends, so don't guess — normalise. See main_test.go.
func rewriteVertex(inPath string) (path, host string) {
	path = strings.TrimPrefix(inPath, "/vertex")
	if !strings.HasPrefix(path, "/v1/") {
		path = "/v1" + path
	}
	return path, vertexHost(locationFromPath(path))
}

// timing is carried per-request so ModifyResponse can log TTFB next to the total.
type timing struct {
	start  time.Time
	ttfb   time.Duration
	status int
}

type timingKey struct{}

func main() {
	addr := ":" + envOr("PORT", "8080")
	// A user-ADC credential is billed against a quota project; a service account
	// is not. Set only when the mounted credential needs it.
	quotaProject := os.Getenv("GOOGLE_CLOUD_QUOTA_PROJECT")

	ts, err := google.DefaultTokenSource(context.Background(),
		"https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		log.Fatalf("no ADC: %v (mount a key at GOOGLE_APPLICATION_CREDENTIALS)", err)
	}
	// Warm it once at startup: a credential problem should kill the pod now, not
	// silently 500 the first model call twenty minutes into a run.
	tok, err := ts.Token()
	if err != nil {
		log.Fatalf("ADC token: %v", err)
	}
	log.Printf("ADC ok, token type=%s expires=%s quotaProject=%q",
		tok.TokenType, tok.Expiry.Format(time.RFC3339), quotaProject)

	// Issue #34. Unlike the Vertex hop, this one cannot be dodged with an http://
	// base URL: the sandbox believes it is talking to github.com, so we must
	// present a cert for that name and the sandbox must trust whoever signed it.
	var ic *interceptor
	if dir := os.Getenv("TLS_DIR"); dir != "" {
		ic = newInterceptor(dir, os.Getenv("GH_TOKEN_SENTINEL"), os.Getenv("GH_TOKEN"))
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			p, host := rewriteVertex(pr.In.URL.Path)
			pr.Out.URL.Scheme = "https"
			pr.Out.URL.Host = host
			pr.Out.URL.Path = p
			pr.Out.Host = host

			// THE POINT OF THE WHOLE TICKET: the credential is attached here.
			if t, err := ts.Token(); err == nil {
				pr.Out.Header.Set("Authorization", "Bearer "+t.AccessToken)
			} else {
				log.Printf("token refresh failed: %v", err)
			}
			if quotaProject != "" {
				pr.Out.Header.Set("X-Goog-User-Project", quotaProject)
			}
			// Whatever the sandbox sent as a credential is meaningless upstream.
			pr.Out.Header.Del("X-Api-Key")
		},
		ModifyResponse: func(resp *http.Response) error {
			if t, ok := resp.Request.Context().Value(timingKey{}).(*timing); ok {
				t.ttfb = time.Since(t.start)
				t.status = resp.StatusCode
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("upstream error %s: %v", r.URL.Path, err)
			http.Error(w, err.Error(), http.StatusBadGateway)
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("/vertex/", rp)

	srv := &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect {
				host, _, _ := net.SplitHostPort(r.Host)
				if ic != nil && ic.hosts[host] {
					ic.connect(w, r, host)
				} else {
					tunnel(w, r)
				}
				return
			}
			t := &timing{start: time.Now()}
			r = r.WithContext(context.WithValue(r.Context(), timingKey{}, t))
			mux.ServeHTTP(w, r)
			if r.URL.Path != "/healthz" {
				log.Printf("%s %s -> %d ttfb=%s total=%s",
					r.Method, r.URL.Path, t.status,
					t.ttfb.Round(time.Millisecond),
					time.Since(t.start).Round(time.Millisecond))
			}
		}),
	}
	log.Printf("listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}

// tunnel is the https_proxy half: a byte pipe, no interception, no allowlist.
// ponytail: unrestricted on purpose — egress allowlisting is the termination
// ticket. The log line is the inventory that ticket will start from.
func tunnel(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	up, err := net.DialTimeout("tcp", r.Host, 15*time.Second)
	if err != nil {
		log.Printf("CONNECT %s -> dial failed: %v", r.Host, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer up.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	down, _, err := hj.Hijack()
	if err != nil {
		log.Printf("CONNECT %s -> hijack: %v", r.Host, err)
		return
	}
	defer down.Close()
	if _, err := down.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	log.Printf("CONNECT %s established", r.Host)

	done := make(chan struct{})
	go func() { _, _ = io.Copy(up, down); close(done) }()
	n, _ := io.Copy(down, up)
	<-done
	log.Printf("CONNECT %s closed bytes_down=%d dur=%s", r.Host, n, time.Since(start).Round(time.Millisecond))
}

// ---------------------------------------------------------------- issue #34 ---

// interceptor terminates TLS for the GitHub hosts and swaps the sentinel the
// sandbox holds for the real installation token. The sandbox therefore has no
// GitHub credential either — only a string that is worthless anywhere else.
type interceptor struct {
	cert     tls.Certificate
	hosts    map[string]bool
	sentinel string
	token    string
	tr       *http.Transport
}

func newInterceptor(dir, sentinel, token string) *interceptor {
	cert, err := tls.LoadX509KeyPair(dir+"/tls.crt", dir+"/tls.key")
	if err != nil {
		log.Fatalf("TLS_DIR=%s: %v", dir, err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		log.Fatalf("parse leaf: %v", err)
	}
	// The intercept list IS the certificate's SAN list. A host we cannot present a
	// cert for is a host we must not intercept, so there is nothing else to
	// configure and the two can never drift apart.
	// ponytail: finite SAN list rather than minting leaves per connection from the
	// CA key. Mint on the fly if the set turns out to be open-ended.
	hosts := map[string]bool{}
	for _, d := range leaf.DNSNames {
		hosts[d] = true
	}
	log.Printf("intercepting %v (cert expires %s) sentinel=%t token=%t",
		leaf.DNSNames, leaf.NotAfter.Format(time.RFC3339), sentinel != "", token != "")
	return &interceptor{
		cert: cert, hosts: hosts, sentinel: sentinel, token: token,
		// HTTP/1.1 upstream on purpose: the response is written back to the client
		// verbatim, and "HTTP/2.0 200" is not a valid HTTP/1.1 status line.
		tr: &http.Transport{
			ForceAttemptHTTP2: false,
			TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}},
		},
	}
}

func (ic *interceptor) connect(w http.ResponseWriter, r *http.Request, host string) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "no hijack", http.StatusInternalServerError)
		return
	}
	down, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer down.Close()
	if _, err := down.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}

	tc := tls.Server(down, &tls.Config{
		Certificates: []tls.Certificate{ic.cert},
		NextProtos:   []string{"http/1.1"}, // http.ReadRequest speaks HTTP/1.1 only
	})
	_ = tc.SetDeadline(time.Now().Add(30 * time.Second))
	if err := tc.Handshake(); err != nil {
		// THE ANSWER TO QUESTION ONE: a client that does not trust our CA fails
		// here, by name, so "does git honour SSL_CERT_FILE" is one log line.
		log.Printf("MITM %s handshake REJECTED: %v", host, err)
		return
	}
	_ = tc.SetDeadline(time.Time{})
	log.Printf("MITM %s handshake ok", host)
	ic.serve(tc, host)
}

// serve is a hand-rolled HTTP/1.1 loop rather than httputil.ReverseProxy because
// the connection is already hijacked: there is no Listener to hand to a Server,
// and the one-connection-Listener adapters all leak either a goroutine or the
// connection. Keep-alive framing comes from net/http either way.
func (ic *interceptor) serve(c net.Conn, host string) {
	defer c.Close()
	br := bufio.NewReader(c)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			if err != io.EOF {
				log.Printf("MITM %s read: %v", host, err)
			}
			return
		}
		req.URL.Scheme, req.URL.Host, req.RequestURI = "https", host, ""

		// THE POINT OF THE WHOLE TICKET.
		verdict := swapAuth(req.Header, ic.sentinel, ic.token)

		start := time.Now()
		resp, err := ic.tr.RoundTrip(req)
		if err != nil {
			log.Printf("MITM %s %s %s -> %v", host, req.Method, req.URL.Path, err)
			return
		}
		log.Printf("MITM %s %s %s -> %d auth=%s %s", host, req.Method, req.URL.Path,
			resp.StatusCode, verdict, time.Since(start).Round(time.Millisecond))
		werr := resp.Write(c)
		resp.Body.Close()
		if werr != nil || req.Close || resp.Close {
			return
		}
	}
}

// swapAuth replaces the sentinel with the real GitHub token wherever a client
// puts a credential, and returns a log verdict — never the token.
//
// Two shapes, because a run uses both, and #33's prototype only ever produced the
// second one:
//
//	Authorization: Basic base64(x-access-token:SENTINEL)   git, from the clone URL
//	Authorization: token|Bearer SENTINEL                   gh, from GH_TOKEN
//
// A credential that is not the sentinel is passed through untouched: the sandbox
// having smuggled its own is not something this proxy can fix, and swallowing it
// would only hide it.
func swapAuth(h http.Header, sentinel, token string) string {
	a := h.Get("Authorization")
	switch {
	case a == "":
		return "none"
	case sentinel == "" || token == "":
		return "unconfigured"
	}
	scheme, rest, ok := strings.Cut(a, " ")
	if !ok {
		return "unparsed"
	}
	lower := strings.ToLower(scheme) // verdicts are stable; the header keeps its casing
	switch lower {
	case "basic":
		raw, err := base64.StdEncoding.DecodeString(rest)
		if err != nil {
			return "basic-undecodable"
		}
		user, pass, _ := strings.Cut(string(raw), ":")
		// git accepts the token as either half of the userinfo; handle both.
		if user != sentinel && pass != sentinel {
			return "basic-not-sentinel"
		}
		if user == sentinel {
			user = "x-access-token"
		}
		h.Set("Authorization", "Basic "+
			base64.StdEncoding.EncodeToString([]byte(user+":"+token)))
		return "basic-swapped"
	case "bearer", "token":
		if rest != sentinel {
			return lower + "-not-sentinel"
		}
		h.Set("Authorization", scheme+" "+token)
		return lower + "-swapped"
	}
	return "scheme-" + lower
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
