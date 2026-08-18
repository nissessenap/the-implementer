// Command proxy runs the credential proxy. Like the package it wraps, it imports
// nothing from the orchestrator.
package main

import (
	"log"
	"net/http"
	"os"

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

	addr := ":" + envOr("PORT", "8080")
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, proxy.New(certs)))
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
