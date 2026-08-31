package orchestrator

import (
	"regexp"
	"testing"

	"github.com/nissessenap/the-implementer/proxy"
)

func name(owner, repo, issue string) string {
	return JobName(proxy.Run{Owner: owner, Repo: repo, Issue: issue, UID: "ignored"})
}

// The ordinary case, and the one an operator reads in `kubectl get job`: no hash,
// no prefix, just the identity.
func TestJobNameIsTheSlug(t *testing.T) {
	if got, want := name("nissessenap", "the-implementer", "15"), "nissessenap-the-implementer-15"; got != want {
		t.Errorf("JobName = %q, want %q", got, want)
	}
	// Case-folding needs no hash: GitHub forbids owners and repos that differ
	// only in case, so the fold is not lossy.
	if got, want := name("NisseSsenap", "The-Implementer", "15"), "nissessenap-the-implementer-15"; got != want {
		t.Errorf("JobName = %q, want %q", got, want)
	}
}

// THE load-bearing clause. Narrow the condition to length alone and both of these
// become "acme-my-repo-5": the second run is swallowed as redelivery and "no
// database" turns into "silently drops runs". Underscores in repository names are
// real — google-deepmind/open_spiel keeps its underscore and open-spiel 404s.
func TestJobNameSurvivesLossyNormalisation(t *testing.T) {
	under, dash := name("acme", "my_repo", "5"), name("acme", "my-repo", "5")
	if under == dash {
		t.Fatalf("acme/my_repo#5 and acme/my-repo#5 both got %q", under)
	}
	if dash != "acme-my-repo-5" {
		t.Errorf("the clean name should not be hashed: %q", dash)
	}
	if len(under) > 63 {
		t.Errorf("hashed name %q is %d chars", under, len(under))
	}
	// The hash is over the raw identity, never the slug — hashing the lossy
	// artifact would make both variants hash identically and defeat the point.
	if hashOf(under) == hashOf(name("acme", "my.repo", "5")) {
		t.Error("my_repo and my.repo hash the same: the hash is over the slug")
	}
}

func hashOf(n string) string {
	if len(n) < 8 {
		return n
	}
	return n[len(n)-8:]
}

// 63 is the cap, and it comes from spec.template's labels rather than
// metadata.name — which would have allowed 253. Anything longer is refused by the
// apiserver with an error naming no cause an operator would recognise.
func TestJobNameFitsTheLabelCap(t *testing.T) {
	long := name("kubernetes-sigs", "cluster-api-provider-openstack-and-then-some-more-name", "12345")
	if len(long) > 63 {
		t.Errorf("JobName = %q, %d chars, cap is 63", long, len(long))
	}
	// The invariant is the cap and not equality with it: trimming a cut that landed
	// on a '-' gives 62, which is correct. What must be true is that the name was
	// truncated *and* hashed rather than merely cut, which is the suffix.
	if !regexp.MustCompile(`-[0-9a-f]{8}$`).MatchString(long) {
		t.Errorf("JobName = %q does not end in a hash", long)
	}
	if len(long) < 55 {
		t.Errorf("JobName = %q is %d chars, so nothing was truncated", long, len(long))
	}
	// Still a DNS-1123 label: the truncation must not leave a trailing dash
	// before the hash separator, and nothing outside [a-z0-9-].
	for i, c := range long {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			t.Errorf("JobName = %q: byte %d is %q", long, i, c)
		}
	}
	if long[0] == '-' || long[len(long)-1] == '-' {
		t.Errorf("JobName = %q starts or ends with a dash", long)
	}
}

// Two long repositories differing past the truncation point still get different
// names, which is the other half of what the hash buys.
func TestJobNameTruncationDoesNotCollide(t *testing.T) {
	pad := "a-very-long-repository-name-that-runs-past-the-cap"
	a, b := name("acme", pad+"-one", "5"), name("acme", pad+"-two", "5")
	if a == b {
		t.Errorf("both truncated to %q", a)
	}
}
