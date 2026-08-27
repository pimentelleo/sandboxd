package main

import (
	"net/http"
	"testing"
	"time"
)

func TestNewCopilotBridgeServerBoundsRequestReads(t *testing.T) {
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	server := newCopilotBridgeServer(handler)

	if server.Handler == nil {
		t.Fatal("bridge handler is nil")
	}
	if got, want := server.ReadHeaderTimeout, 5*time.Second; got != want {
		t.Fatalf("ReadHeaderTimeout = %s, want %s", got, want)
	}
	if got, want := server.ReadTimeout, 15*time.Second; got != want {
		t.Fatalf("ReadTimeout = %s, want %s", got, want)
	}
	if got, want := server.IdleTimeout, 60*time.Second; got != want {
		t.Fatalf("IdleTimeout = %s, want %s", got, want)
	}
}
