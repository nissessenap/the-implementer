package proxy

import (
	"crypto/hmac"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/time/rate"
)

// authenticate answers "which run is calling", and is the first thing every
// request meets — before the method dispatch, and so before any hijack. Order is
// load-bearing: the secret is checked statelessly, then the source pod is
// resolved, then the two must agree. Doing it after the hijack means answering
// "200 Connection Established" to a caller we have not authenticated, and there is
// no status code left to refuse it with.
//
// Three answers, kept distinct: a bad credential is 407 (the client may have one
// and be sending it wrong), a credential that does not match the pod it came from
// is 403 (nothing the client can fix, and it is the interesting one — a run
// claiming another run's identity, or an informer cache serving a recycled pod
// IP), and a source that has spent its failure budget is 429 before either check.
func (s *Server) authenticate(r *http.Request) (Run, int, error) {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return Run{}, http.StatusForbidden, errors.New("no source address")
	}
	// Checked before the credential is even parsed, so a brute force costs the
	// attacker its own rate and us nothing. Only failures spend tokens, so a busy
	// sandbox is never throttled by its own traffic.
	lim := s.fails.get(ip)
	if lim.Tokens() < 1 {
		return Run{}, http.StatusTooManyRequests, errors.New("too many auth failures")
	}
	fail := func(code int, err error) (Run, int, error) {
		lim.Allow()
		return Run{}, code, err
	}

	// No credential at all spends no token, deliberately: a client that has one
	// and did not pre-send it takes the 407 as a challenge and retries, and
	// charging for that would rate-limit the ordinary path. What is worth
	// limiting is *guessing*, which means a credential that was presented and
	// did not hold up.
	user, pass, ok := proxyBasicAuth(r.Header.Get("Proxy-Authorization"))
	if !ok {
		return Run{}, http.StatusProxyAuthRequired, errors.New("no Proxy-Authorization: Basic")
	}
	claim, ok := parseRun(user)
	if !ok {
		// %q, because this is attacker-controlled and goes straight to a log.
		return fail(http.StatusProxyAuthRequired, fmt.Errorf("malformed run id %q", user))
	}
	_, want := Cred(s.key, claim)
	if !hmac.Equal([]byte(pass), []byte(want)) {
		return fail(http.StatusProxyAuthRequired, fmt.Errorf("wrong secret for %q", claim))
	}

	got, err := s.resolve(r.Context(), ip)
	if err != nil {
		return fail(http.StatusForbidden, err)
	}
	if got != claim {
		// The UIDs, because the interesting mismatch is the one where the
		// repository matches and the run does not — a recycled pod IP.
		return fail(http.StatusForbidden, fmt.Errorf("claimed %s (%s), pod is %s (%s)",
			claim, claim.UID, got, got.UID))
	}
	return claim, http.StatusOK, nil
}

// proxyBasicAuth is http.Request.BasicAuth for the proxy header, which net/http
// has no accessor for.
func proxyBasicAuth(h string) (user, pass string, ok bool) {
	const prefix = "Basic "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(h[len(prefix):])
	if err != nil {
		return "", "", false
	}
	user, pass, ok = strings.Cut(string(raw), ":")
	return user, pass, ok
}

// failLimiter rate-limits authentication failures per source address. Keyed by IP
// so one misconfigured pod cannot lock the others out.
//
// ponytail: the map is never swept. Its keys are pod IPs in one namespace, so it
// is bounded by the cluster; sweep it if that ever stops being true.
type failLimiter struct {
	mu sync.Mutex
	m  map[string]*rate.Limiter
}

func (f *failLimiter) get(ip string) *rate.Limiter {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.m == nil {
		f.m = map[string]*rate.Limiter{}
	}
	l, ok := f.m[ip]
	if !ok {
		// A run authenticates correctly on every request or none, so the only
		// traffic this shapes is failure. Burst 5 so a misconfigured sandbox
		// still reports its own 407 a few times before it reads as a flood.
		l = rate.NewLimiter(1, 5)
		f.m[ip] = l
	}
	return l
}
