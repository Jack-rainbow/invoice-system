package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLivenessIsNotDependencyReadiness(t *testing.T) {
	h := NewHandler(Options{})
	for _, tc := range []struct {
		path string
		want int
	}{{"/healthz", 200}, {"/readyz", 503}, {"/api/v1/me", 503}, {"/api/v1/documents", 503}, {"/unregistered", 404}} {
		r := httptest.NewRequest("GET", tc.path, nil)
		r.Header.Set("X-Request-ID", "attacker-controlled")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s got %d want %d", tc.path, w.Code, tc.want)
		}
		if w.Header().Get("Cache-Control") != "private, no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("missing security headers %s", tc.path)
		}
		if w.Header().Get("X-Request-ID") == "attacker-controlled" || len(w.Header().Get("X-Request-ID")) != 32 {
			t.Fatalf("request id not generated: %v", w.Header())
		}
		if tc.path == "/api/v1/me" && (strings.Contains(w.Body.String(), "role") || strings.Contains(w.Body.String(), "balance")) {
			t.Fatalf("unconnected API leaked business state: %s", w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadinessRequiresExplicitSuccessfulChecker(t *testing.T) {
	for _, ready := range []bool{false, true} {
		h := NewHandler(Options{Ready: func(context.Context) bool { return ready }})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
		want := http.StatusServiceUnavailable
		if ready {
			want = http.StatusOK
		}
		if w.Code != want {
			t.Fatalf("ready=%v code=%d", ready, w.Code)
		}
	}
}

func TestReadinessFailureDoesNotExposeInternalError(t *testing.T) {
	h := NewHandler(Options{Ready: func(context.Context) bool { panic("sensitive database details") }})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
	if w.Code != 503 || strings.Contains(w.Body.String(), "sensitive") {
		t.Fatalf("unsafe failure response: %d %s", w.Code, w.Body.String())
	}
}

func TestHealthDoesNotAcceptWrites(t *testing.T) {
	w := httptest.NewRecorder()
	NewHandler(Options{}).ServeHTTP(w, httptest.NewRequest("POST", "/healthz", nil))
	if w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("unexpected write response: %d %v", w.Code, w.Header())
	}
}
