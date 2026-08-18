// Command proxy runs the credential proxy. Like the package it wraps, it imports
// nothing from the orchestrator.
package main

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"os"

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
	log.Printf("listening on :8080, resolving runs in namespace %s", ns)
	log.Fatal(http.ListenAndServe(":8080", proxy.New(certs, key, pods.Run)))
}
