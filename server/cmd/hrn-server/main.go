package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
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
	dsn := os.Getenv("HRN_DATABASE_URL")
	if dsn == "" {
		return errors.New("HRN_DATABASE_URL is required")
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
	operator := os.Getenv("HRN_OPERATOR_TOKEN")
	scaler := os.Getenv("HRN_SCALER_TOKEN")
	if len(operator) < 32 || len(scaler) < 32 || operator == scaler {
		return errors.New("independent operator and scaler tokens of at least 32 characters are required")
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
	store, err := platform.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer store.Close()
	server := &http.Server{Addr: address, Handler: (&platform.HTTPServer{Store: store, OperatorToken: operator, ScalerToken: scaler}).Handler(),
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
