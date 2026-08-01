// PROTOTYPE — throwaway. The credential proxy from issue #33.
//
// Two jobs, one port, because they are two halves of the same question — "can the
// sandbox hold zero credentials?":
//
//	POST /vertex/...  reverse proxy to Google Cloud's Agent Platform (Vertex), with
//	                  a GCP access token attached here. The sandbox sends none.
//	CONNECT host:443  plain tunnel, so https_proxy in the sandbox funnels git/gh/go
//	                  egress through us. No TLS interception — that is the
//	                  termination ticket. Every host is logged, which doubles as an
//	                  egress inventory for the future allowlist.
//
// Deliberately noisy: every request logs status, time-to-first-byte and total, so
// "what does one more hop cost" has a number rather than an opinion.
package main

import (
	"context"
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
				tunnel(w, r)
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

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
