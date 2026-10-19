package cache

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewCache_Panics(t *testing.T) {
	tests := []struct {
		name         string
		maxEntries   int
		maxBodyBytes int
		wantPanic    string
	}{
		{
			name:         "zero maxEntries",
			maxEntries:   0,
			maxBodyBytes: 1024,
			wantPanic:    "cache: maxEntries must be positive",
		},
		{
			name:         "negative maxEntries",
			maxEntries:   -5,
			maxBodyBytes: 1024,
			wantPanic:    "cache: maxEntries must be positive",
		},
		{
			name:         "zero maxBodyBytes",
			maxEntries:   100,
			maxBodyBytes: 0,
			wantPanic:    "cache: maxBodyBytes must be positive",
		},
		{
			name:         "negative maxBodyBytes",
			maxEntries:   100,
			maxBodyBytes: -10,
			wantPanic:    "cache: maxBodyBytes must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("expected panic %q, got nil", tt.wantPanic)
				}
				msg, ok := r.(string)
				if !ok || msg != tt.wantPanic {
					t.Fatalf("panic message = %v, want %q", r, tt.wantPanic)
				}
			}()

			_ = NewCache(tt.maxEntries, tt.maxBodyBytes)
		})
	}
}

func TestCache_PutGetRoundtrip(t *testing.T) {
	c := NewCache(10, 1024)

	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	header.Set("X-Custom", "value1")

	entry := &Entry{
		Status:    http.StatusOK,
		Header:    header,
		Body:      []byte(`{"key":"value"}`),
		ExpiresAt: time.Now().Add(time.Minute),
		ETag:      `"test-etag"`,
	}

	c.Put("GET /api/v1?test=1", entry)

	got, hit := c.Get("GET /api/v1?test=1")
	if !hit {
		t.Fatal("expected cache hit, got miss")
	}

	if got.Status != http.StatusOK {
		t.Fatalf("status = %d, want %d", got.Status, http.StatusOK)
	}
	if got.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got.Header.Get("Content-Type"))
	}
	if string(got.Body) != `{"key":"value"}` {
		t.Fatalf("body = %q, want %q", string(got.Body), `{"key":"value"}`)
	}
	if got.ETag != `"test-etag"` {
		t.Fatalf("ETag = %q, want %q", got.ETag, `"test-etag"`)
	}

	_, hitMissing := c.Get("GET /missing")
	if hitMissing {
		t.Fatal("expected miss for non-existent key")
	}
}

func TestCache_LazyExpiry(t *testing.T) {
	c := NewCache(10, 1024)

	entry := &Entry{
		Status:    http.StatusOK,
		Header:    make(http.Header),
		Body:      []byte("ephemeral"),
		ExpiresAt: time.Now().Add(10 * time.Millisecond),
	}

	c.Put("GET /fast-expire?", entry)

	// Sleep past expiration
	time.Sleep(25 * time.Millisecond)

	got, hit := c.Get("GET /fast-expire?")
	if hit || got != nil {
		t.Fatalf("expected lazy expiry, got hit=%v, entry=%v", hit, got)
	}

	// Verify key was lazily deleted from map
	c.mu.Lock()
	_, exists := c.entries["GET /fast-expire?"]
	c.mu.Unlock()

	if exists {
		t.Fatal("expected key to be lazily deleted from map on read")
	}
}

func TestCache_EvictionAtCapacity(t *testing.T) {
	c := NewCache(2, 1024)

	e1 := &Entry{Status: http.StatusOK, ExpiresAt: time.Now().Add(time.Minute)}
	e2 := &Entry{Status: http.StatusOK, ExpiresAt: time.Now().Add(time.Minute)}
	e3 := &Entry{Status: http.StatusOK, ExpiresAt: time.Now().Add(time.Minute)}

	c.Put("key1", e1)
	c.Put("key2", e2)

	c.mu.Lock()
	if len(c.entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(c.entries))
	}
	c.mu.Unlock()

	// Inserting 3rd entry triggers eviction of an arbitrary entry
	c.Put("key3", e3)

	c.mu.Lock()
	count := len(c.entries)
	_, has3 := c.entries["key3"]
	c.mu.Unlock()

	if count != 2 {
		t.Fatalf("expected 2 entries after eviction, got %d", count)
	}
	if !has3 {
		t.Fatal("expected newly added key3 to exist")
	}
}

