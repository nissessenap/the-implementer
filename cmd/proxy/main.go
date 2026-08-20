// Command proxy runs the credential proxy. Like the package it wraps, it imports
// nothing from the orchestrator.
package main

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/nissessenap/the-implementer/proxy"
)

func main() {
	dir := os.Getenv("TLS_DIR")
	if dir == "" {
		log.Fatal("TLS_DIR is required: the certificate is also the intercept list")
	}
	certs, err := proxy.LoadCerts(dir)
	if err != nil {
		log.Fatalf("TLS_DIR=%s: %v", dir, err)
	}

	// Required, both of them: a proxy that cannot authenticate its caller hands
	// authenticated GitHub, GAR and Vertex access to any pod that reaches its
	// ClusterIP, and with no NetworkPolicy in MVP that is every pod in the
	// cluster. There is no unauthenticated mode to fall back to.
	keyFile := os.Getenv("RUN_KEY_FILE")
	if keyFile == "" {
		log.Fatal("RUN_KEY_FILE is required: it is what authenticates the calling run")
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		log.Fatalf("RUN_KEY_FILE=%s: %v", keyFile, err)
	}
	// Trimmed, because a key that arrives from a file rather than --from-literal
	// carries a trailing newline and the mismatch is invisible in a hex digest.
	if key = bytes.TrimSpace(key); len(key) == 0 {
		log.Fatalf("RUN_KEY_FILE=%s is empty", keyFile)
	}
	// Optional, like every credential here. With none of them the proxy is still a
	// working interception proxy — every host comes out tokenless, which is what
	// e2e stages 30 and 40 exercise.
	//
	// Two ways to hold it, and refusing both at once is the point: they are not
	// equivalent — one is scoped to the calling run's repository and the other is
	// scoped to whatever the operator put in the Secret — so "which one won" must
	// never be something to work out from a log.
	appID, tokenFile := os.Getenv("GITHUB_APP_ID"), os.Getenv("GITHUB_TOKEN_FILE")
	if appID != "" && tokenFile != "" {
		log.Fatal("GITHUB_APP_ID and GITHUB_TOKEN_FILE are both set: the minted and static credentials are not interchangeable, pick one")
	}
	var creds []*proxy.Credential
	switch {
	case appID != "":
		id, err := strconv.ParseInt(appID, 10, 64)
		if err != nil {
			log.Fatalf("GITHUB_APP_ID=%s: %v", appID, err)
		}
		// Boot-time, and blocking: NewGHAIT with WithValidateKey reaches the KMS,
		// so a key that is disabled or the wrong algorithm fails here rather than
		// on the first run that needed a token.
		gh, err := proxy.MintedGitHub(context.Background(), proxy.GitHubApp{
			AppID: id,
			// The provider must also have been *linked in* by build tag; naming
			// one that was not is a boot failure, by name. Blanket underscore
			// imports are deliberately not used: ghait's provider registry is a
			// global map with no identity check, so anything in the binary can
			// shadow a provider.
			Provider: os.Getenv("GITHUB_APP_PROVIDER"),
			Key:      os.Getenv("GITHUB_APP_KEY"),
		})
		if err != nil {
			log.Fatalf("GitHub App: %v", err)
		}
		creds = append(creds, gh)
	case tokenFile != "":
		gh, err := proxy.StaticGitHub(tokenFile)
		if err != nil {
			log.Fatalf("GITHUB_TOKEN_FILE=%s: %v", tokenFile, err)
		}
		creds = append(creds, gh)
	}
	// Artifact Registry, on the proxy's own Google identity — Workload Identity,
	// and so a metadata server. There is nowhere to mount a key, deliberately, so
	// a cluster without one cannot turn this on. Off by default and explicitly on
	// rather than "on whenever ADC happens to resolve": the certificate covers
	// `*.pkg.dev` either way, so attaching a Google token is the operator's
	// decision and not a side effect of where the pod runs.
	//
	// ponytail: set-or-not, not parsed — the chart only writes this var when the
	// operator turned it on, so `GAR_ENABLED=false` by hand means on. A second
	// writer wants strconv.ParseBool here.
	if os.Getenv("GAR_ENABLED") != "" {
		// Blocking, like the App key check above: it resolves ADC and spends one
		// token, so a proxy that can reach no Google identity at all never goes
		// ready. It cannot tell a *wrong* one from a right one — a GKE cluster
		// with no Workload Identity binding lends its node pool's service account
		// and everything works — so GAR logs the identity's email instead.
		gar, err := proxy.GAR(context.Background())
		if err != nil {
			log.Fatalf("Artifact Registry: %v", err)
		}
		creds = append(creds, gar)
	}
	// Validated against the certificate here, at boot: a credential bound to a
	// host we cannot intercept can never fire, and the symptom months later is an
	// unauthenticated request rather than an error.
	credFor, err := proxy.NewCreds(certs, creds...)
	if err != nil {
		log.Fatalf("credentials: %v", err)
	}

	ns := os.Getenv("POD_NAMESPACE")
	if ns == "" {
		log.Fatal("POD_NAMESPACE is required: it is the only namespace we watch pods in")
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("in-cluster config: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("kubernetes client: %v", err)
	}
	// Blocks until the cache has synced, before the listener exists — so the
	// readiness probe cannot pass while every run would be refused as unknown.
	pods, err := proxy.WatchPods(context.Background(), cs, ns)
	if err != nil {
		log.Fatalf("watching pods in %s: %v", ns, err)
	}

	// :8080 and not a knob: the sandbox is handed an `https_proxy` URL naming
	// this port, so a second place to change it is a second place to get it wrong.
	srv := &http.Server{
		Addr:    ":8080",
		Handler: proxy.New(certs, key, pods.Run, credFor),

		// The two timeouts a CONNECT proxy can actually set. Every pod in the
		// cluster can reach this port — there is no NetworkPolicy in MVP — and
		// without ReadHeaderTimeout a caller that trickles its request line one
		// byte at a time holds a goroutine indefinitely, before the handler, and so
		// before it has authenticated as anything.
		//
		// Not ReadTimeout and not WriteTimeout, which is the tempting completion of
		// the set and would break every tunnel: their deadlines are set on the
		// connection and survive the hijack, so they would cut a CONNECT off
		// mid-transfer. These two are safe — net/http clears the read deadline once
		// the headers are parsed, and IdleTimeout only governs the gap between
		// keep-alive requests, which a hijacked connection no longer has.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("listening on %s, resolving runs in namespace %s", srv.Addr, ns)
	log.Fatal(srv.ListenAndServe())
}
