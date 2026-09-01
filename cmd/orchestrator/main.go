// Command orchestrator turns an issue into a run.
//
// `serve` is the trigger — a labelled issue becomes a run with no human at a
// command line. `run` is the same thing from a terminal, and it stays because
// every hard part of the PodSpec is demoable without a publicly reachable
// endpoint: the two share startRun, so what the webhook does is what `run` does.
//
//	orchestrator serve [-addr :8080]
//	orchestrator run [-dry-run] [-toolchain go] owner/repo#5
//	orchestrator watch [-once]
//	orchestrator cred owner repo issue run-uid
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

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
	case "serve":
		serve(os.Args[2:])
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
	log.Fatal("usage: orchestrator serve [-addr :8080]\n" +
		"       orchestrator run [-dry-run] [-toolchain <t>] <owner>/<repo>#<n>\n" +
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

	cfg := runConfig(*toolchain)
	ctx, kube := context.Background(), client()
	// One word first, then the reference: the e2e reads $2 off this line, and a
	// verb with a space in it would make the parse depend on which branch ran.
	name, verb, err := startRun(ctx, kube, cfg, r, *dry)
	if err != nil {
		log.Fatal(err)
	}
	// The existing Job's phase, appended *last* so $1 and $2 stay what the e2e
	// reads. Without it `exists` covers two different situations: a redelivery,
	// and a re-run of a run that already finished — ttlSecondsAfterFinished keeps
	// a terminal Job for a day, and for that day this would exit 0 having done
	// nothing, with no way to tell. Whether a re-run should replace a terminal Job
	// is a policy question above this line; being able to see which one happened
	// is not.
	state := ""
	if verb == "exists" {
		state = " (" + orchestrator.Phase(ctx, kube, cfg.Namespace, name) + ")"
	}
	// Appended after the phase is read, or the read would key off the decorated verb.
	if *dry {
		verb += "-dry-run"
	}
	fmt.Printf("%s %s/%s %s%s\n", verb, cfg.Namespace, name, r, state)
}

// serve is ADR 0004's front-end half, and the trigger the whole system was for: a
// labelled issue becomes a run with nobody at a command line. It creates one Job
// and is done — it does not wait for the run, and the HTTP response is sent minutes
// before the pull request exists.
//
// It holds no GitHub credential of any kind, which is not an oversight: the
// authorization is two clauses on the payload (orchestrator.Webhook), the refusal
// is silent on purpose, and there is nothing here that could write to GitHub. The
// informer is the half that does, and it is still `watch`.
func serve(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8080", "the address to serve the webhook on; the chart's Service targets 8080")
	fs.Parse(args)
	if fs.NArg() != 0 {
		usage()
	}

	// ponytail: ADR 0003's detection is a later ticket, so until it lands the
	// toolchain is one value for every repository this installation serves — and
	// unset is legal, meaning the review phase runs with no language subagent.
	// Detection replaces this read entirely rather than defaulting it, and it is
	// also the first thing that puts a GitHub credential in this half of the
	// process, which is why it is not a smaller change than it looks.
	cfg := runConfig(os.Getenv("TOOLCHAIN"))
	kube := client()

	h := &orchestrator.Webhook{
		// Required, and by name: an empty secret is an open trigger for anything
		// that can reach the Service, and every legitimate delivery still works —
		// so the misconfiguration is invisible unless boot refuses it.
		Secret: []byte(env("GITHUB_WEBHOOK_SECRET")),
		Start: func(ctx context.Context, r proxy.Run) error {
			// The run's second factor, fresh per run — see `run` above for why it
			// is not per issue, and why a swallowed AlreadyExists discards it.
			r.UID = uid()
			name, verb, err := startRun(ctx, kube, cfg, r, false)
			if err != nil {
				return err
			}
			// The same disambiguation `run` prints, for the same reason: `exists`
			// is a redelivery *or* a re-label of a run that already ended, and the
			// TTL keeps a terminal Job for a day. The response body cannot carry
			// this — nobody reads the App's delivery page — so the log is where a
			// silently-dropped retry becomes visible at all.
			if verb == "exists" {
				verb += " (" + orchestrator.Phase(ctx, kube, cfg.Namespace, name) + ")"
			}
			log.Printf("webhook: %s %s/%s %s", verb, cfg.Namespace, name, r)
			return nil
		},
	}

	// SIGTERM is how a Deployment rolls, and draining is the whole point of the
	// handshake below: a create already sent to the apiserver is a run, while a
	// delivery cut off before it got there is a **lost** one — GitHub does not
	// retry, it only marks the delivery failed and keeps it redeliverable by hand
	// for three days, which needs a human to go looking.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{
		Addr:    *addr,
		Handler: orchestrator.Routes(h),
		// The body timeouts the proxy deliberately omits — there, deadlines set on
		// the connection survive the CONNECT hijack and would cut a transfer off
		// mid-stream; here the handler is plain JSON and none of that applies.
		// Without ReadTimeout the read deadline is cleared once the headers are
		// parsed, so a body trickled a byte at a time holds a goroutine for as long
		// as it likes — before the signature check, so from anything that can reach
		// the Service.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	// Shutdown closes the listeners first, so ListenAndServe returns *immediately*
	// and the in-flight handlers are still running: without waiting for this
	// channel the process would exit through the middle of them, which is the
	// opposite of draining. Bounded at 20s and not longer: the preStop sleep and
	// this both spend the same terminationGracePeriodSeconds (5 + 20 against the
	// default 30), and past it the kubelet's SIGKILL lands with nothing logged.
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			log.Printf("webhook: shutdown: %v", err)
		}
	}()
	log.Printf("webhook: serving %s, creating runs in %s", *addr, cfg.Namespace)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-done
}

// startRun is the whole of "turn a run into an object", and it is shared so that a
// Job a webhook created and a Job a command line created differ in nothing but who
// asked. Returns the Job's name and the one word that says which of the two
// happened: `created`, or `exists` for a name that collided at the apiserver — a
// redelivery, a second label, or a re-run of a Job the TTL still holds.
func startRun(ctx context.Context, kube kubernetes.Interface, cfg orchestrator.Config, r proxy.Run, dry bool) (name, verb string, err error) {
	job := cfg.Build(r)
	created, err := orchestrator.Create(ctx, kube, job, dry)
	if err != nil {
		return job.Name, "", fmt.Errorf("creating job %s: %w", job.Name, err)
	}
	verb = "exists"
	if created {
		verb = "created"
	}
	return job.Name, verb, nil
}

// runConfig is everything the Job builder needs that is not the run, read from the
// environment both `serve` and `run` take it from — so a Job a webhook created and
// a Job a command line created differ in nothing but who asked.
func runConfig(toolchain string) orchestrator.Config {
	cfg := orchestrator.Config{
		Namespace: env("POD_NAMESPACE"),
		ProxyHost: env("PROXY_HOST"),
		Key:       runKey(),
		Toolchain: toolchain,
	}
	var err error
	if cfg.Template, err = orchestrator.LoadTemplate(env("JOB_TEMPLATE_FILE")); err != nil {
		log.Fatal(err)
	}
	return cfg
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
	// context — which the handlers pass to GitHub, so a rollout really does abort a
	// report mid-pagination or mid-POST. Nothing is lost: a POST that reached GitHub
	// left the marker, and the next process relists and finds it either way.
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
