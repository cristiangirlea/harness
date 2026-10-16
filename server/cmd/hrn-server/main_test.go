package main

import (
	"os"
	"strings"
	"testing"
)

func TestListenRequiresTLSExceptForLoopbackIPLiterals(t *testing.T) {
	tests := []struct {
		name     string
		address  string
		loopback bool
	}{
		{"loopback v4", "127.0.0.1:8088", true},
		{"loopback v4 range", "127.42.0.2:8088", true},
		{"loopback v6", "[::1]:8088", true},
		{"mapped loopback v4", "[::ffff:127.0.0.1]:8088", true},
		{"empty host", ":8080", false},
		{"unspecified v4", "0.0.0.0:8088", false},
		{"unspecified v6", "[::]:8088", false},
		{"LAN address", "192.168.1.10:8088", false},
		{"public address", "203.0.113.1:8088", false},
		{"host name", "control.example:8088", false},
		{"localhost", "localhost:8088", false},
		{"missing port", "127.0.0.1", false},
		{"empty address", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateListen(tt.address, "", "")
			if tt.loopback {
				if err != nil {
					t.Errorf("loopback HTTP refused: %v", err)
				}
			} else if err == nil {
				t.Error("non-loopback HTTP allowed")
			} else {
				for _, setting := range []string{"HRN_LISTEN", "HRN_TLS_CERT_FILE", "HRN_TLS_KEY_FILE"} {
					if !strings.Contains(err.Error(), setting) {
						t.Errorf("error %q does not name %s", err, setting)
					}
				}
			}
			if err := validateListen(tt.address, "cert.pem", "key.pem"); err != nil {
				t.Errorf("TLS refused: %v", err)
			}
		})
	}
}

func TestListenRequiresBothTLSFiles(t *testing.T) {
	for _, address := range []string{"127.0.0.1:8088", "0.0.0.0:8088"} {
		for _, files := range [][2]string{{"cert.pem", ""}, {"", "key.pem"}} {
			err := validateListen(address, files[0], files[1])
			if err == nil || !strings.Contains(err.Error(), "HRN_TLS_CERT_FILE and HRN_TLS_KEY_FILE must be set together") {
				t.Errorf("validateListen(%q, %q, %q) = %v, want paired TLS settings error", address, files[0], files[1], err)
			}
		}
	}
}

func TestServeRefusesPlainNetworkHTTPBeforeOpeningDatabase(t *testing.T) {
	args := os.Args
	os.Args = []string{"hrn-server", "serve"}
	t.Cleanup(func() { os.Args = args })
	t.Setenv("HRN_DATABASE_URL", "://invalid")
	t.Setenv("HRN_OPERATOR_TOKEN", strings.Repeat("o", 32))
	t.Setenv("HRN_SCALER_TOKEN", strings.Repeat("s", 32))
	t.Setenv("HRN_LISTEN", ":8080")
	t.Setenv("HRN_TLS_CERT_FILE", "")
	t.Setenv("HRN_TLS_KEY_FILE", "")
	if err := run(); err == nil || !strings.Contains(err.Error(), "HRN_LISTEN") {
		t.Fatalf("run() = %v, want listen configuration error before database access", err)
	}
}

func TestMaxCostPerJobParsesThePerJobLimit(t *testing.T) {
	if limit, err := maxCostPerJob(""); err != nil || limit != 0 {
		t.Fatalf("unset = %v, %v; want no limit", limit, err)
	}
	if limit, err := maxCostPerJob("2.5"); err != nil || limit != 2.5 {
		t.Fatalf("2.5 = %v, %v", limit, err)
	}
	for _, raw := range []string{"abc", "0", "-1", "NaN", "+Inf"} {
		if _, err := maxCostPerJob(raw); err == nil || !strings.Contains(err.Error(), "HRN_MAX_COST_USD_PER_JOB") {
			t.Errorf("%q: err = %v, want the variable named", raw, err)
		}
	}
}