func TestCache_InvalidateAndInvalidatePrefix(t *testing.T) {
	c := NewCache(10, 1024)

	e := &Entry{Status: http.StatusOK, ExpiresAt: time.Now().Add(time.Minute)}
	c.Put("GET /api/v1/users?", e)
	c.Put("GET /api/v1/posts?", e)
	c.Put("GET /api/v2/items?", e)

	// Invalidate specific key
	c.Invalidate("GET /api/v2/items?")
	if _, hit := c.Get("GET /api/v2/items?"); hit {
		t.Fatal("expected GET /api/v2/items? to be invalidated")
	}

	// Invalidate prefix
	c.InvalidatePrefix("GET /api/v1")
	if _, hit := c.Get("GET /api/v1/users?"); hit {
		t.Fatal("expected GET /api/v1/users? to be invalidated")
	}
	if _, hit := c.Get("GET /api/v1/posts?"); hit {
		t.Fatal("expected GET /api/v1/posts? to be invalidated")
	}
}

func TestCache_ConcurrentRace(t *testing.T) {
	t.Parallel()

	c := NewCache(50, 1024)
	const goroutines = 16
	const ops = 100

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < ops; j++ {
				key := fmt.Sprintf("GET /path/%d?", (workerID+j)%10)
				if j%2 == 0 {
					c.Put(key, &Entry{
						Status:    http.StatusOK,
						Body:      []byte("data"),
						ExpiresAt: time.Now().Add(time.Second),
					})
				} else {
					_, _ = c.Get(key)
				}
			}
		}(i)
	}

	wg.Wait()
}

func TestCacheMiddleware_HitAndBackendCallCount(t *testing.T) {
	store := NewCache(10, 1024)
	var callCount atomic.Int32

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("response-data"))
	})

	mw := CacheMiddleware(store, time.Minute, 1024)
	handler := mw(backend)

	// 1. First GET: Cache MISS, backend executed
	req1 := httptest.NewRequest(http.MethodGet, "/resource?id=123", nil)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", rec1.Code)
	}
	if rec1.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("first X-Cache = %q, want MISS", rec1.Header().Get("X-Cache"))
	}
	if rec1.Body.String() != "response-data" {
		t.Fatalf("first body = %q, want response-data", rec1.Body.String())
	}
	if calls := callCount.Load(); calls != 1 {
		t.Fatalf("backend calls after miss = %d, want 1", calls)
	}

	// 2. Second GET: Cache HIT, backend NOT executed, byte-identical body
	req2 := httptest.NewRequest(http.MethodGet, "/resource?id=123", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200", rec2.Code)
	}
	if rec2.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("second X-Cache = %q, want HIT", rec2.Header().Get("X-Cache"))
	}
	if rec2.Body.String() != rec1.Body.String() {
		t.Fatalf("second body = %q, want identical to first %q", rec2.Body.String(), rec1.Body.String())
	}
	if calls := callCount.Load(); calls != 1 {
		t.Fatalf("backend calls after hit = %d, want 1", calls)
	}
}

func TestCacheMiddleware_NormalizesQueryParameterOrder(t *testing.T) {
	store := NewCache(10, 1024)
	var callCount atomic.Int32
	handler := CacheMiddleware(store, time.Minute, 1024)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		_, _ = w.Write([]byte("response-data"))
	}))

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/resource?b=2&a=1&a=3", nil))
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/resource?a=1&a=3&b=2", nil))

	if first.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("first X-Cache = %q, want MISS", first.Header().Get("X-Cache"))
	}
	if second.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("second X-Cache = %q, want HIT", second.Header().Get("X-Cache"))
	}
	if calls := callCount.Load(); calls != 1 {
		t.Fatalf("backend calls = %d, want 1", calls)
	}
}

