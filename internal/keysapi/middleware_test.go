package keysapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opentela-ai/api/internal/neonauth"
)

type fakeVerifier struct {
	sub string
	err error
}

func (f fakeVerifier) Verify(context.Context, string) (neonauth.Claims, error) {
	return neonauth.Claims{Subject: f.sub}, f.err
}

func TestMiddlewarePassesSubject(t *testing.T) {
	var gotUser string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, _ = UserID(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := Middleware(fakeVerifier{sub: "alice"})(next)

	req := httptest.NewRequest(http.MethodGet, "/manage/keys", nil)
	req.Header.Set("Authorization", "Bearer some.jwt.token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || gotUser != "alice" {
		t.Fatalf("code=%d user=%q, want 200/alice", rec.Code, gotUser)
	}
}

func TestMiddlewareRejects(t *testing.T) {
	cases := []struct {
		name, auth string
		verr       error
	}{
		{"missing", "", nil},
		{"not-bearer", "Basic xyz", nil},
		{"empty-token", "Bearer ", nil},
		{"invalid-jwt", "Bearer bad", errors.New("boom")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := Middleware(fakeVerifier{sub: "alice", err: tc.verr})(
				http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
			req := httptest.NewRequest(http.MethodGet, "/manage/keys", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s: code=%d, want 401", tc.name, rec.Code)
			}
		})
	}
}

func TestMiddlewareEmptySubjectRejected(t *testing.T) {
	// Verify succeeds (no error) but returns an empty subject → must be 401.
	h := Middleware(fakeVerifier{sub: ""})(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	req := httptest.NewRequest(http.MethodGet, "/manage/keys", nil)
	req.Header.Set("Authorization", "Bearer good.jwt.token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d, want 401 for empty subject", rec.Code)
	}
}
