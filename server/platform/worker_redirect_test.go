package platform

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWorkerDoesNotForwardProjectTokenAcrossRedirect(t *testing.T) {
	tokenReceived := make(chan string, 1)
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenReceived <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer destination.Close()
	control := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/capture", http.StatusTemporaryRedirect)
	}))
	defer control.Close()
	worker := &Worker{ControlURL: control.URL, Token: "project-token"}
	status, err := worker.call(context.Background(), http.MethodPost, "/internal/claim", nil, nil)
	if status != http.StatusTemporaryRedirect || err == nil {
		t.Fatalf("redirect response: status=%d err=%v", status, err)
	}
	select {
	case received := <-tokenReceived:
		t.Fatalf("project token reached redirect destination: %q", received)
	default:
	}
}
