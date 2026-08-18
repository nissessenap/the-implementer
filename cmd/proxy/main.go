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

	// :8080 and not a knob: the sandbox is handed an `https_proxy` URL naming
	// this port, so a second place to change it is a second place to get it wrong.
	log.Print("listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", proxy.New(certs)))
}
