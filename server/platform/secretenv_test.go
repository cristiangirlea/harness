package platform

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestSecretEnv(t *testing.T) {
	const name = "HRN_TEST_SECRET"
	const secret = "private-test-value"
	tests := []struct {
		name    string
		plain   string
		file    bool
		content string
		missing bool
		dir     bool
		want    string
		wantErr string
	}{
		{name: "plain value", plain: secret, want: secret},
		{name: "LF", file: true, content: secret + "\n", want: secret},
		{name: "CRLF", file: true, content: secret + "\r\n", want: secret},
		{name: "no newline", file: true, content: secret, want: secret},
		{name: "two newlines", file: true, content: secret + "\n\n", want: secret + "\n"},
		{name: "both set", plain: secret, file: true, content: secret, wantErr: "set " + name + " or " + name + "_FILE, not both"},
		{name: "missing file", file: true, missing: true, wantErr: name + "_FILE"},
		{name: "directory", file: true, dir: true, wantErr: "regular file"},
		{name: "empty file", file: true, wantErr: "empty"},
		{name: "empty after LF", file: true, content: "\n", wantErr: "empty"},
		{name: "empty after CRLF", file: true, content: "\r\n", wantErr: "empty"},
		{name: "over limit", file: true, content: strings.Repeat(secret, 5000), wantErr: "64 KiB"},
		{name: "at limit", file: true, content: strings.Repeat("x", 64*1024), want: strings.Repeat("x", 64*1024)},
		{name: "neither set"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(name, tt.plain)
			t.Setenv(name+"_FILE", "")
			var path string
			if tt.file {
				path = filepath.Join(t.TempDir(), "secret")
				switch {
				case tt.dir:
					if err := os.Mkdir(path, 0755); err != nil {
						t.Fatal(err)
					}
				case !tt.missing:
					if err := os.WriteFile(path, []byte(tt.content), 0644); err != nil {
						t.Fatal(err)
					}
				}
				t.Setenv(name+"_FILE", path)
			}
			got, err := SecretEnv(name)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatal("expected an error")
				}
				message := err.Error()
				if !strings.Contains(message, tt.wantErr) {
					t.Errorf("error = %q, want substring %q", message, tt.wantErr)
				}
				if !strings.Contains(message, name) {
					t.Error("error does not name the variable")
				}
				if tt.plain == "" && !strings.Contains(message, strconv.Quote(path)) {
					t.Error("file error does not name the path")
				}
				if strings.Contains(message, secret) {
					t.Error("error contains secret content")
				}
				if got != "" {
					t.Error("returned a value with an error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Error("returned an unexpected secret value")
			}
		})
	}
}
