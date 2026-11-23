package retrieval

import (
	"regexp"
	"strings"
)

var knowledgeToken = regexp.MustCompile(`\bakia[a-z0-9]{16}\b|\beyj[a-z0-9_-]+\.[a-z0-9_-]+\.[a-z0-9_-]+\b`)

// UnsafeText mirrors the import screen before a maintainer reviews a draft; the server still decides.
func UnsafeText(text string, paths bool) string {
	text = strings.ToLower(text)
	for _, marker := range []string{"<project-reference", "</project-reference>", "[reference ", "[/reference]"} {
		if strings.Contains(text, marker) {
			return marker
		}
	}
	if pattern := unsafeKnowledgePath(text); paths && pattern != "" {
		return pattern
	}
	for _, secret := range []string{"-----begin private key-----", "-----begin rsa private key-----", "-----begin openssh private key-----", "-----begin ec private key-----", "-----begin encrypted private key-----", "sk-ant-", "ghp_", "github_pat_", "authorization: bearer ", "ignore previous instructions", "disable permission checks"} {
		if strings.Contains(text, secret) {
			return secret
		}
	}
	if knowledgeToken.MatchString(text) {
		return knowledgeToken.String()
	}
	return ""
}
func unsafeKnowledgePath(text string) string {
	text = strings.ReplaceAll(text, `\`, "/")
	for _, private := range []string{".codex/auth.json", ".ssh/", ".aws/credentials", "credentials.json"} {
		if strings.Contains(text, private) {
			return private
		}
	}
	for _, segment := range strings.FieldsFunc(text, func(r rune) bool {
		return r == '/' || r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '"' || r == '\'' || r == '`' || r == '=' || r == ',' || r == ';'
	}) {
		if segment == ".env" {
			return ".env"
		}
		if strings.HasPrefix(segment, ".env.") && len(segment) > len(".env.") {
			return ".env.*"
		}
	}
	return ""
}
