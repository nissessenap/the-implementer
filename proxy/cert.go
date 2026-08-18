package proxy

import (
	"crypto/tls"
	"fmt"
	"log"
	"os"
	"sync"
	"time"
)

// Certs serves the proxy's serving certificate and, from the same object, the
// list of hosts it may intercept. One source, so the two cannot drift: a host we
// cannot present a certificate for is a host we must not intercept, and there is
// nothing else to configure.
//
// Re-read when the file changes, because cert-manager renews the Secret and the
// kubelet refreshes the mount underneath a running pod. Serving the boot-time
// certificate forever — the prototype's bug — outlives its own expiry.
type Certs struct {
	certFile, keyFile string

	mu   sync.Mutex
	mod  time.Time
	cert *tls.Certificate
}

// LoadCerts reads tls.crt/tls.key from dir and fails loudly if it cannot: a
// proxy with no certificate has nothing to serve.
func LoadCerts(dir string) (*Certs, error) {
	c := &Certs{certFile: dir + "/tls.crt", keyFile: dir + "/tls.key"}
	if _, err := c.current(); err != nil {
		return nil, err
	}
	return c, nil
}

// current returns the loaded certificate, re-reading it first if the file's
// modification time moved. A stat per call rather than a watcher goroutine: it
// costs microseconds on a path that is already doing a TLS handshake.
// ponytail: stat-on-demand, swap for fsnotify if handshake latency ever shows it.
func (c *Certs) current() (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// The certificate alone, not both files: the kubelet swaps the whole mount
	// atomically, so the key never changes without this timestamp moving.
	st, err := os.Stat(c.certFile)
	if err != nil {
		if c.cert != nil {
			log.Printf("certs: stat %s: %v (serving previous)", c.certFile, err)
			return c.cert, nil
		}
		return nil, err
	}
	if c.cert != nil && st.ModTime().Equal(c.mod) {
		return c.cert, nil
	}

	cert, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		// A renewal caught mid-write is a transient the previous certificate
		// covers; only a cold start has nothing to fall back to.
		if c.cert != nil {
			log.Printf("certs: reload failed: %v (serving previous)", err)
			return c.cert, nil
		}
		return nil, fmt.Errorf("load %s: %w", c.certFile, err)
	}
	c.cert, c.mod = &cert, st.ModTime()
	log.Printf("certs: loaded, intercepting %v (expires %s)",
		cert.Leaf.DNSNames, cert.Leaf.NotAfter.Format(time.RFC3339))
	return c.cert, nil
}

// GetCertificate is tls.Config's hook, called per handshake — which is what makes
// a renewed Secret take effect with no pod restart.
func (c *Certs) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return c.current()
}

// Intercepts answers "is this host on the certificate", and picks up a renewal's
// SAN list on the same reload the certificate itself arrives on.
//
// x509.Certificate.VerifyHostname rather than our own matcher, so what we agree to
// intercept and what a client will accept cannot disagree. That rule is why the
// SAN is `*.pkg.dev` and not `*-go.pkg.dev`: a wildcard must occupy the whole
// leftmost label, so the certificate is deliberately wider than any credential
// rule layered on top of it.
func (c *Certs) Intercepts(host string) bool {
	cert, err := c.current()
	if err != nil {
		return false
	}
	return cert.Leaf.VerifyHostname(host) == nil
}
