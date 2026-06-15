package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsManager(t *testing.T) {
	cases := map[string]bool{"owner": true, "admin": true, "member": false, "": false, "viewer": false}
	for role, want := range cases {
		if got := isManager(role); got != want {
			t.Errorf("isManager(%q) = %v, want %v", role, got, want)
		}
	}
}

func TestMaskToken(t *testing.T) {
	if got := maskToken("abcdef0123456789"); got != "abcdef…" {
		t.Errorf("maskToken long = %q, want %q", got, "abcdef…")
	}
	if got := maskToken("short"); got != "••••" {
		t.Errorf("maskToken short = %q, want masked", got)
	}
	// A masked token must never contain the full secret.
	secret := "supersecrettoken1234"
	if maskToken(secret) == secret {
		t.Error("maskToken returned the full secret")
	}
}

func TestOperationStatusForRun(t *testing.T) {
	type want struct {
		status string
		ok     bool
	}
	cases := map[string]want{
		"succeeded": {"in_review", true},
		"failed":    {"blocked", true},
		"blocked":   {"blocked", true},
		"running":   {"", false},
		"queued":    {"", false},
	}
	for state, w := range cases {
		gotStatus, gotOK := operationStatusForRun(state)
		if gotStatus != w.status || gotOK != w.ok {
			t.Errorf("operationStatusForRun(%q) = (%q,%v), want (%q,%v)", state, gotStatus, gotOK, w.status, w.ok)
		}
	}
}

func TestCORSRejectsDisallowedMutationOrigin(t *testing.T) {
	s := &Server{}
	called := false
	h := s.cors(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	req := httptest.NewRequest(http.MethodPost, "http://api.example.test/api/fleets", nil)
	req.Header.Set("Origin", "https://attacker.example.test")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden || called {
		t.Fatalf("status=%d called=%v, want 403 before handler", rec.Code, called)
	}
}

func TestCORSAllowsSameOriginMutationByDefault(t *testing.T) {
	s := &Server{}
	called := false
	h := s.cors(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	req := httptest.NewRequest(http.MethodPost, "http://api.example.test/api/fleets", nil)
	req.Header.Set("Origin", "http://api.example.test")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if !called {
		t.Fatalf("status=%d, same-origin handler was not called", rec.Code)
	}
}

func TestCORSExplicitAllowlistRejectsUnlistedLoopbackOrigin(t *testing.T) {
	s := &Server{allowedOrigins: []string{"https://ufo.example.test"}}
	called := false
	h := s.cors(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	req := httptest.NewRequest(http.MethodPost, "http://localhost:8080/api/fleets", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden || called {
		t.Fatalf("status=%d called=%v, want 403 before handler", rec.Code, called)
	}
}

func TestReadJSONRequiresApplicationJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	var dst map[string]string

	if readJSON(rec, req, &dst) || rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("readJSON=%v status=%d, want false/415", dst, rec.Code)
	}
}

func TestParseDate(t *testing.T) {
	valid, ok := parseDate(ptr("2026-06-13"))
	if !ok || !valid.Valid {
		t.Fatal("valid date rejected")
	}
	if _, ok := parseDate(ptr("06/13/2026")); ok {
		t.Fatal("invalid date accepted")
	}
}

func ptr(s string) *string { return &s }
