package eval

import (
	"embed"
	"encoding/json"
)

//go:embed testdata/retrieval-manifest.json testdata/retrieval-synthetic-samples.json
var retrievalFixtures embed.FS

// RetrievalFixture loads labelled deterministic accounting inputs, never a backend.
func RetrievalFixture() (RetrievalManifest, []RetrievalSample, error) {
	var m RetrievalManifest
	var s []RetrievalSample
	b, e := retrievalFixtures.ReadFile("testdata/retrieval-manifest.json")
	if e == nil {
		e = json.Unmarshal(b, &m)
	}
	if e != nil {
		return m, s, e
	}
	b, e = retrievalFixtures.ReadFile("testdata/retrieval-synthetic-samples.json")
	if e == nil {
		e = json.Unmarshal(b, &s)
	}
	return m, s, e
}
