package main

import "testing"

// The only non-trivial logic in the proxy is turning what Claude Code appends to
// ANTHROPIC_VERTEX_BASE_URL into a real Vertex URL. Get that wrong and every model
// call 404s from inside a cluster, which is an expensive way to find a typo.
func TestVertexRewrite(t *testing.T) {
	for _, c := range []struct{ in, wantHost, wantPath string }{
		// what the docs say Claude Code appends (no /v1)
		{"/vertex/projects/p/locations/global/publishers/anthropic/models/claude-opus-5:streamRawPredict",
			"aiplatform.googleapis.com",
			"/v1/projects/p/locations/global/publishers/anthropic/models/claude-opus-5:streamRawPredict"},
		// ...and if it already includes it, don't double up
		{"/vertex/v1/projects/p/locations/europe-west1/publishers/anthropic/models/m:rawPredict",
			"europe-west1-aiplatform.googleapis.com",
			"/v1/projects/p/locations/europe-west1/publishers/anthropic/models/m:rawPredict"},
		// multi-region gets the .rep. host, not the {loc}- prefix
		{"/vertex/projects/p/locations/eu/publishers/anthropic/models/m:streamRawPredict",
			"aiplatform.eu.rep.googleapis.com",
			"/v1/projects/p/locations/eu/publishers/anthropic/models/m:streamRawPredict"},
	} {
		p, host := rewriteVertex(c.in)
		if host != c.wantHost || p != c.wantPath {
			t.Errorf("%s\n got  %s %s\n want %s %s", c.in, host, p, c.wantHost, c.wantPath)
		}
	}
}
