package copilot

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBridgeReturnsOnlySafeErrorEnvelope(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	manager := newTestManager(t, &now, &fakeRuntime{}, nil)
	secret := "prompt-content-that-must-not-be-an-error"
	request := httptest.NewRequest(http.MethodPost, bridgeTaskPath,
		bytes.NewBufferString(`{"capability":"01234567890123456789012345678901","prompt":"`+secret+`","model":"","continue":false,"system_prompt":""}`))
	response := httptest.NewRecorder()
	manager.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("response = %d, headers = %#v", response.Code, response.Header())
	}
	if strings.Contains(body, secret) || strings.Contains(body, "01234567890123456789012345678901") {
		t.Fatalf("private request data leaked in bridge response: %s", body)
	}
	if body != "{\"type\":\"error\",\"message\":\"task authorization is invalid or expired\"}\n" {
		t.Fatalf("unsafe bridge response: %s", body)
	}
}

func TestBridgeRejectsUnknownFields(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	manager := newTestManager(t, &now, &fakeRuntime{}, nil)
	request := httptest.NewRequest(http.MethodPost, bridgeCancelPath,
		bytes.NewBufferString(`{"capability":"01234567890123456789012345678901","sandbox_id":"never accepted"}`))
	response := httptest.NewRecorder()
	manager.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("response code = %d", response.Code)
	}
}
