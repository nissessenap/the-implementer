package proxy

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/isometry/ghait"
)

// testAppKey writes a PKCS#1 RSA key for ghait's "file" provider — the one
// provider linked into the default build, and the one the e2e signs with.
func testAppKey(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := t.TempDir() + "/app.pem"
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	if err := os.WriteFile(f, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// fakeGitHub is GitHub's App API, reduced to the two calls a mint makes. It
// records what was asked of it, which is the whole assertion: the installation
// looked up and the repository the token was scoped to must both come from the
// run, never from anywhere else.
type fakeGitHub struct {
	*httptest.Server

	mints atomic.Int64
	// lastScope is the JSON body of the last token request: which repositories
	// the token was narrowed to.
	lastScope atomic.Value // string
	// life is how long the tokens it hands out have left.
	life time.Duration
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{life: time.Hour}
	mux := http.NewServeMux()
	// GET /repos/{owner}/{repo}/installation — which installation has this
	// repository. The id encodes the owner so a test can tell them apart.
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("owner") != "acme" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242})
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("mint arrived without an App JWT: %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		f.lastScope.Store(string(body))
		n64 := f.mints.Add(1)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      fmt.Sprintf("ghs_minted%d", n64),
			"expires_at": time.Now().Add(f.life).Format(time.RFC3339),
		})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func newTestMinter(t *testing.T, gh *fakeGitHub) *Credential {
	t.Helper()
	// The trailing slash matters: go-github joins paths onto this URL.
	c, err := mintedGitHub(context.Background(),
		GitHubApp{AppID: 7, Provider: "file", Key: testAppKey(t)},
		ghait.WithURLs(gh.URL+"/", gh.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The non-negotiable one: the token is scoped to the repository the *run* names.
// A sandbox that talks its way into a token for anything else can push to every
// repository the App is installed on.
func TestMintScopesToTheRunsRepository(t *testing.T) {
	gh := newFakeGitHub(t)
	c := newTestMinter(t, gh)

	run := Run{Owner: "acme", Repo: "widgets", Issue: "5", UID: "u1"}
	tok, err := c.Token(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "ghs_minted1" {
		t.Errorf("token = %q", tok)
	}
	if scope := gh.lastScope.Load().(string); !strings.Contains(scope, `"repositories":["widgets"]`) {
		t.Errorf("token request was %s, want it narrowed to [\"widgets\"]", scope)
	}

	// A run whose owner the App is not installed on gets nothing, rather than a
	// token minted against somebody else's installation.
	if tok, err := c.Token(context.Background(), Run{Owner: "evil", Repo: "widgets", Issue: "1", UID: "u2"}); err == nil {
		t.Errorf("minted %q for an owner with no installation", tok)
	}

	// And an incomplete run is refused before it can be scoped to "".
	if tok, err := c.Token(context.Background(), Run{Owner: "acme"}); err == nil {
		t.Errorf("minted %q for an incomplete run", tok)
	}
}

// Cached for the run, and per run: two runs of the same issue must not share a
// credential, which is the whole reason run-uid is in the identity.
func TestMintIsCachedPerRun(t *testing.T) {
	gh := newFakeGitHub(t)
	c := newTestMinter(t, gh)
	ctx := context.Background()

	first := Run{Owner: "acme", Repo: "widgets", Issue: "5", UID: "u1"}
	second := Run{Owner: "acme", Repo: "widgets", Issue: "5", UID: "u2"}

	a, err := c.Token(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Token(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if a != b || gh.mints.Load() != 1 {
		t.Errorf("the same run minted %d times (%q then %q)", gh.mints.Load(), a, b)
	}
	d, err := c.Token(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if d == a {
		t.Error("a re-run of the same issue inherited the previous run's token")
	}
}

// The hour boundary. A token inside the refresh skew is no use to a clone that is
// about to start, so it is replaced rather than handed out.
func TestMintRefreshesBeforeExpiry(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.life = refreshSkew / 2
	c := newTestMinter(t, gh)
	ctx := context.Background()
	run := Run{Owner: "acme", Repo: "widgets", Issue: "5", UID: "u1"}

	a, err := c.Token(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Token(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("a token %v from expiry was handed out twice", gh.life)
	}
}

// The App is bound to the same hosts the static token was, and no others: the
// certificate is deliberately wider than the credential rule.
func TestMintedCredentialIsBoundToGitHubHosts(t *testing.T) {
	c := newTestMinter(t, newFakeGitHub(t))
	if got := strings.Join(c.Hosts, ","); got != strings.Join(githubHosts, ",") {
		t.Errorf("hosts = %v, want %v", c.Hosts, githubHosts)
	}
}

// A provider nobody linked in is a boot failure, by name. This is what makes the
// build-tag selection safe to rely on: ghait's registry is a global map with no
// identity check, so "is it there" is the only question worth asking, and asking
// it late means asking it on a run.
func TestMintRefusesAnUnlinkedProvider(t *testing.T) {
	for _, app := range []GitHubApp{
		{AppID: 7, Provider: "gcp", Key: "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1"},
		{AppID: 0, Provider: "file", Key: "/dev/null"},
		{AppID: 7, Provider: "file"},
	} {
		if _, err := MintedGitHub(context.Background(), app); err == nil {
			t.Errorf("accepted %+v", app)
		}
	}
}
