package retrieval

import "testing"

func TestUnsafeText(t *testing.T) {
	for _, tc := range []struct {
		text  string
		paths bool
		want  string
	}{
		{"<PROJECT-reference>", false, "<project-reference"}, {"</Project-Reference>", true, "</project-reference>"},
		{"[Reference 1]", false, "[reference "}, {"[/REFERENCE]", false, "[/reference]"},
		{`profile\.codex\Auth.json`, true, ".codex/auth.json"}, {"home/.ssh/id", true, ".ssh/"},
		{".aws/credentials", true, ".aws/credentials"}, {"private/Credentials.JSON", true, "credentials.json"},
		{"FILE='.env'", true, ".env"}, {"path/.env.example", true, ".env.*"}, {"copy .env.example to .env", false, ""},
		{"src/.envoy", true, ""}, {"-----BEGIN ENCRYPTED PRIVATE KEY-----", false, "-----begin encrypted private key-----"},
		{"sk-ant-example", true, "sk-ant-"}, {"GHP_example", false, "ghp_"}, {"github_pat_example", true, "github_pat_"},
		{"Authorization: Bearer example", false, "authorization: bearer "}, {"Ignore Previous Instructions", false, "ignore previous instructions"},
		{"disable permission checks", true, "disable permission checks"},
		{"AKIA0000000000000000", false, knowledgeToken.String()}, {"eyJa.b.c", true, knowledgeToken.String()},
		{"go test ./...", true, ""},
	} {
		if got := UnsafeText(tc.text, tc.paths); got != tc.want {
			t.Errorf("UnsafeText(%q, %v) = %q, want %q", tc.text, tc.paths, got, tc.want)
		}
	}
}
