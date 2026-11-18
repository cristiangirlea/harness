package main

import (
	"os"
	"path/filepath"
	"strconv"
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
	t.Setenv("HRN_DATABASE_URL_FILE", "")
	t.Setenv("HRN_OPERATOR_TOKEN_FILE", "")
	t.Setenv("HRN_SCALER_TOKEN_FILE", "")
	t.Setenv("HRN_OPERATOR_TOKEN", strings.Repeat("o", 32))
	t.Setenv("HRN_SCALER_TOKEN", strings.Repeat("s", 32))
	t.Setenv("HRN_LISTEN", ":8080")
	t.Setenv("HRN_TLS_CERT_FILE", "")
	t.Setenv("HRN_TLS_KEY_FILE", "")
	if err := run(); err == nil || !strings.Contains(err.Error(), "HRN_LISTEN") {
		t.Fatalf("run() = %v, want listen configuration error before database access", err)
	}
}

func TestServeRefusesAnInvalidCostLimitBeforeOpeningDatabase(t *testing.T) {
	args := os.Args
	os.Args = []string{"hrn-server", "serve"}
	t.Cleanup(func() { os.Args = args })
	t.Setenv("HRN_DATABASE_URL", "://invalid")
	t.Setenv("HRN_DATABASE_URL_FILE", "")
	t.Setenv("HRN_OPERATOR_TOKEN_FILE", "")
	t.Setenv("HRN_SCALER_TOKEN_FILE", "")
	t.Setenv("HRN_OPERATOR_TOKEN", strings.Repeat("o", 32))
	t.Setenv("HRN_SCALER_TOKEN", strings.Repeat("s", 32))
	t.Setenv("HRN_LISTEN", "127.0.0.1:8088")
	t.Setenv("HRN_TLS_CERT_FILE", "")
	t.Setenv("HRN_TLS_KEY_FILE", "")
	t.Setenv("HRN_MAX_COST_USD_PER_JOB", "abc")
	if err := run(); err == nil || !strings.Contains(err.Error(), "HRN_MAX_COST_USD_PER_JOB") {
		t.Fatalf("run() = %v, want cost limit error before database access", err)
	}
}

