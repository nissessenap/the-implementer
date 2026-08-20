//go:build ghait.vault

package proxy

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/isometry/ghait"
)

// The half of the signing path the e2e cannot reach without a GitHub App: ghait's
// *vault* provider actually signing an App JWT, and GitHub verifying it.
//
// e2e stage 55 proves the other half — a real OpenBao, a real BYOK import, a real
// signature over a real network — but nothing there calls ghait's Sign(), because
// a mint needs GitHub. This does, offline, with a transit that signs and a GitHub
// that checks, so the whole chain is covered on every pull request.
//
// Built only with `ghait.vault`, which is the tag the e2e's image is built with;
// `make test` runs the package twice for exactly that reason.

// fakeTransit is Vault/OpenBao's transit engine, reduced to the two calls ghait
// makes: read the key's metadata, and sign. It signs for real, with an RSA key the
// test holds, so what comes back is verifiable rather than a fixture.
type fakeTransit struct {
	*httptest.Server
	key *rsa.PrivateKey
	// version is the key version transit claims in its `vault:vN:` prefix. 1 in
	// life; anything else is a key that has been rotated.
	version int
}

func newFakeTransit(t *testing.T, version int) *fakeTransit {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeTransit{key: k, version: version}
	mux := http.NewServeMux()
	// The boot-time check: ghait refuses anything but a signing rsa-2048 key.
	mux.HandleFunc("GET /v1/transit/keys/app", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"type": "rsa-2048", "supports_signing": true},
		})
	})
	mux.HandleFunc("PUT /v1/transit/sign/app", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input      string `json:"input"`
			Hash       string `json:"hash_algorithm"`
			Sig        string `json:"signature_algorithm"`
			Marshaling string `json:"marshaling_algorithm"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("transit got %s", body)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		// Pinned here rather than trusted: these three are what make the answer a
		// JWT signature segment, and a change to any of them upstream would
		// otherwise show up as a GitHub 401 in production.
		if req.Hash != "sha2-256" || req.Sig != "pkcs1v15" || req.Marshaling != "jws" {
			t.Errorf("transit asked for %s/%s/%s, want sha2-256/pkcs1v15/jws", req.Hash, req.Sig, req.Marshaling)
		}
		signing, err := base64.StdEncoding.DecodeString(req.Input)
		if err != nil {
			t.Errorf("input is not base64: %v", err)
			http.Error(w, "bad input", http.StatusBadRequest)
			return
		}
		sum := sha256.Sum256(signing)
		sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		// `jws` marshaling is base64url with no padding — which is what lets ghait
		// hand it straight to golang-jwt as the third segment — behind transit's
		// own `vault:vN:` prefix.
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"signature": fmt.Sprintf("vault:v%d:%s", f.version, base64.RawURLEncoding.EncodeToString(sig)),
		}})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// verifyingGitHub is newFakeGitHub's job plus the one thing it has no reason to
// do: check the App JWT's signature, as the real GitHub does. Without that a
// signature mangled on the way out still mints, and the failure moves to
// production.
func verifyingGitHub(t *testing.T, pub *rsa.PublicKey) *httptest.Server {
	t.Helper()
	verify := func(w http.ResponseWriter, r *http.Request) bool {
		jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		parts := strings.Split(jwt, ".")
		if len(parts) != 3 {
			http.Error(w, `{"message":"A JSON web token could not be decoded"}`, http.StatusUnauthorized)
			return false
		}
		sig, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			http.Error(w, `{"message":"A JSON web token could not be decoded"}`, http.StatusUnauthorized)
			return false
		}
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return false
		}
		return true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		if !verify(w, r) {
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 4242})
	})
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		if !verify(w, r) {
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_minted_through_transit",
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func vaultMinter(t *testing.T, transit *fakeTransit) (*Credential, error) {
	t.Helper()
	gh := verifyingGitHub(t, &transit.key.PublicKey)
	// The provider's client is configured by environment and nothing else — ghait
	// passes it no options — which is the same reason the chart has VAULT_ADDR and
	// VAULT_TOKEN and no other knob.
	t.Setenv("VAULT_ADDR", transit.URL)
	t.Setenv("VAULT_TOKEN", "not-a-real-token")
	// The trailing slash matters: go-github joins paths onto this URL.
	return mintedGitHub(context.Background(),
		GitHubApp{AppID: 7, Provider: "vault", Key: "transit/app"},
		ghait.WithURLs(gh.URL+"/", gh.URL+"/"))
}

// The whole chain, credential-free: ghait signs the App JWT through transit and
// GitHub verifies it against the key transit holds.
func TestVaultProviderSignsAJWTGitHubAccepts(t *testing.T) {
	c, err := vaultMinter(t, newFakeTransit(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := c.Token(context.Background(), Run{Owner: "acme", Repo: "widgets", Issue: "5", UID: "u1"})
	if err != nil {
		t.Fatalf("mint through transit: %v", err)
	}
	if tok != "ghs_minted_through_transit" {
		t.Errorf("token = %q", tok)
	}
}

// The rotation bug, pinned rather than fixed: ghait v0.14.0 strips a hardcoded
// "vault:v1:", so a rotated key's "vault:v2:" prefix stays inside the signature
// segment and every mint fails — an outage, not a bypass, which is the half of it
// that matters. It is fixed upstream and not here; when a bump fixes it this test
// fails, which is how the operator warning in values.yaml gets deleted.
func TestVaultProviderFailsClosedOnARotatedKey(t *testing.T) {
	c, err := vaultMinter(t, newFakeTransit(t, 2))
	if err != nil {
		// Check() only reads the key, so boot survives a rotation; if that ever
		// changes this test still holds, it just holds one step earlier.
		return
	}
	if tok, err := c.Token(context.Background(), Run{Owner: "acme", Repo: "widgets", Issue: "5", UID: "u1"}); err == nil {
		t.Errorf("minted %q with a v2 prefix inside the signature — it should have failed closed", tok)
	}
}
