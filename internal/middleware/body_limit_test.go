package middleware_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/syncronix-labs/aegora/internal/middleware"
)

func TestMaxRequestBody_ContentLength(t *testing.T) {
	const limit = 10

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantNext   bool
	}{
		{name: "under limit", body: strings.Repeat("a", limit-1), wantStatus: http.StatusOK, wantNext: true},
		{name: "exactly at limit", body: strings.Repeat("a", limit), wantStatus: http.StatusOK, wantNext: true},
		{name: "one byte over limit", body: strings.Repeat("a", limit+1), wantStatus: http.StatusRequestEntityTooLarge, wantNext: false},
		{name: "empty body", body: "", wantStatus: http.StatusOK, wantNext: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			handler := middleware.MaxRequestBody(limit)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				if _, err := io.ReadAll(r.Body); err != nil {
					t.Errorf("reading body under limit: %v", err)
				}
				w.WriteHeader(http.StatusOK)
			}))

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader(tt.body))
			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if called != tt.wantNext {
				t.Fatalf("next handler called = %v, want %v", called, tt.wantNext)
			}
			if tt.wantStatus == http.StatusRequestEntityTooLarge {
				if got := rec.Header().Get("Content-Type"); got != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", got)
				}
				if got := rec.Body.String(); got != `{"error":"request body too large"}` {
					t.Errorf("body = %q, want request body too large JSON", got)
				}
			}
		})
	}
}

func TestMaxRequestBody_UnknownLengthOverLimit(t *testing.T) {
	const limit = 10

	var readErr error
	handler := middleware.MaxRequestBody(limit)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader(strings.Repeat("a", limit+5)))
	req.ContentLength = -1 // simulate chunked transfer encoding
	handler.ServeHTTP(httptest.NewRecorder(), req)

	var mbe *http.MaxBytesError
	if !errors.As(readErr, &mbe) {
		t.Fatalf("read error = %v, want *http.MaxBytesError", readErr)
	}
	if mbe.Limit != limit {
		t.Errorf("MaxBytesError.Limit = %d, want %d", mbe.Limit, limit)
	}
}

func TestMaxRequestBody_DisabledWhenNonPositive(t *testing.T) {
	for _, limit := range []int64{0, -1} {
		called := false
		handler := middleware.MaxRequestBody(limit)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
			if _, err := io.ReadAll(r.Body); err != nil {
				t.Errorf("limit %d: reading body: %v", limit, err)
			}
			w.WriteHeader(http.StatusOK)
		}))

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader(strings.Repeat("a", 1<<20)))
		handler.ServeHTTP(rec, req)

		if !called || rec.Code != http.StatusOK {
			t.Fatalf("limit %d: called = %v, status = %d; want passthrough", limit, called, rec.Code)
		}
	}
}
