package remote

import (
	"errors"
	"io"
	"os"
	"path/filepath"
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
