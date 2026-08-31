package orchestrator

import (
	"regexp"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/nissessenap/the-implementer/proxy"
)

// isLabel is the apiserver's own check, not a copy of it: alphabet, the leading
// and trailing character, and the 63-character cap in one call. A name this
// rejects is a name `kubectl create` rejects.
func isLabel(t *testing.T, name, why string) {
	t.Helper()
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		t.Errorf("JobName(%s) = %q: %s", why, name, strings.Join(errs, "; "))
	}
}

func name(owner, repo, issue string) string {
	return JobName(proxy.Run{Owner: owner, Repo: repo, Issue: issue, UID: "ignored"})
}

// The ordinary case, and the one an operator reads in `kubectl get job`: the
// identity, then the hash. No prefix, and the slug is not truncated.
func TestJobNameIsTheSlug(t *testing.T) {
	got := name("nissessenap", "the-implementer", "15")
	if !strings.HasPrefix(got, "nissessenap-the-implementer-15-") {
		t.Errorf("JobName = %q, want the identity in front of the hash", got)
	}
	if !regexp.MustCompile(`^nissessenap-the-implementer-15-[0-9a-f]{8}$`).MatchString(got) {
		t.Errorf("JobName = %q is not slug + 8 hex", got)
	}
	// Case-folding needs no separate name: GitHub forbids owners and repos that
	// differ only in case, so the fold is not lossy and both spellings are one run.
	if fold := name("NisseSsenap", "The-Implementer", "15"); fold != got {
		t.Errorf("JobName = %q for the case-folded spelling, want %q", fold, got)
	}
}

// The reason every name is hashed. Two pairs, and neither can be separated by a
// length check: normalisation is lossy (`my_repo` and `my-repo` both slugify to
// `my-repo` — google-deepmind/open_spiel keeps its underscore and open-spiel
// 404s), and the '-' the components are joined with is legal *inside* an owner and
// a repo, so the join re-splits. Collide either pair and the second run is
// swallowed as redelivery: "no database" becomes "silently drops runs".
func TestJobNameSurvivesLossyNormalisation(t *testing.T) {
	for _, p := range [][2]proxy.Run{
		{{Owner: "acme", Repo: "my_repo", Issue: "5"}, {Owner: "acme", Repo: "my-repo", Issue: "5"}},
		{{Owner: "acme-my", Repo: "repo", Issue: "5"}, {Owner: "acme", Repo: "my-repo", Issue: "5"}},
		{{Owner: "kubernetes", Repo: "sigs-cluster-api", Issue: "70"}, {Owner: "kubernetes-sigs", Repo: "cluster-api", Issue: "70"}},
	} {
		a, b := JobName(p[0]), JobName(p[1])
		if a == b {
			t.Errorf("%s and %s both got %q", p[0], p[1], a)
		}
		isLabel(t, a, p[0].String())
		isLabel(t, b, p[1].String())
	}
	// The hash is over the raw identity, never the slug — hashing the lossy
	// artifact would make both variants hash identically and defeat the point.
	if hashOf(name("acme", "my_repo", "5")) == hashOf(name("acme", "my.repo", "5")) {
		t.Error("my_repo and my.repo hash the same: the hash is over the slug")
	}
}

// An identity that slugifies to nothing leaves the bare hash, not a leading '-',
// which the apiserver refuses. Unreachable through ParseIssue — the issue must be
// numeric, so the slug always ends in a digit — but JobName takes a bare Run and
// the webhook front-end will build one from a payload.
func TestJobNameIsAlwaysALabel(t *testing.T) {
	for _, r := range []proxy.Run{
		{Owner: "_", Repo: "_", Issue: "_"},
		{},
		{Owner: "..", Repo: "..", Issue: ".."},
		{Owner: "acme", Repo: "widgets", Issue: "5"},
	} {
		isLabel(t, JobName(r), r.String())
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
	isLabel(t, long, "the long identity")
	// The invariant is the cap and not equality with it: trimming a cut that landed
	// on a '-' gives 62, which is correct. What must be true is that the name was
	// truncated *and* hashed rather than merely cut, which is the suffix.
	if !regexp.MustCompile(`-[0-9a-f]{8}$`).MatchString(long) {
		t.Errorf("JobName = %q does not end in a hash", long)
	}
	if len(long) < 55 {
		t.Errorf("JobName = %q is %d chars, so nothing was truncated", long, len(long))
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
