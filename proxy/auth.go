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

	// An absent header spends no token, deliberately: a client that has a
	// credential and did not pre-send it takes the 407 as a challenge and retries,
	// and charging for that would rate-limit the ordinary path. Nothing else is
	// free — what is worth limiting is *guessing*, which means anything that was
	// presented and did not hold up.
	h := r.Header.Get("Proxy-Authorization")
	if h == "" {
		return Run{}, http.StatusProxyAuthRequired, errors.New("no Proxy-Authorization")
	}
	user, pass, ok := proxyBasicAuth(h)
	if !ok {
		// Present but unparseable: nobody reaches this by *not* having pre-sent a
		// credential, so it is not the challenge path and costs a token.
		return fail(http.StatusProxyAuthRequired, errors.New("Proxy-Authorization is not parseable Basic"))
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

	// The failure limiter shapes *rate*, and only after the fact: a token is spent
	// when a request has already failed, so nothing stops N callers entering the
	// resolve wait together on one token's worth of budget. A slot bounds the
	// standing cost of that wait instead. Refused without spending a token — a
	// full resolver is our problem, not evidence about this caller.
	select {
	case s.sem <- struct{}{}:
	default:
		return Run{}, http.StatusTooManyRequests, errors.New("resolver is full")
	}
	// Deferred rather than released on the next line: a resolver that panics must
	// not retire a slot permanently, and everything after it is a struct compare.
	defer func() { <-s.sem }()
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

// How many callers may be inside the resolver at once. It is the wait in Pods.Run
// that this bounds, not the index lookup, which is a map read.
//
// ponytail: a flat cap, not per-IP. One namespace of runs is nowhere near it;
// make it per-IP if one noisy run ever starves the others.
const resolving = 64

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
