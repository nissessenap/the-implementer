package proxy

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// issue writes a freshly signed tls.crt/tls.key for sans into dir and returns a
// pool holding the CA that signed it — the same shape cert-manager mounts, so the
// tests exercise the real load path rather than a hand-built tls.Certificate.
func issue(t *testing.T, dir string, sans ...string) *x509.CertPool {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: sans[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     sans,
	}, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, typ string, der []byte) {
		if err := os.WriteFile(dir+"/"+name, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("tls.key", "PRIVATE KEY", keyDER)
	write("tls.crt", "CERTIFICATE", leafDER)
	// Explicit, so a reload test does not depend on two writes landing in
	// different filesystem timestamp ticks.
	mt := time.Now().Add(time.Duration(len(sans)) * time.Second)
	if err := os.Chtimes(dir+"/tls.crt", mt, mt); err != nil {
		t.Fatal(err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return pool
}

// The SAN list is the intercept list, and a renewal must move both. The wildcard
// cases are x509's rule, not ours — one leftmost label, nothing deeper.
func TestCertsIntercepts(t *testing.T) {
	dir := t.TempDir()
	issue(t, dir, "a.test", "*.pkg.test")
	c, err := LoadCerts(dir)
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]bool{
		"a.test":           true,
		"eu-go.pkg.test":   true,
		"deep.x.pkg.test":  false,
		"pkg.test":         false,
		"b.test":           false,
		"nota.test":        false,
		"a.test.evil.test": false,
	} {
		if got := c.Intercepts(host); got != want {
			t.Errorf("Intercepts(%q) = %v, want %v", host, got, want)
		}
	}

	// The renewal. cert-manager rewrites the Secret, the kubelet refreshes the
	// mount, and both the served certificate and the intercept list must follow
	// with no restart.
	before, err := c.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	issue(t, dir, "b.test", "c.test", "d.test")
	if c.Intercepts("a.test") || !c.Intercepts("b.test") {
		t.Error("intercept list did not follow the renewed certificate")
	}
	after, err := c.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if before.Leaf.Equal(after.Leaf) {
		t.Error("GetCertificate still serves the boot-time certificate")
	}
}

// A cert that cannot be read at startup is fatal; one that breaks later is not,
// because a renewal caught mid-write must not take the proxy down.
func TestCertsLoadFails(t *testing.T) {
	if _, err := LoadCerts(t.TempDir()); err == nil {
		t.Fatal("LoadCerts on an empty dir should fail")
	}

	dir := t.TempDir()
	issue(t, dir, "a.test")
	c, err := LoadCerts(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Half-written, as a reader racing cert-manager's rewrite would see it.
	if err := os.WriteFile(dir+"/tls.crt", []byte("-----BEGIN CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(time.Minute)
	if err := os.Chtimes(dir+"/tls.crt", mt, mt); err != nil {
		t.Fatal(err)
	}
	if !c.Intercepts("a.test") {
		t.Error("a half-written renewal dropped the working certificate")
	}
}

// The run every test authenticates as, and the key its secret derives from.
var (
	testKey = []byte("shared key")
	testRun = Run{Owner: "acme", Repo: "widgets", Issue: "5", UID: "run-1"}
)

// proxyFor stands up the proxy plus a TLS upstream holding the same leaf, and
// returns the proxy's address along with a pointer to the last address dialled.
func proxyFor(t *testing.T, sans ...string) (proxyAddr string, dialed *string, gotHost *string, pool *x509.CertPool) {
	t.Helper()
	dir := t.TempDir()
	pool = issue(t, dir, sans...)
	certs, err := LoadCerts(dir)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := tls.LoadX509KeyPair(dir+"/tls.crt", dir+"/tls.key")
	if err != nil {
		t.Fatal(err)
	}

	host := new(string)
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*host = r.Host
		_, _ = io.WriteString(w, "upstream\n")
	}))
	up.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	up.StartTLS()
	t.Cleanup(up.Close)

	s := New(certs, testKey, func(context.Context, string) (Run, error) { return testRun, nil })
	s.tr.TLSClientConfig.RootCAs = pool
	addr := new(string)
	real := (&net.Dialer{}).DialContext
	s.dial = func(ctx context.Context, network, a string) (net.Conn, error) {
		*addr = a
		return real(ctx, network, strings.TrimPrefix(up.URL, "https://"))
	}
	px := httptest.NewServer(s)
	t.Cleanup(px.Close)
	return strings.TrimPrefix(px.URL, "http://"), addr, host, pool
}

// connect opens a CONNECT tunnel through the proxy, asserts the 200, and returns
// the raw connection with its reader positioned on the first tunnelled byte.
func connect(t *testing.T, proxyAddr, authority, pipelined string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	// The credential every client derives from the https_proxy URL's userinfo.
	user, pass := Cred(testKey, testRun)
	auth := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
	// pipelined goes out in the same write, unread: a client is entitled to send
	// its first payload bytes without waiting for the 200, and they must survive
	// the hijack.
	if _, err := io.WriteString(c, "CONNECT "+authority+" HTTP/1.1\r\nHost: "+authority+
		"\r\nProxy-Authorization: Basic "+auth+"\r\n\r\n"+pipelined); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil || !strings.Contains(line, "200") {
		t.Fatalf("CONNECT %s: %q %v", authority, line, err)
	}
	if _, err := br.ReadString('\n'); err != nil { // the blank line ending it
		t.Fatal(err)
	}
	return c, br
}

// The pin. A sandbox controls the inner Host header; if that header picked the
// upstream, it could point an intercepted tunnel — and one day the credential
// attached to it — at a host of its choosing. One line, so it gets a test.
func TestInnerHostCannotRedirect(t *testing.T) {
	proxyAddr, dialed, upstreamHost, pool := proxyFor(t, "intercepted.test")

	raw, _ := connect(t, proxyAddr, "intercepted.test:443", "")
	tc := tls.Client(raw, &tls.Config{RootCAs: pool, ServerName: "intercepted.test"})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("handshake against our own CA: %v", err)
	}
	if _, err := io.WriteString(tc, "GET /x HTTP/1.1\r\nHost: evil.test\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if *dialed != "intercepted.test:443" {
		t.Errorf("dialled %q — the inner Host header redirected the tunnel", *dialed)
	}
	if *upstreamHost != "intercepted.test" {
		t.Errorf("upstream saw Host %q, want intercepted.test", *upstreamHost)
	}
}

// A host we hold no certificate for gets a byte pipe and nothing else — no
// handshake, no inspection.
func TestTunnelIsOpaque(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()

	dir := t.TempDir()
	issue(t, dir, "intercepted.test")
	certs, err := LoadCerts(dir)
	if err != nil {
		t.Fatal(err)
	}
	px := httptest.NewServer(New(certs, testKey, func(context.Context, string) (Run, error) { return testRun, nil }))
	defer px.Close()

	// Pipelined, so this also asserts the bytes buffered with the CONNECT are not
	// dropped — where a TLS client's ClientHello would be.
	raw, br := connect(t, strings.TrimPrefix(px.URL, "http://"), echo.Addr().String(), "ping\n")
	_ = raw.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := br.ReadString('\n')
	if err != nil || got != "ping\n" {
		t.Fatalf("tunnel echoed %q, %v", got, err)
	}
}
