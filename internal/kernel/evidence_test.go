package kernel

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestNewEvidence(t *testing.T) {
	content := []byte("API_TOKEN=secretvalue\n" + strings.Repeat("a", 2024) + "€tail")
	e := NewEvidence("gate", "test", content)
	if e.Kind != "gate" || e.Source != "test" || e.Hash != fmt.Sprintf("%x", sha256.Sum256(content)) {
		t.Fatalf("incorrect evidence: %+v", e)
	}
	if len(e.Excerpt) != 2047 || !utf8.ValidString(e.Excerpt) || strings.Contains(e.Excerpt, "secretvalue") {
		t.Fatalf("unsafe excerpt: %q", e.Excerpt)
	}
}
