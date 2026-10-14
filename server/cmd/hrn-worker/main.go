package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cristiangirlea/harness/internal/agent"
	"github.com/cristiangirlea/harness/server/platform"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

func run() error {
	control := os.Getenv("HRN_CONTROL_URL")
	modelURL := os.Getenv("HRN_FIXTURE_API_URL")
	projectID := os.Getenv("HRN_PROJECT_ID")
	workerToken := os.Getenv("HRN_WORKER_TOKEN")
	workspace := os.Getenv("HRN_WORKSPACE_ROOT")
	if control == "" || modelURL == "" || projectID == "" || workerToken == "" || workspace == "" {
		return errors.New("HRN_CONTROL_URL, HRN_FIXTURE_API_URL, HRN_PROJECT_ID, HRN_WORKER_TOKEN and HRN_WORKSPACE_ROOT are required")
	}
	parsed, err := url.Parse(control)
	if err != nil {
		return err
	}
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopback(parsed.Hostname())) {
		return errors.New("control service must use HTTPS or loopback HTTP")
	}
	parsed, err = url.Parse(modelURL)
	if err != nil {
		return err
	}
	if parsed.Scheme != "http" || !isLoopback(parsed.Hostname()) || parsed.User != nil {
		return errors.New("initial worker supports only a local fake API endpoint")
	}
	id, err := randomWorkerID()
	if err != nil {
		return err
	}
	backend := agent.NewAPIBackend()
	backend.BaseURL = modelURL
	backend.APIKey = "fixture-only"
	backend.Tools = nil
	worker := &platform.Worker{ControlURL: control, ProjectID: projectID, Token: workerToken,
		WorkerID: id, WorkspaceRoot: workspace, Backend: backend}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := waitForFixture(ctx, modelURL); err != nil {
		return err
	}
	return worker.Run(ctx)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func randomWorkerID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return "w_" + hex.EncodeToString(bytes[:]), nil
}

func waitForFixture(parent context.Context, origin string) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/healthz", nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("local fixture model did not become ready: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
