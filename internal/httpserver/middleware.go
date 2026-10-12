// Package httpserver holds the HTTP hardening in front of the MCP handler:
// body limit, Origin check, bearer auth, concurrency limit and /healthz.
package httpserver

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"slices"
	"strings"
	"time"
)

// limitWait is how long a request may queue for a concurrency slot.
const limitWait = 30 * time.Second

// BodyLimit rejects a request that declares a body larger than n bytes with
// 413 and caps what the handler can read at n bytes (reads past the cap fail
// with *http.MaxBytesError). It never reads the body itself, so it cannot be
// held up by a slow client; it must run after authentication.
func BodyLimit(n int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > n {
			refuse(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, n)
		next.ServeHTTP(w, r)
	})
}

// OriginCheck answers 403 to a request whose Origin header is present but not
// in allowed (exact match). Requests without Origin (non-browser clients)
// pass. With an empty list every browser Origin is refused.
func OriginCheck(allowed []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origins, ok := r.Header["Origin"]; ok {
			if len(origins) != 1 || !slices.Contains(allowed, origins[0]) {
				refuse(w, "origin not allowed", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// BearerAuth requires "Authorization: Bearer <token>" (scheme is
// case-insensitive) and answers 401 otherwise.
func BearerAuth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validBearer(r, token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			refuse(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// validBearer reports whether r carries the bearer token. The comparison is
// constant-time and does not leak the token length (both sides are hashed).
func validBearer(r *http.Request, token string) bool {
	scheme, got, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// Limit lets at most n requests run next at once; a request that cannot get a
// slot within 30s (or whose client went away) gets 503.
func Limit(n int, next http.Handler) http.Handler { return limit(n, limitWait, next) }

func limit(n int, wait time.Duration, next http.Handler) http.Handler {
	sem := make(chan struct{}, n)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case sem <- struct{}{}:
		default:
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case sem <- struct{}{}:
			case <-timer.C:
				busy(w)
				return
			case <-r.Context().Done():
				busy(w)
				return
			}
		}
		defer func() { <-sem }()
		next.ServeHTTP(w, r)
	})
}

func busy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	refuse(w, "server busy", http.StatusServiceUnavailable)
}

// refuse answers a request that was turned away before its body was read.
// "Connection: close" matters: without it net/http first tries to drain the
// unread body (up to 256KB) before it sends the response, so a client that
// trickles a chunked body would hold the answer back until the read timeout.
func refuse(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Connection", "close")
	http.Error(w, msg, code)
}
