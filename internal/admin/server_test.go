package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecurityHeadersContainBrowserBoundaries(t *testing.T) {
	server := &Server{}
	handler := server.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "https://example.test/", nil)
	request.Header.Set("X-Forwarded-Proto", "https")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	csp := response.Header().Get("Content-Security-Policy")
	for _, want := range []string{
		"default-src 'self'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
		"object-src 'none'",
		"script-src 'self'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("Content-Security-Policy %q is missing %q", csp, want)
		}
	}
	if got := response.Header().Get("Strict-Transport-Security"); got == "" {
		t.Error("missing Strict-Transport-Security on a secure request")
	}
}

func TestSecurityHeadersOmitHSTSOverPlainHTTP(t *testing.T) {
	server := &Server{}
	handler := server.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if got := response.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("unexpected Strict-Transport-Security over plain HTTP: %q", got)
	}
}
