// Command orchestrator turns an issue into a run.
//
// Deliberately a command line and not the webhook: every hard part of the PodSpec
// is demoable and testable without a publicly reachable endpoint, and the webhook
// lands on top of this. What it does is what the webhook front-end will do —
// build one Job and create it — so the seam being exercised here is the real one.
//
//	orchestrator run [-dry-run] [-toolchain go] owner/repo#5
//	orchestrator cred owner repo issue run-uid
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/nissessenap/the-implementer/orchestrator"
	"github.com/nissessenap/the-implementer/proxy"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
	}
	// ponytail: two subcommands and a hand-rolled dispatch. A flag library, or
	// per-subcommand help, when there is a third.
	switch os.Args[1] {
	case "run":
		run(os.Args[2:])
	case "cred":
		cred(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	log.Fatal("usage: orchestrator run [-dry-run] [-toolchain <t>] <owner>/<repo>#<n>\n" +
		"       orchestrator cred <owner> <repo> <issue> <run-uid>")
}

func run(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	dry := fs.Bool("dry-run", false, "send the Job to the apiserver for validation only — it answers whether the derived name is accepted, which is the one question no amount of reasoning about DNS-1123 settles")
	toolchain := fs.String("toolchain", os.Getenv("TOOLCHAIN"), "ADR 0003's answer for this repository; unset is legal and means the review phase runs with no language subagent")
	fs.Parse(args)
	if fs.NArg() != 1 {
		usage()
	}
	r, err := orchestrator.ParseIssue(fs.Arg(0))
	if err != nil {
		log.Fatal(err)
	}
	// The run's second factor, and the reason the credential is per-*run* rather
	// than per-issue: a re-run of the same issue must not inherit the previous
	// run's credential. Fresh here, which is also why a swallowed AlreadyExists
	// below discards it — a redelivery is not a new run.
	r.UID = uid()

	cfg := orchestrator.Config{
		Namespace: env("POD_NAMESPACE"),
		ProxyHost: env("PROXY_HOST"),
		Key:       runKey(),
		Toolchain: *toolchain,
	}
	if cfg.Template, err = orchestrator.LoadTemplate(env("JOB_TEMPLATE_FILE")); err != nil {
		log.Fatal(err)
	}
	job := cfg.Build(r)

	created, err := orchestrator.Create(context.Background(), client(), job, *dry)
	if err != nil {
		log.Fatalf("creating job %s: %v", job.Name, err)
	}
	// One word first, then the reference: the e2e reads $2 off this line, and a
	// verb with a space in it would make the parse depend on which branch ran.
	verb := "exists" // redelivery, swallowed — the name collided at the apiserver
	if created {
		verb = "created"
	}
	if *dry {
		verb += "-dry-run"
	}
	fmt.Printf("%s %s/%s %s\n", verb, cfg.Namespace, job.Name, r)
}

// cred prints the userinfo the sandbox's https_proxy URL carries. Not for
// operators: it exists so the e2e harness derives a run credential from *this*
// code rather than from a hand-rolled `openssl dgst -hmac` that reimplements
// proxy.Cred and can drift from it silently.
func cred(args []string) {
	if len(args) != 4 {
		usage()
	}
	user, pass := proxy.Cred(runKey(), proxy.Run{Owner: args[0], Repo: args[1], Issue: args[2], UID: args[3]})
	fmt.Printf("%s:%s\n", user, pass)
}

func env(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("%s is required", k)
	}
	return v
}

// runKey reads the one long-lived shared key both components mount. A file and
// not a variable, exactly as the proxy reads it — one mechanism, and a key in a
// process environment is a key in every child's.
func runKey() []byte {
	f := env("RUN_KEY_FILE")
	b, err := os.ReadFile(f)
	if err != nil {
		log.Fatalf("RUN_KEY_FILE=%s: %v", f, err)
	}
	// Trimmed, because a key that arrives from a file rather than --from-literal
	// carries a trailing newline and the mismatch is invisible in a hex digest.
	// The proxy trims the same way; if only one end did, nothing would ever
	// authenticate and neither log would say why.
	if b = []byte(strings.TrimSpace(string(b))); len(b) == 0 {
		log.Fatalf("RUN_KEY_FILE=%s is empty", f)
	}
	return b
}

// uid is the run's second factor, and its entropy is not the security parameter:
// the credential is an HMAC under the shared key, so guessing a UID buys nothing
// without the key. What it has to be is *different from the last run of the same
// issue*, which four bytes are.
//
// ponytail: 32 bits, so two runs of one issue collide at about one in four
// billion — and the consequence of a collide is a re-run inheriting the previous
// run's credential, which is the same position as having no UID at all. Widen it
// if that ever becomes a boundary rather than a hygiene measure.
func uid() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		log.Fatalf("run uid: %v", err)
	}
	return hex.EncodeToString(b[:])
}

// client is in-cluster first, kubeconfig second — the same binary is the webhook
// front-end later and a developer's command line now.
func client() kubernetes.Interface {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(), nil).ClientConfig()
		if err != nil {
			log.Fatalf("no cluster: %v", err)
		}
	}
	c, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("kubernetes client: %v", err)
	}
	return c
}
