package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHostDispatchRoutesProductionPreviewBeforeAPI(t *testing.T) {
	const previewHost = "s-01abc-3000.preview.example.test"
	previewCalls := 0
	apiCalls := 0
	preview := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		previewCalls++
		w.WriteHeader(http.StatusNoContent)
	})
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		apiCalls++
		w.WriteHeader(http.StatusTeapot)
	})
	handler := hostDispatch(nil, preview, func(host string) bool {
		return host == previewHost
	}, api, nil)

	request := httptest.NewRequest(http.MethodGet, "https://"+previewHost+"/", nil)
	request.Host = previewHost
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("preview route status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if previewCalls != 1 || apiCalls != 0 {
		t.Fatalf("host dispatch calls: preview=%d api=%d", previewCalls, apiCalls)
	}
}

func TestHostDispatchRoutesNonPreviewToAPI(t *testing.T) {
	apiCalls := 0
	api := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		apiCalls++
		w.WriteHeader(http.StatusNoContent)
	})
	handler := hostDispatch(nil, http.NotFoundHandler(), func(string) bool {
		return false
	}, api, nil)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://api.example.test/healthz", nil))

	if response.Code != http.StatusNoContent || apiCalls != 1 {
		t.Fatalf("API route status/calls = %d/%d", response.Code, apiCalls)
	}
}

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
