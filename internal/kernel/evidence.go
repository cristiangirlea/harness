package kernel

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/cristiangirlea/harness/internal/mask"
)

type Evidence struct {
	Kind    string `json:"kind,omitempty"`
	Source  string `json:"source,omitempty"`
	Hash    string `json:"hash,omitempty"` // SHA-256 of the recorded content.
	Excerpt string `json:"excerpt,omitempty"`
}

// NewEvidence hashes the original content and masks the bounded excerpt.
func NewEvidence(kind, source string, content []byte) Evidence {
	hash := sha256.Sum256(content)
	excerpt := strings.ToValidUTF8(mask.Text(string(content)), "�")
	if len(excerpt) > 2048 {
		excerpt = excerpt[:2048]
		for !utf8.ValidString(excerpt) {
			excerpt = excerpt[:len(excerpt)-1]
		}
	}
	return Evidence{Kind: kind, Source: source, Hash: fmt.Sprintf("%x", hash), Excerpt: excerpt}
}
