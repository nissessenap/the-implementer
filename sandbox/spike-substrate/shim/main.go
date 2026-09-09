// Command shim is the throwaway HTTP front for the run plan, for the substrate
// spike only (sandbox/spike-substrate/README.md). Substrate has no batch
// primitive, no exec RPC and no completion state: work reaches an Actor as an
// ordinary HTTP request through atenet-router, and the workload is whatever HTTP
// server the container happens to run. So this is that server, and it is the
// *whole* of the adaptation — phase.sh is untouched.
//
// ponytail: throwaway. No auth, no TLS, no graceful shutdown. The router's mTLS
// tunnel is the only thing in front of it and the actor is deleted after one run.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// request is the per-run input. Everything the Job's PodSpec carries as env
// arrives here instead — including the credentials, which is the one deliberate
// departure from ADR 0005 and is argued in the README: an ActorTemplate's env is
// literal-only and the template is shared by every run of a language, so a token
// there would be the "pod env exists before the user does" failure upstream
// warns about. Per-run over the tunnel is worse than the proxy and better than
// the template.
type request struct {
	Repo        string `json:"repo"`
	Issue       string `json:"issue"`
	Toolchain   string `json:"toolchain"`
	GHToken     string `json:"gh_token"`
	ClaudeToken string `json:"claude_token"`
	MaxUSDPhase string `json:"max_usd_per_phase"`
}

// one run at a time: an Actor is one run (README), and a second concurrent
// phase.sh would share the workspace and the branch with the first.
var running sync.Mutex

func main() {
	http.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	http.HandleFunc("/run", run)
	addr := ":" + env("PORT", "80")
	log.Printf("spike shim listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func run(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST /run", http.StatusMethodNotAllowed)
		return
	}
	if !running.TryLock() {
		http.Error(w, "a run is already in progress", http.StatusConflict)
		return
	}
	defer running.Unlock()

	var req request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Repo == "" || req.Issue == "" {
		http.Error(w, "repo and issue are required", http.StatusBadRequest)
		return
	}

	// The result channel. phase.sh writes its blob to $TERM_LOG, which it already
	// takes from the environment rather than hard-coding /dev/termination-log —
	// so the BYO contract needed no change at all to grow a second reader.
	home := env("HOME", "/home/agent")
	termLog := filepath.Join(home, "result.json")
	os.Remove(termLog)

	// PHASE_SH is the image's path in the image and a stub in main_test.go —
	// the same seam phase.sh itself uses for TERM_LOG and OPT.
	cmd := exec.Command(env("PHASE_SH", "/usr/local/bin/phase.sh"))
	cmd.Env = append(os.Environ(),
		"REPO="+req.Repo,
		"ISSUE="+req.Issue,
		"TOOLCHAIN="+req.Toolchain,
		"GH_TOKEN="+req.GHToken,
		"CLAUDE_CODE_OAUTH_TOKEN="+req.ClaudeToken,
		"TERM_LOG="+termLog,
		"WORKSPACE="+env("WORKSPACE", "/workspace"),
		"UNATTENDED=1",
		"MAX_BUDGET_PER_PHASE_USD="+def(req.MaxUSDPhase, "2"),
	)
	// The transcript stays the observability channel: stdout/stderr here is what
	// `kubectl ate logs actors` shows, the same role `kubectl logs` plays today.
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr

	log.Printf("run start repo=%s issue=%s toolchain=%q", req.Repo, req.Issue, req.Toolchain)
	runErr := cmd.Run()

	blob, readErr := os.ReadFile(termLog)
	w.Header().Set("Content-Type", "application/json")
	if readErr != nil || len(blob) == 0 {
		// The "ending with no result" case, which is exactly what the informer's
		// why() covers today: nothing inside the sandbox got to speak. Same
		// synthesised-Result answer, different reason source.
		log.Printf("run produced no blob: exec=%v read=%v", runErr, readErr)
		json.NewEncoder(w).Encode(map[string]any{
			"status":  "failed",
			"branch":  "implementer/issue-" + req.Issue,
			"message": fmt.Sprintf("no result blob: exec=%v read=%v", runErr, readErr),
			"phases":  []any{},
		})
		return
	}
	log.Printf("run done exec=%v blob=%dB", runErr, len(blob))
	w.Write(blob)
}

func env(k, d string) string { return def(os.Getenv(k), d) }

func def(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
