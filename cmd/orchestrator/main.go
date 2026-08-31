// Command orchestrator turns an issue into a run.
//
// Deliberately a command line and not the webhook: every hard part of the PodSpec
// is demoable and testable without a publicly reachable endpoint, and the webhook
// lands on top of this. What it does is what the webhook front-end will do —
// build one Job and create it — so the seam being exercised here is the real one.
//
//	orchestrator run [-dry-run] [-toolchain go] owner/repo#5
//	orchestrator watch [-once]
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
	"os/signal"
	"strconv"
	"strings"
	"syscall"

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
	// ponytail: a hand-rolled dispatch, still. A flag library or per-subcommand
	// help when one of these grows a second page of flags.
	switch os.Args[1] {
	case "run":
		run(os.Args[2:])
	case "watch":
		watch(os.Args[2:])
	case "cred":
		cred(os.Args[2:])
	default:
		usage()
	}
}

func usage() {
	log.Fatal("usage: orchestrator run [-dry-run] [-toolchain <t>] <owner>/<repo>#<n>\n" +
		"       orchestrator watch [-once]\n" +
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

	ctx, kube := context.Background(), client()
	created, err := orchestrator.Create(ctx, kube, job, *dry)
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
	// The existing Job's phase, appended *last* so $1 and $2 stay what the e2e
	// reads. Without it `exists` covers two different situations: a redelivery,
	// and a re-run of a run that already finished — ttlSecondsAfterFinished keeps
	// a terminal Job for a day, and for that day this would exit 0 having done
	// nothing, with no way to tell. Whether a re-run should replace a terminal Job
	// is a policy question above this line; being able to see which one happened
	// is not.
	state := ""
	if !created {
		state = " (" + orchestrator.Phase(ctx, kube, cfg.Namespace, job.Name) + ")"
	}
	fmt.Printf("%s %s/%s %s%s\n", verb, cfg.Namespace, job.Name, r, state)
}

// watch is the informer half of ADR 0004: it watches the runs in this namespace and
// gives every ending one issue comment. Why it exists at all is written down once,
// on orchestrator.Reporter.
//
// It holds no App private key: the token comes from the credential proxy's own mint
// path, signed by whichever external provider was linked in by build tag.
func watch(args []string) {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	// Restart-is-a-relist, on its own: reconcile what is in the namespace now and
	// exit. Not an operator mode — it is how the e2e asserts the restart case
	// without a Deployment, and there is no Deployment until the webhook lands.
	once := fs.Bool("once", false, "reconcile the runs in the namespace once and exit, rather than watching")
	fs.Parse(args)
	if fs.NArg() != 0 {
		usage()
	}

	ctx := context.Background()
	appID, err := strconv.ParseInt(env("GITHUB_APP_ID"), 10, 64)
	if err != nil {
		log.Fatalf("GITHUB_APP_ID=%s: %v", os.Getenv("GITHUB_APP_ID"), err)
	}
	// The API base URL is the seam, and it is handed to *both* clients: ghait's
	// mint path and the orchestrator's own calls. Empty is api.github.com.
	api := os.Getenv("GITHUB_API_URL")
	// The same mint path the proxy uses, deliberately — one signer, one build-tag
	// choice of provider, and no second implementation of "authenticate as the
	// App". The credential it returns is per-run and scoped to the run's own
	// repository, which is exactly the scope the comment needs.
	cred, err := proxy.MintedGitHub(ctx, proxy.GitHubApp{
		AppID: appID,
		// Must have been linked in by build tag; naming one that was not is a boot
		// failure, by name.
		Provider: env("GITHUB_APP_PROVIDER"),
		Key:      env("GITHUB_APP_KEY"),
		BaseURL:  api,
	})
	if err != nil {
		log.Fatalf("GitHub App: %v", err)
	}

	r := &orchestrator.Reporter{
		Kube: client(),
		NS:   env("POD_NAMESPACE"),
		GH:   &orchestrator.GitHub{BaseURL: api, Token: cred.Token},
	}
	if *once {
		if err := r.Reconcile(ctx); err != nil {
			log.Fatal(err)
		}
		return
	}
	// SIGTERM is how a Deployment rolls, and the informer's whole lifetime is this
	// context — so a rollout stops the watch rather than being killed mid-report.
	// Nothing is lost either way: the next process relists.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := r.Watch(ctx); err != nil {
		log.Fatal(err)
	}
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
