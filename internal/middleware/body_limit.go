package middleware

import (
	"net/http"
)

const bodyTooLargeJSON = `{"error":"request body too large"}`

// MaxRequestBody returns a middleware that rejects request bodies larger than limit bytes
// with 413 Request Entity Too Large.
//
// Enforcement happens in two places:
//   - A declared Content-Length above limit is rejected immediately, before the request
//     reaches the queue or any upstream.
//   - Bodies without a declared length (chunked transfer encoding) are wrapped in
//     http.MaxBytesReader, so reads past limit fail with *http.MaxBytesError. The proxy
//     maps that error to 413 and does not count it against upstream health.
//
// A non-positive limit disables the check.
//
// Intended chain order: after AccessLog and Metrics so rejections are still logged and
// counted, and before Shedding, RateLimit, and Queue so oversized requests never consume
// admission budget or queue slots.
func MaxRequestBody(limit int64) Middleware {
	return func(next http.Handler) http.Handler {
		if limit <= 0 {
			return next
		}

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Connection", "close")
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				_, _ = w.Write([]byte(bodyTooLargeJSON))
				return
			}

			if r.Body != nil && r.Body != http.NoBody {
				r.Body = http.MaxBytesReader(w, r.Body, limit)
			}

			next.ServeHTTP(w, r)
		})
	}
}
