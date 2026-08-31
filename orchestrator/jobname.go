// Package orchestrator builds the one object a run is: a Job.
//
// It imports the proxy for the annotation keys, the Run identity and the
// credential derivation. That direction is the whole of the seam — the proxy
// imports nothing of this package's — so there is one set of annotation strings
// and one HMAC in the system rather than two that can drift.
package orchestrator

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/nissessenap/the-implementer/proxy"
)

// maxName is the cap, and it is not metadata.name's 253. The Job controller
// stamps the name into `spec.template`'s labels, and a label *value* caps at 63 —
// measured against k3s in proto/jobname.sh, where the apiserver's rejection names
// no cause an operator would recognise.
const maxName = 63

// hashLen leaves 54 characters of slug: 54 + '-' + 8 = 63.
const hashLen = 8

// JobName derives the Job's name from the run's issue. This is the whole of
// idempotency (ADR 0004): redelivery, double-labelling and a mid-run restart all
// produce the same name, and `AlreadyExists` is swallowed. There is no dedupe
// table because there is nothing to keep one in.
//
// No `implementer-` prefix: the orchestrator runs in a dedicated namespace, which
// is both the uniqueness scope and the reason a prefix would be redundant. That
// makes the dedicated namespace a load-bearing configuration item.
//
// The run's UID is deliberately not part of the name — it is what makes the
// *credential* per-run, where the name is per-issue on purpose.
func JobName(r proxy.Run) string {
	raw := r.Owner + "-" + r.Repo + "-" + r.Issue
	slug := slugify(raw)

	// The condition is wider than length, and that is the load-bearing clause.
	// Normalisation is lossy: `acme/my_repo#5` and `acme/my-repo#5` both slugify
	// to `acme-my-repo-5`, both are under the cap, and a length-only condition
	// would give them the same name — silently swallowing the second run as
	// redelivery, which turns "no database" into "silently drops runs".
	//
	// "Slugifying lost nothing" is exactly "the slug is the identity, lowercased",
	// so the comparison is the predicate — no second spelling of the alphabet.
	if len(slug) <= maxName && slug == strings.ToLower(raw) {
		return slug
	}

	// Over the *raw* identity, case-folded, never over the slug: hashing the lossy
	// artifact would make both variants hash identically and defeat the point.
	// Case-folded because GitHub forbids owners and repos differing only in case,
	// so that fold is the one normalisation which loses nothing.
	sum := sha256.Sum256([]byte(strings.ToLower(r.String())))
	if len(slug) > maxName-hashLen-1 {
		slug = slug[:maxName-hashLen-1]
	}
	// Trimmed after truncating, or a cut landing on a '-' puts two dashes in front
	// of the hash — legal in a label, and it reads as a bug. Only the trailing one:
	// interior runs come from the identity itself and are left alone.
	return strings.Trim(slug, "-") + "-" + hex.EncodeToString(sum[:])[:hashLen]
}

// slugify is lower(s) with every character outside [a-z0-9] replaced by '-' and
// the result trimmed, which is DNS-1123's alphabet for a label.
func slugify(s string) string {
	b := []byte(strings.ToLower(s))
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			b[i] = '-'
		}
	}
	return strings.Trim(string(b), "-")
}
