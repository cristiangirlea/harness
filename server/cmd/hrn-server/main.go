package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/cristiangirlea/harness/server/platform"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	dsn, err := platform.SecretEnv("HRN_DATABASE_URL")
	if err != nil {
		return err
	}
	if dsn == "" {
		return errors.New("HRN_DATABASE_URL (or HRN_DATABASE_URL_FILE) is required")
	}
	if len(os.Args) != 2 {
		return errors.New("usage: hrn-server migrate|serve")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if os.Args[1] == "migrate" {
		return platform.Migrate(ctx, dsn)
	}
	if os.Args[1] != "serve" {
		return errors.New("usage: hrn-server migrate|serve")
	}
	operator, err := platform.SecretEnv("HRN_OPERATOR_TOKEN")
	if err != nil {
		return err
	}
	scaler, err := platform.SecretEnv("HRN_SCALER_TOKEN")
	if err != nil {
		return err
	}
	if len(operator) < 32 || len(scaler) < 32 || operator == scaler {
		return errors.New("independent operator and scaler tokens of at least 32 characters are required")
	}
	metrics, err := platform.SecretEnv("HRN_METRICS_TOKEN")
	if err != nil {
		return err
	}
	if metrics != "" && (len(metrics) < 32 || metrics == operator || metrics == scaler) {
		return errors.New("HRN_METRICS_TOKEN must be at least 32 characters and differ from the operator and scaler tokens")
	}
	address := os.Getenv("HRN_LISTEN")
	if address == "" {
		address = "127.0.0.1:8088"
	}
	certFile := os.Getenv("HRN_TLS_CERT_FILE")
	keyFile := os.Getenv("HRN_TLS_KEY_FILE")
	if err := validateListen(address, certFile, keyFile); err != nil {
		return err
	}
	maxCost, err := maxCostPerJob(os.Getenv("HRN_MAX_COST_USD_PER_JOB"))
	if err != nil {
		return err
	}
	budgetRequired, err := requireTenantBudget(os.Getenv("HRN_REQUIRE_TENANT_BUDGET"))
	if err != nil {
		return err
	}
	oidc, err := oidcFromEnv(os.LookupEnv)
	if err != nil {
		return err
	}
	store, err := platform.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer store.Close()
	server := &http.Server{Addr: address, Handler: (&platform.HTTPServer{
		Store: store, OperatorToken: operator, ScalerToken: scaler, MetricsToken: metrics, OIDC: oidc, MaxCostUSDPerJob: maxCost, RequireTenantBudget: budgetRequired,
	}).Handler(),
		ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 16 * 1024}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := store.MarkExpired(ctx); err != nil {
					log.Printf("lease reconciliation: %v", err)
				}
				if _, err := store.AdmitOne(ctx); err != nil {
					log.Printf("job admission: %v", err)
				}
			}
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	log.Printf("hrn control service listening on %s", address)
	if certFile != "" {
		err = server.ListenAndServeTLS(certFile, keyFile)
	} else {
		err = server.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("serve: %w", err)
}

func oidcFromEnv(lookup func(string) (string, bool)) (*platform.OIDCVerifier, error) {
	var config platform.OIDCConfig
	for _, setting := range []struct {
		name   string
		target *string
	}{
		{"HRN_OIDC_ISSUER", &config.Issuer},
		{"HRN_OIDC_AUDIENCE", &config.Audience},
		{"HRN_OIDC_OPERATOR_CLAIM", &config.OperatorClaim},
		{"HRN_OIDC_OPERATOR_VALUE", &config.OperatorValue},
		{"HRN_OIDC_TENANT_CLAIM", &config.TenantClaim},
	} {
		value, set := lookup(setting.name)
		if set && value == "" {
			return nil, fmt.Errorf("%s must not be empty", setting.name)
		}
		*setting.target = value
	}
	return platform.NewOIDCVerifier(config)
}

func maxCostPerJob(raw string) (float64, error) {
	if raw == "" {
		return 0, nil
	}
	limit, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(limit) || math.IsInf(limit, 0) || limit <= 0 {
		return 0, fmt.Errorf("HRN_MAX_COST_USD_PER_JOB must be a positive number of USD, got %q", raw)
	}
	return limit, nil
}

func requireTenantBudget(raw string) (bool, error) {
	switch raw {
	case "":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("HRN_REQUIRE_TENANT_BUDGET must be empty or 1, got %q", raw)
	}
}

func validateListen(address, certFile, keyFile string) error {
	if (certFile == "") != (keyFile == "") {
		return errors.New("HRN_TLS_CERT_FILE and HRN_TLS_KEY_FILE must be set together")
	}
	if certFile != "" {
		return nil
	}
	host, _, err := net.SplitHostPort(address)
	if err == nil {
		ip := net.ParseIP(host)
		if ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return errors.New("HRN_LISTEN must use a loopback IP literal without TLS; set both HRN_TLS_CERT_FILE and HRN_TLS_KEY_FILE for other addresses")
}
