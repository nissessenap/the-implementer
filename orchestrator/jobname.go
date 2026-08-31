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
	// Every name carries the hash, and the hash is the whole of uniqueness: the
	// slug in front of it is there to be *read*, not to distinguish runs.
	//
	// The first version hashed conditionally — only when normalisation lost
	// something — and that condition cannot be written correctly. The components
	// are joined with '-', which is legal *inside* an owner and inside a repo, so
	// every re-split of the join at a different '-' is a distinct identity with the
	// same name: `acme-my/repo#5` and `acme/my-repo#5`, or
	// `kubernetes/sigs-cluster-api#70` and `kubernetes-sigs/cluster-api#70`, all of
	// them lossless and all of them under the cap. The second run then collides at
	// the apiserver, is swallowed as redelivery, and is silently dropped — which
	// turns "no database" into "silently drops runs".
	//
	// Nine characters of suffix buys that whole class away, and the name still
	// reads: nissessenap-the-implementer-70-1a2b3c4d.
	slug := slugify(r.Owner + "-" + r.Repo + "-" + r.Issue)

	// Over the *raw* identity, case-folded, never over the slug: hashing the lossy
	// artifact would make `my_repo` and `my-repo` hash identically and defeat the
	// point. Case-folded because GitHub forbids owners and repos differing only in
	// case, so that fold is the one normalisation which loses nothing.
	sum := sha256.Sum256([]byte(strings.ToLower(r.String())))
	hash := hex.EncodeToString(sum[:])[:hashLen]

	if len(slug) > maxName-hashLen-1 {
		slug = slug[:maxName-hashLen-1]
	}
	// Trimmed after truncating, or a cut landing on a '-' puts two dashes in front
	// of the hash — legal in a label, and it reads as a bug. An identity that
	// slugifies to nothing at all gets the bare hash rather than a leading '-',
	// which is not a DNS-1123 label at all: unreachable through ParseIssue, which
	// requires a numeric issue, but JobName takes a bare Run and the webhook
	// front-end will build one from a payload instead.
	if slug = strings.Trim(slug, "-"); slug == "" {
		return hash
	}
	return slug + "-" + hash
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