func TestCacheMiddleware_POSTBypasses(t *testing.T) {
	store := NewCache(10, 1024)
	var callCount atomic.Int32

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("posted"))
	})

	mw := CacheMiddleware(store, time.Minute, 1024)
	handler := mw(backend)

	// POST request 1
	req1 := httptest.NewRequest(http.MethodPost, "/resource", strings.NewReader("payload"))
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	if rec1.Header().Get("X-Cache") != "" {
		t.Fatalf("expected no X-Cache header on POST, got %q", rec1.Header().Get("X-Cache"))
	}

	// POST request 2
	req2 := httptest.NewRequest(http.MethodPost, "/resource", strings.NewReader("payload"))
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Header().Get("X-Cache") != "" {
		t.Fatalf("expected no X-Cache header on POST, got %q", rec2.Header().Get("X-Cache"))
	}
	if calls := callCount.Load(); calls != 2 {
		t.Fatalf("backend calls for 2 POSTs = %d, want 2 (bypass)", calls)
	}
}

func TestCacheMiddleware_AuthorizationBypasses(t *testing.T) {
	store := NewCache(10, 1024)
	var callCount atomic.Int32

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("authenticated"))
	})

	mw := CacheMiddleware(store, time.Minute, 1024)
	handler := mw(backend)

	// 1. GET with Authorization header
	req1 := httptest.NewRequest(http.MethodGet, "/private", nil)
	req1.Header.Set("Authorization", "Bearer token-123")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	if rec1.Header().Get("X-Cache") != "" {
		t.Fatalf("expected no X-Cache on Authorization bypass, got %q", rec1.Header().Get("X-Cache"))
	}

	// 2. Second GET with Authorization: should run backend again (never stored)
	req2 := httptest.NewRequest(http.MethodGet, "/private", nil)
	req2.Header.Set("Authorization", "Bearer token-123")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if calls := callCount.Load(); calls != 2 {
		t.Fatalf("expected 2 backend calls due to Authorization bypass, got %d", calls)
	}
}

func TestCacheMiddleware_NoStoreNotCached(t *testing.T) {
	store := NewCache(10, 1024)
	var callCount atomic.Int32

	backend := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		if r.URL.Path == "/resp-no-store" {
			w.Header().Set("Cache-Control", "no-store, private")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data"))
	})

	mw := CacheMiddleware(store, time.Minute, 1024)
	handler := mw(backend)

	// Case 1: Backend returns Cache-Control: no-store
	req1 := httptest.NewRequest(http.MethodGet, "/resp-no-store", nil)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("expected X-Cache MISS, got %q", rec1.Header().Get("X-Cache"))
	}

	// Repeat identical GET: must still be MISS and call backend again
	req2 := httptest.NewRequest(http.MethodGet, "/resp-no-store", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("expected X-Cache MISS on second call, got %q", rec2.Header().Get("X-Cache"))
	}
	if calls := callCount.Load(); calls != 2 {
		t.Fatalf("backend calls = %d, want 2", calls)
	}

	// Case 2: Request sends Cache-Control: no-store
	req3 := httptest.NewRequest(http.MethodGet, "/req-no-store", nil)
	req3.Header.Set("Cache-Control", "no-store")
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)

	req4 := httptest.NewRequest(http.MethodGet, "/req-no-store", nil)
	rec4 := httptest.NewRecorder()
	handler.ServeHTTP(rec4, req4)
	if rec4.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("expected X-Cache MISS after request no-store, got %q", rec4.Header().Get("X-Cache"))
	}
}

