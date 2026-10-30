package mask

import (
	"strings"
	"testing"
)

func TestTextMasksSecretValuesAndKeepsOrdinaryText(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"url", "https://user:password@example.test/a", "https://••••@example.test/a"},
		{"user", `curl -u "user:password" --user=other:secret`, `curl -u "••••" --user=••••`},
		{"json", `{"password": "a\"b", "api_key":"value", "name":"ok"}`, `{"password": "••••", "api_key":"••••", "name":"ok"}`},
		{"headers", "Authorization: Bearer whatever\nx-api-key: abc\nCookie: a=b; c=d", "Authorization: ••••\nx-api-key: ••••\nCookie: ••••"},
		{"bearer", "use Bearer abc.def here", "use Bearer •••• here"},
		{"openai", "sk-test-12345", "••••"},
		{"github", "ghp_abc123 github_pat_abc123", "•••• ••••"},
		{"github token prefixes", "gho_fake ghs_fake ghu_fake ghr_fake", "•••• •••• •••• ••••"},
		{"glued names", "PGPASSWORD=x APIKEY=x DBPASSWD=x APITOKEN=x APPSECRET=x MYSQL_PWD=x DB_PASSWD=x", "PGPASSWORD=•••• APIKEY=•••• DBPASSWD=•••• APITOKEN=•••• APPSECRET=•••• MYSQL_PWD=•••• DB_PASSWD=••••"},
		{"glued json names", `{"PGPASSWORD":"x","APIKEY":"x","DBPASSWD":"x","MYSQL_PWD":"x"}`, `{"PGPASSWORD":"••••","APIKEY":"••••","DBPASSWD":"••••","MYSQL_PWD":"••••"}`},
		{"ordinary uppercase names", "PWD=/home/x KEYBOARD=us MONKEY=1", "PWD=/home/x KEYBOARD=us MONKEY=1"},
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
		{"url token as user", "https://ghp_abc123@github.com/x", "https://••••@github.com/x"},
		{"ssh git user", "ssh://git@github.com/x git+ssh://git@example.test/y", "ssh://git@github.com/x git+ssh://git@example.test/y"},
		{"scp git user", "git@github.com:x/y.git", "git@github.com:x/y.git"},
		{"git user with password", "https://git:password@example.test/a", "https://••••@example.test/a"},
		{"git-like user", "ssh://gitlab@example.test/a ssh://GIT@example.test/b", "ssh://••••@example.test/a ssh://••••@example.test/b"},
		{"user escaped", `{"command":"curl -u \"user:password\""}`, `{"command":"curl -u \"••••\""}`},
		{"empty and ordinary", "", ""},
		{"ordinary url port", "https://example.test:443/path", "https://example.test:443/path"},

		{"unquoted escapes", `API_KEY=two\ words cmd --password a\b`, `API_KEY=•••• cmd --password ••••`},

		{"ordinary flags", "--max-tokens=400 --auth-type=basic --cookie-jar=f", "--max-tokens=400 --auth-type=basic --cookie-jar=f"},
		{"ordinary names", "authors=x monkey=1 keyboard=us", "authors=x monkey=1 keyboard=us"},
		{"ordinary json names", `{"authors":"x","monkey":"1","keyboard":"us"}`, `{"authors":"x","monkey":"1","keyboard":"us"}`},
		{"name segments", "API_KEY=abc my-token=value SESSION_ID=abc", "API_KEY=•••• my-token=•••• SESSION_ID=••••"},
		{"auth flag", "--auth basic", "--auth ••••"},

		{"unchanged", "feature/sk-add-user-avatar 0123456789abcdef0123456789abcdef01234567 -p https://example.test/a NAME=value", "feature/sk-add-user-avatar 0123456789abcdef0123456789abcdef01234567 -p https://example.test/a NAME=value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Text(tc.in); got != tc.want {
				t.Fatalf("Text = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTextMasksLongTextWithoutCuttingIt(t *testing.T) {
	prefix := strings.Repeat("a", 2048) + " "
	for _, secret := range []string{"API_KEY=value", "Authorization: Bearer value", "--password value", `{"password":"value"}`, "sk-test-value", "https://user:password@example.test", "--user user:password", "AKIAIOSFODNN7EXAMPLE", "eyJheader.payload.signature"} {
		if got, want := Text(prefix+secret), prefix+Text(secret); got != want {
			t.Errorf("long text was cut or left unmasked for %q", secret)
		}
	}
	for _, text := range []string{strings.Repeat("界", 400), strings.Repeat("é", 600), strings.Repeat("a", 2048)} {
		if got := Text(text); got != text {
			t.Error("ordinary long text was cut")
		}
	}
}
