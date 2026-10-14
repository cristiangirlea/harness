package mask

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTextRules(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"url", "https://user:password@example.test/a", "https://••••@example.test/a"},
		{"user", `curl -u "user:password" --user=other:secret`, `curl -u "••••" --user=••••`},
		{"json", `{"password": "a\"b", "api_key":"value", "name":"ok"}`, `{"password": "••••", "api_key":"••••", "name":"ok"}`},
		{"headers", "Authorization: Bearer whatever\nx-api-key: abc\nCookie: a=b; c=d", "Authorization: ••••\nx-api-key: ••••\nCookie: ••••"},
		{"bearer", "use Bearer abc.def here", "use Bearer •••• here"},
		{"openai", "sk-test-12345", "••••"},
		{"github", "ghp_abc123 github_pat_abc123", "•••• ••••"},
		{"slack", "xoxb-123-abc", "••••"},
		{"aws", "AKIAIOSFODNN7EXAMPLE", "••••"},
		{"jwt", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.signature", "••••"},
		{"env", `API_KEY=abc TOKEN='two words' SECRET="three words" PASSWORD=p AUTH=a CREDENTIALS=c COOKIE=k SESSION=s`, `API_KEY=•••• TOKEN='••••' SECRET="••••" PASSWORD=•••• AUTH=•••• CREDENTIALS=•••• COOKIE=•••• SESSION=••••`},
		{"flags", `--password abc --token="two words" --api-key 'three words'`, `--password •••• --token="••••" --api-key '••••'`},
		{"escaped env", `{"command":"API_KEY=\"two words\" cmd"}`, `{"command":"API_KEY=\"••••\" cmd"}`},
		{"escaped flag", `{"command":"cmd --token \"two words\""}`, `{"command":"cmd --token \"••••\""}`},
		{"adjacent tokens", "ghp_abc,ghp_def sk-abc sk-def", "••••,•••• •••• ••••"},
		{"idempotent", "API_KEY=••••", "API_KEY=••••"},

		{"url user only", "https://user@example.test", "https://••••@example.test"},
		{"user escaped", `{"command":"curl -u \"user:password\""}`, `{"command":"curl -u \"••••\""}`},
		{"empty and ordinary", "", ""},
		{"ordinary url port", "https://example.test:443/path", "https://example.test:443/path"},

		{"unquoted escapes", `API_KEY=two\ words cmd --password a\b`, `API_KEY=•••• cmd --password ••••`},

		{"unchanged", "feature/sk-add-user-avatar 0123456789abcdef0123456789abcdef01234567 -p https://example.test/a NAME=value", "feature/sk-add-user-avatar 0123456789abcdef0123456789abcdef01234567 -p https://example.test/a NAME=value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Text(tc.in); got != tc.want {
				t.Fatalf("Text = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTextBound(t *testing.T) {
	for _, secret := range []string{"API_KEY=", "Authorization: Bearer ", "--password ", `{"password":"`, "sk-", "https://user:password", "--user user:", "AKIA", "eyJ"} {
		prefix := strings.Repeat("a", 950) + " " + secret
		got := Text(prefix + strings.Repeat("z", 2000))
		if strings.Contains(got, "zz") || !strings.Contains(got, "••••") || !strings.HasSuffix(got, "…") {
			t.Errorf("boundary leak: %q", got)
		}
	}
	got := Text(strings.Repeat("界", 400))
	if !utf8.ValidString(got) || got != strings.Repeat("界", 341)+"…" {
		t.Fatalf("split rune: %q", got)
	}
	got = Text(strings.Repeat("é", 600))
	if !utf8.ValidString(got) || got != strings.Repeat("é", 512)+"…" {
		t.Fatalf("rune bound: %q", got)
	}
	if got := Text(strings.Repeat("a", 1024)); len(got) != 1024 {
		t.Fatal("exact bound cut")
	}
}