func TestCacheMiddleware_SetCookieNotCached(t *testing.T) {
	store := NewCache(10, 1024)
	var callCount atomic.Int32

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		w.Header().Set("Set-Cookie", "session=secret123; HttpOnly")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("user-data"))
	})

	mw := CacheMiddleware(store, time.Minute, 1024)
	handler := mw(backend)

	req1 := httptest.NewRequest(http.MethodGet, "/profile", nil)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	req2 := httptest.NewRequest(http.MethodGet, "/profile", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("expected X-Cache MISS on Set-Cookie response, got %q", rec2.Header().Get("X-Cache"))
	}
	if calls := callCount.Load(); calls != 2 {
		t.Fatalf("backend calls = %d, want 2 (Set-Cookie prevented caching)", calls)
	}
}

func TestCacheMiddleware_OversizedBodyStreamsAndNotStored(t *testing.T) {
	store := NewCache(10, 10) // 10 bytes limit
	var callCount atomic.Int32

	largePayload := "this-is-a-large-payload-exceeding-ten-bytes"

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(largePayload))
	})

	mw := CacheMiddleware(store, time.Minute, 10)
	handler := mw(backend)

	// First request: streams oversized body through to client
	req1 := httptest.NewRequest(http.MethodGet, "/large", nil)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec1.Code)
	}
	if rec1.Body.String() != largePayload {
		t.Fatalf("streamed body mismatch: got %q, want %q", rec1.Body.String(), largePayload)
	}

	// Second request: must run backend again because oversized body was not stored
	req2 := httptest.NewRequest(http.MethodGet, "/large", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("expected X-Cache MISS for oversized response, got %q", rec2.Header().Get("X-Cache"))
	}
	if calls := callCount.Load(); calls != 2 {
		t.Fatalf("backend calls = %d, want 2 (oversized response not cached)", calls)
	}
}

func TestCacheMiddleware_ETagAndIfNoneMatch(t *testing.T) {
	store := NewCache(10, 1024)

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"version":1}`))
	})

	mw := CacheMiddleware(store, time.Minute, 1024)
	handler := mw(backend)

	// 1. Initial GET: Backend sent no ETag, so middleware must generate one (W/"<hex>")
	req1 := httptest.NewRequest(http.MethodGet, "/version", nil)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	etag := rec1.Header().Get("ETag")
	if etag == "" {
		t.Fatal("expected generated ETag header, got empty")
	}
	if !strings.HasPrefix(etag, "W/\"") || !strings.HasSuffix(etag, "\"") {
		t.Fatalf("expected weak ETag W/\"...\", got %q", etag)
	}
	// Check length: W/" + 16 hex chars + " = 20 chars
	if len(etag) != 20 {
		t.Fatalf("expected 16 hex chars in weak ETag (%d total chars), got %q (len %d)", 20, etag, len(etag))
	}

	// 2. Second GET with matching If-None-Match -> 304 Not Modified, empty body, X-Cache: HIT
	req2 := httptest.NewRequest(http.MethodGet, "/version", nil)
	req2.Header.Set("If-None-Match", etag)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304 Not Modified", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Fatalf("expected empty body for 304, got %q", rec2.Body.String())
	}
	if rec2.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("expected X-Cache HIT on 304, got %q", rec2.Header().Get("X-Cache"))
	}
	if rec2.Header().Get("ETag") != etag {
		t.Fatalf("expected ETag %q preserved on 304, got %q", etag, rec2.Header().Get("ETag"))
	}
}

func TestCacheMiddleware_BackendETagPreserved(t *testing.T) {
	store := NewCache(10, 1024)

	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"upstream-etag-1234"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("content"))
	})

	mw := CacheMiddleware(store, time.Minute, 1024)
	handler := mw(backend)

	req := httptest.NewRequest(http.MethodGet, "/etag-test", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("ETag"); got != `"upstream-etag-1234"` {
		t.Fatalf("expected backend ETag preserved, got %q", got)
	}

	// Verify cached entry has it
	entry, hit := store.Get("GET /etag-test?")
	if !hit {
		t.Fatal("expected cache hit")
	}
	if entry.ETag != `"upstream-etag-1234"` {
		t.Fatalf("entry.ETag = %q, want \"upstream-etag-1234\"", entry.ETag)
	}
}
