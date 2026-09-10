// The one check the shim earns: the result channel. phase.sh is stubbed the way
// sandbox/phase_test.go stubs claude and gh — the point is that the blob the
// script writes to $TERM_LOG is what the HTTP caller receives byte-for-byte, and
// that a script which writes nothing still produces a decodable Result rather
// than an empty 200.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const body = `{"repo":"o/r","issue":"42","toolchain":"go","gh_token":"t","claude_token":"c"}`

// stub writes a phase.sh replacement and points the handler's env at it.
func stub(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "phase.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PHASE_SH", path)
	t.Setenv("AGENT_HOME", dir)
}

func post(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	run(w, httptest.NewRequest(http.MethodPost, "/run", strings.NewReader(body)))
	return w
}

func TestBlobIsTheResponse(t *testing.T) {
	// The env the run plan is handed, echoed into the blob, so the test also
	// pins that REPO/ISSUE/TOOLCHAIN and the credentials actually arrive.
	stub(t, `printf '{"status":"completed","branch":"%s","repo":"%s","tc":"%s","gh":"%s"}' \
	  "implementer/issue-$ISSUE" "$REPO" "$TOOLCHAIN" "$GH_TOKEN" > "$TERM_LOG"`)

	w := post(t)
	if w.Code != http.StatusOK {
		t.Fatalf("code %d: %s", w.Code, w.Body)
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("undecodable: %v: %s", err, w.Body)
	}
	for k, want := range map[string]string{
		"status": "completed", "branch": "implementer/issue-42",
		"repo": "o/r", "tc": "go", "gh": "t",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}

func TestNoBlobIsStillAResult(t *testing.T) {
	// The ending with no result — the case the informer's why() covers today.
	stub(t, `echo "died before writing" >&2; exit 1`)

	w := post(t)
	if w.Code != http.StatusOK {
		t.Fatalf("code %d: %s", w.Code, w.Body)
	}
	var got struct {
		Status  string `json:"status"`
		Branch  string `json:"branch"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("undecodable: %v: %s", err, w.Body)
	}
	if got.Status != "failed" {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if got.Branch != "implementer/issue-42" {
		t.Errorf("branch = %q", got.Branch)
	}
	if !strings.Contains(got.Message, "no result blob") {
		t.Errorf("message = %q", got.Message)
	}
}

func TestStaleBlobIsNotReturned(t *testing.T) {
	// A blob left behind by an earlier run must not be served as this run's
	// result — the reason run() removes it before exec.
	stub(t, `exit 1`)
	if err := os.WriteFile(filepath.Join(os.Getenv("AGENT_HOME"), "result.json"),
		[]byte(`{"status":"completed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var got struct{ Status string }
	if err := json.Unmarshal(post(t).Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "failed" {
		t.Errorf("stale blob served: status = %q, want failed", got.Status)
	}
}
