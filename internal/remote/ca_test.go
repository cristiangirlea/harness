package remote

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReadCAPEMStopsAfterTheSizeLimit(t *testing.T) {
	// A reader without a size models a file growing after the handle's Stat.
	input := strings.NewReader(strings.Repeat("x", 2<<20))
	_, err := readCAPEM(input)
	if err == nil || !strings.Contains(err.Error(), "HRN_SERVER_CA_FILE") || !strings.Contains(err.Error(), "at most 1 MiB") {
		t.Fatalf("oversized read error = %v", err)
	}
	if got := (2 << 20) - input.Len(); got != (1<<20)+1 {
		t.Fatalf("read %d bytes, want the limit plus one byte", got)
	}
}

func TestReadCAPEMAllowsExactlyTheSizeLimit(t *testing.T) {
	data, err := readCAPEM(strings.NewReader(strings.Repeat("x", 1<<20)))
	if err != nil || len(data) != 1<<20 {
		t.Fatalf("read %d bytes, error = %v", len(data), err)
	}
}

type brokenCAReader struct{}

func (brokenCAReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestReadCAPEMNamesCAFileInReadErrors(t *testing.T) {
	_, err := readCAPEM(brokenCAReader{})
	if !errors.Is(err, io.ErrUnexpectedEOF) || !strings.Contains(err.Error(), "HRN_SERVER_CA_FILE") {
		t.Fatalf("read error = %v, want named and wrapped read failure", err)
	}
}

func TestNewFromEnvNamesCAFileInOpenErrors(t *testing.T) {
	t.Setenv("HRN_SERVER_URL", "https://host")
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", filepath.Join(t.TempDir(), "missing.pem"))
	if _, err := NewFromEnv(); !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "HRN_SERVER_CA_FILE") {
		t.Fatalf("open error = %v, want named and wrapped open failure", err)
	}
}

func TestNewFromEnvRejectsOversizedCAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", (1<<20)+1)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRN_SERVER_URL", "https://host")
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", path)
	if _, err := NewFromEnv(); err == nil || !strings.Contains(err.Error(), "HRN_SERVER_CA_FILE") || !strings.Contains(err.Error(), "at most 1 MiB") {
		t.Fatalf("oversized CA file error = %v", err)
	}
}

func TestNewFromEnvNamesCAFileInCertificateErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRN_SERVER_URL", "https://host")
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	t.Setenv("HRN_SERVER_CA_FILE", path)
	if _, err := NewFromEnv(); err == nil || !strings.Contains(err.Error(), "HRN_SERVER_CA_FILE") || !strings.Contains(err.Error(), "no certificates") {
		t.Fatalf("invalid certificate error = %v", err)
	}
}

func TestNewFromEnvRefusesADevicePathBeforeOpeningIt(t *testing.T) {
	t.Setenv("HRN_SERVER_URL", "https://host")
	t.Setenv("HRN_TENANT_TOKEN", strings.Repeat("t", 64))
	opened := false
	original := openCAFile
	openCAFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		opened = true
		return original(name, flag, perm)
	}
	t.Cleanup(func() { openCAFile = original })
	paths := []string{os.DevNull, t.TempDir()}
	if runtime.GOOS == "windows" {
		paths = append(paths, `\\.\pipe\hrn-ca-test`, `\\?\`+filepath.Join(t.TempDir(), "ca.pem"),
			`//./pipe/hrn-ca-test`, `//?/C:/ca.pem`, `\\.\NUL`, `\??\C:\ca.pem`, filepath.Join(t.TempDir(), "COM1"), "con.pem",
			"COM1:", "NUL:", `C:\x\LPT1:`, `\\localhost\pipe\hrn-ca-test`, `//127.0.0.1/PIPE/hrn-ca-test`,
			`\\localhost\mailslot\hrn-ca-test`)
	} else {
		fifo := filepath.Join(t.TempDir(), "ca.fifo")
		makeCAFIFO(t, fifo)
		paths = append(paths, fifo)
	}
	for _, path := range paths {
		opened = false
		t.Setenv("HRN_SERVER_CA_FILE", path)
		if _, err := NewFromEnv(); err == nil || !strings.Contains(err.Error(), "regular PEM CA file") || opened {
			t.Errorf("CA file %q: error = %v, opened = %v; want a refusal before any open", path, err, opened)
		}
	}
}
