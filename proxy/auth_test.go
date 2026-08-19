package proxy

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// gate stands up a proxy whose pod lookup returns pod, and returns a function
// that attempts one CONNECT with the given credential and reports the status.
// No upstream: everything under test is refused before anything is dialled.
func gate(t *testing.T, pod Run, podErr error) func(user, pass string) int {
	t.Helper()
	dir := t.TempDir()
	issue(t, dir, "intercepted.test")
	certs, err := LoadCerts(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := New(certs, testKey, func(context.Context, string) (Run, error) { return pod, podErr }, nil)
	// Never reached by a refusal, and a loud failure if one gets through.
	s.dial = func(_ context.Context, _, addr string) (net.Conn, error) {
		return nil, errors.New("dialled " + addr + " for a request that should have been refused")
	}
	px := httptest.NewServer(s)
	t.Cleanup(px.Close)

	return func(user, pass string) int {
		c, err := net.Dial("tcp", strings.TrimPrefix(px.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		req := "CONNECT intercepted.test:443 HTTP/1.1\r\nHost: intercepted.test:443\r\n"
		if user != "" || pass != "" {
			req += "Proxy-Authorization: Basic " +
				base64.StdEncoding.EncodeToString([]byte(user+":"+pass)) + "\r\n"
		}
		if _, err := io.WriteString(c, req+"\r\n"); err != nil {
			t.Fatal(err)
		}
		var code int
		var line string
		if _, err := fmt.Fscanf(c, "HTTP/1.1 %d %s", &code, &line); err != nil {
			t.Fatal(err)
		}
		return code
	}
}

// The whole gate, in the order it must run: the secret first, statelessly, then
// the pod, then the two must agree.
func TestAuthenticate(t *testing.T) {
	user, pass := Cred(testKey, testRun)
	other := Run{Owner: "acme", Repo: "widgets", Issue: "9", UID: "run-2"}
	otherUser, otherPass := Cred(testKey, other)

	for _, tc := range []struct {
		name       string
		pod        Run
		podErr     error
		user, pass string
		want       int
	}{
		{"the run itself", testRun, nil, user, pass, http.StatusOK},
		{"no credential at all", testRun, nil, "", "", http.StatusProxyAuthRequired},
		{"wrong secret", testRun, nil, user, "deadbeef", http.StatusProxyAuthRequired},
		{"secret for another run", testRun, nil, otherUser, otherPass, http.StatusForbidden},
		{"claim that is not a run id", testRun, nil, "acme", pass, http.StatusProxyAuthRequired},
		{"empty field in the claim", Run{}, nil, "acme,,5,run-1", pass, http.StatusProxyAuthRequired},
		// A pod carrying no run annotations — any other workload that reached
		// the ClusterIP — resolves to nothing and is refused.
		{"caller is not a run", Run{}, errors.New("no run identity"), user, pass, http.StatusForbidden},
		// Same repository, stale run: an IP recycled from a finished run, which
		// is the case the secret exists for.
		{"pod is a different run", other, nil, user, pass, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := gate(t, tc.pod, tc.podErr)(tc.user, tc.pass); got != tc.want {
				t.Errorf("status %d, want %d", got, tc.want)
			}
		})
	}
}

// Failures are rate-limited, and a client that simply has not sent its credential
// yet is not: that is the challenge-response every non-pre-sending client makes.
func TestAuthFailuresAreRateLimited(t *testing.T) {
	try := gate(t, testRun, nil)
	user, pass := Cred(testKey, testRun)

	// The challenge path, as often as it likes.
	for range 20 {
		if got := try("", ""); got != http.StatusProxyAuthRequired {
			t.Fatalf("an unauthenticated request got %d, want 407", got)
		}
	}

	var got int
	for i := range 20 {
		if got = try(user, "wrong"); got == http.StatusTooManyRequests {
			break
		}
		if i > 10 {
			t.Fatal("20 wrong secrets in a row were never rate-limited")
		}
	}
	if got != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429", got)
	}
	// And an empty bucket refuses the correct credential too, deliberately: the
	// only source of failures from a given pod IP is that pod, so the run being
	// slowed is the one that produced them. Tokens refill at 1/s.
	if got := try(user, pass); got != http.StatusTooManyRequests {
		t.Errorf("status %d while the bucket was empty, want 429", got)
	}
}

// Golden, because both ends derive this independently — the orchestrator in Go
// and the e2e in a shell — and a silent change of encoding breaks every run.
func TestCredIsStable(t *testing.T) {
	user, pass := Cred([]byte("shared key"), Run{"acme", "widgets", "5", "run-1"})
	if user != "acme,widgets,5,run-1" {
		t.Errorf("user = %q", user)
	}
	// openssl dgst -sha256 -hmac 'shared key', over that same username — which is
	// exactly what e2e/lib.sh computes.
	if want := "ba5e361f6a6f01df8304744f8c1311e8580d1289eaba59104919c4e2b9d5b534"; pass != want {
		t.Errorf("pass = %q, want %q", pass, want)
	}
}