func TestRequireTenantBudget(t *testing.T) {
	for _, raw := range []string{"", "1", "0", "true", " 1"} {
		got, err := requireTenantBudget(raw)
		if raw == "" || raw == "1" {
			if got != (raw == "1") || err != nil {
				t.Errorf("%q: %t, %v", raw, got, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "HRN_REQUIRE_TENANT_BUDGET") {
			t.Errorf("%q: want an error naming the variable, got %v", raw, err)
		}
	}
}

func TestServeRefusesInvalidTenantBudgetSettingBeforeOpeningDatabase(t *testing.T) {
	args := os.Args
	os.Args = []string{"hrn-server", "serve"}
	t.Cleanup(func() { os.Args = args })
	t.Setenv("HRN_DATABASE_URL", "invalid-database-url")
	t.Setenv("HRN_DATABASE_URL_FILE", "")
	t.Setenv("HRN_OPERATOR_TOKEN", strings.Repeat("o", 32))
	t.Setenv("HRN_OPERATOR_TOKEN_FILE", "")
	t.Setenv("HRN_SCALER_TOKEN", strings.Repeat("s", 32))
	t.Setenv("HRN_SCALER_TOKEN_FILE", "")
	t.Setenv("HRN_LISTEN", "127.0.0.1:8088")
	t.Setenv("HRN_TLS_CERT_FILE", "")
	t.Setenv("HRN_TLS_KEY_FILE", "")
	t.Setenv("HRN_MAX_COST_USD_PER_JOB", "")
	t.Setenv("HRN_REQUIRE_TENANT_BUDGET", "0")
	if err := run(); err == nil || !strings.Contains(err.Error(), "HRN_REQUIRE_TENANT_BUDGET") {
		t.Fatalf("run() = %v, want tenant budget setting error before database access", err)
	}
}

func TestServeChecksOperatorTokenFromFileBeforeOpeningDatabase(t *testing.T) {
	args := os.Args
	os.Args = []string{"hrn-server", "serve"}
	t.Cleanup(func() { os.Args = args })
	path := filepath.Join(t.TempDir(), "operator-token")
	if err := os.WriteFile(path, []byte("short\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRN_DATABASE_URL", "://invalid")
	t.Setenv("HRN_DATABASE_URL_FILE", "")
	t.Setenv("HRN_OPERATOR_TOKEN", "")
	t.Setenv("HRN_OPERATOR_TOKEN_FILE", path)
	t.Setenv("HRN_SCALER_TOKEN", strings.Repeat("s", 32))
	t.Setenv("HRN_SCALER_TOKEN_FILE", "")
	if err := run(); err == nil || err.Error() != "independent operator and scaler tokens of at least 32 characters are required" {
		t.Fatalf("run() = %v, want token length error before database access", err)
	}
}

func TestServeRejectsMissingDatabaseURLFileBeforeOpeningDatabase(t *testing.T) {
	args := os.Args
	os.Args = []string{"hrn-server", "serve"}
	t.Cleanup(func() { os.Args = args })
	path := filepath.Join(t.TempDir(), "missing-database-url")
	t.Setenv("HRN_DATABASE_URL", "")
	t.Setenv("HRN_DATABASE_URL_FILE", path)
	t.Setenv("HRN_OPERATOR_TOKEN", strings.Repeat("o", 32))
	t.Setenv("HRN_OPERATOR_TOKEN_FILE", "")
	t.Setenv("HRN_SCALER_TOKEN", strings.Repeat("s", 32))
	t.Setenv("HRN_SCALER_TOKEN_FILE", "")
	if err := run(); err == nil || !strings.Contains(err.Error(), "HRN_DATABASE_URL_FILE") || !strings.Contains(err.Error(), strconv.Quote(path)) {
		t.Fatalf("run() = %v, want missing database URL file error before database access", err)
	}
}

func TestServeChecksScalerTokenFromFileBeforeOpeningDatabase(t *testing.T) {
	args := os.Args
	os.Args = []string{"hrn-server", "serve"}
	t.Cleanup(func() { os.Args = args })
	path := filepath.Join(t.TempDir(), "scaler-token")
	if err := os.WriteFile(path, []byte("short\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRN_DATABASE_URL", "://invalid")
	t.Setenv("HRN_DATABASE_URL_FILE", "")
	t.Setenv("HRN_OPERATOR_TOKEN", strings.Repeat("o", 32))
	t.Setenv("HRN_OPERATOR_TOKEN_FILE", "")
	t.Setenv("HRN_SCALER_TOKEN", "")
	t.Setenv("HRN_SCALER_TOKEN_FILE", path)
	if err := run(); err == nil || err.Error() != "independent operator and scaler tokens of at least 32 characters are required" {
		t.Fatalf("run() = %v, want token length error before database access", err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("s", 32)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HRN_LISTEN", ":8080")
	t.Setenv("HRN_TLS_CERT_FILE", "")
	t.Setenv("HRN_TLS_KEY_FILE", "")
	if err := run(); err == nil || !strings.Contains(err.Error(), "HRN_LISTEN") {
		t.Fatalf("run() = %v, want listen configuration error after accepting scaler token file", err)
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

func TestOIDCEnvironment(t *testing.T) {
	const (
		issuer   = "HRN_OIDC_ISSUER"
		audience = "HRN_OIDC_AUDIENCE"
		claim    = "HRN_OIDC_OPERATOR_CLAIM"
		value    = "HRN_OIDC_OPERATOR_VALUE"
		tenant   = "HRN_OIDC_TENANT_CLAIM"
		url      = "https://issuer.example/"
	)
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"off", nil, ""},
		{"operator only", map[string]string{issuer: url, audience: "api", claim: "role", value: "operator"}, ""},
		{"tenant only", map[string]string{issuer: "http://127.1.2.3:8080/path", audience: "api", tenant: "tenant_ids"}, ""},
		{"both", map[string]string{issuer: "http://[::1]:8080", audience: "api", tenant: "tenant-ids", claim: "role", value: "operator"}, ""},
		{"audience alone", map[string]string{audience: "api"}, audience},
		{"operator claim alone", map[string]string{claim: "role"}, claim},
		{"operator value alone", map[string]string{value: "operator"}, value},
		{"tenant claim alone", map[string]string{tenant: "tenants"}, tenant},
		{"missing audience", map[string]string{issuer: url, tenant: "tenants"}, audience},
		{"missing operator value", map[string]string{issuer: url, audience: "api", claim: "role", tenant: "tenants"}, value},
		{"missing operator claim", map[string]string{issuer: url, audience: "api", value: "operator", tenant: "tenants"}, claim},
		{"no role mapping", map[string]string{issuer: url, audience: "api"}, tenant},
		{"nested operator claim", map[string]string{issuer: url, audience: "api", claim: "role.name", value: "operator"}, claim},
		{"nested tenant claim", map[string]string{issuer: url, audience: "api", tenant: "roles[0]"}, tenant},
		{"long tenant claim", map[string]string{issuer: url, audience: "api", tenant: strings.Repeat("x", 129)}, tenant},
	}
	for _, raw := range []string{"", "/issuer", "ftp://issuer.example", "https:///issuer", "http://localhost", "http://192.0.2.1", "http://[::ffff:127.0.0.1]", "https://user@issuer.example", url + "with space", url + "?x=1", url + "?", url + "#x", url + "#"} {
		tests = append(tests, struct {
			name string
			env  map[string]string
			want string
		}{"invalid issuer " + raw, map[string]string{issuer: raw, audience: "api", tenant: "tenants"}, issuer})
	}
	for _, key := range []string{audience, claim, value, tenant} {
		tests = append(tests, struct {
			name string
			env  map[string]string
			want string
		}{"empty " + key, map[string]string{issuer: url, key: ""}, key})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := oidcFromEnv(func(key string) (string, bool) {
				raw, ok := tc.env[key]
				return raw, ok
			})
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) || v != nil {
					t.Fatalf("want error naming %s, got %v", tc.want, err)
				}
			} else if err != nil || (v != nil) != (tc.env != nil) {
				t.Fatalf("configured = %t, error = %v", v != nil, err)
			}
		})
	}
}

func TestServeChecksOIDCBeforeOpeningDatabase(t *testing.T) {
	args := os.Args
	os.Args = []string{"hrn-server", "serve"}
	t.Cleanup(func() { os.Args = args })
	t.Setenv("HRN_DATABASE_URL", "://invalid")
	t.Setenv("HRN_DATABASE_URL_FILE", "")
	t.Setenv("HRN_OPERATOR_TOKEN", strings.Repeat("o", 32))
	t.Setenv("HRN_OPERATOR_TOKEN_FILE", "")
	t.Setenv("HRN_SCALER_TOKEN", strings.Repeat("s", 32))
	t.Setenv("HRN_SCALER_TOKEN_FILE", "")
	t.Setenv("HRN_LISTEN", "127.0.0.1:8088")
	t.Setenv("HRN_TLS_CERT_FILE", "")
	t.Setenv("HRN_TLS_KEY_FILE", "")
	t.Setenv("HRN_MAX_COST_USD_PER_JOB", "")
	t.Setenv("HRN_REQUIRE_TENANT_BUDGET", "")
	t.Setenv("HRN_OIDC_ISSUER", "http://localhost")
	t.Setenv("HRN_OIDC_AUDIENCE", "api")
	t.Setenv("HRN_OIDC_OPERATOR_CLAIM", "role")
	t.Setenv("HRN_OIDC_OPERATOR_VALUE", "operator")
	t.Setenv("HRN_OIDC_TENANT_CLAIM", "tenants")
	if err := run(); err == nil || !strings.Contains(err.Error(), "HRN_OIDC_ISSUER") || strings.Contains(err.Error(), "localhost") {
		t.Fatalf("run() = %v, want issuer configuration error before database access", err)
	}
}
