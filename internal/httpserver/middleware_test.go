package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
})

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestBearerAuth(t *testing.T) {
	const tok = "0123456789abcdef0123456789abcdef"
	h := BearerAuth(tok, okHandler)
	tests := []struct {
		name, header string
		want         int
	}{
		{"no header", "", 401},
		{"wrong token", "Bearer wrong", 401},
		{"prefix of token", "Bearer " + tok[:10], 401},
		{"token with suffix", "Bearer " + tok + "x", 401},
		{"basic scheme", "Basic " + tok, 401},
		{"bare token", tok, 401},
		{"lowercase scheme", "bearer " + tok, 200},
		{"canonical scheme", "Bearer " + tok, 200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/mcp", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			w := serve(h, r)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
			if tc.want == 401 && w.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want Bearer", w.Header().Get("WWW-Authenticate"))
			}
			if tc.want == 401 && w.Header().Get("Connection") != "close" {
				t.Errorf("Connection = %q, want close (so an unread body cannot delay the 401)", w.Header().Get("Connection"))
			}
		})
	}
}

func TestOriginCheck(t *testing.T) {
	h := OriginCheck([]string{"http://ok.example"}, okHandler)
	tests := []struct {
		name, origin string
		set          bool
		want         int
	}{
		{"no origin header", "", false, 200},
		{"evil origin", "http://evil.com", true, 403},
		{"allowed origin", "http://ok.example", true, 200},
		{"empty origin header", "", true, 403},
		{"case differs", "http://OK.example", true, 403},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/mcp", nil)
			if tc.set {
				r.Header.Set("Origin", tc.origin)
			}
			if w := serve(h, r); w.Code != tc.want {
				t.Fatalf("status = %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestOriginCheckNoAllowList(t *testing.T) {
	h := OriginCheck(nil, okHandler)
	r := httptest.NewRequest("POST", "/mcp", nil)
	r.Header.Set("Origin", "http://anything")
	if w := serve(h, r); w.Code != 403 {
		t.Fatalf("status = %d, want 403 (empty allow-list rejects every Origin)", w.Code)
	}
	if w := serve(h, httptest.NewRequest("POST", "/mcp", nil)); w.Code != 200 {
		t.Fatalf("no Origin: status = %d, want 200", w.Code)
	}
}

func TestBodyLimit(t *testing.T) {
	const limit = 1 << 20
	var readErr error
	reader := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, readErr = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK) // deliberately ignores readErr
	})
	h := BodyLimit(limit, reader)

	t.Run("at limit passes", func(t *testing.T) {
		readErr = nil
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(strings.Repeat("a", limit)))
		if w := serve(h, r); w.Code != 200 || readErr != nil {
			t.Fatalf("status = %d, readErr = %v", w.Code, readErr)
		}
	})
	t.Run("over limit with Content-Length", func(t *testing.T) {
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(strings.Repeat("a", limit+1)))
		if w := serve(h, r); w.Code != 413 {
			t.Fatalf("status = %d, want 413", w.Code)
		}
	})
	t.Run("over limit without Content-Length fails the read", func(t *testing.T) {
		readErr = nil
		r := httptest.NewRequest("POST", "/mcp", io.NopCloser(strings.NewReader(strings.Repeat("a", limit+1))))
		r.ContentLength = -1
		serve(h, r)
		var mbe *http.MaxBytesError
		if !errors.As(readErr, &mbe) {
			t.Fatalf("read error = %v, want *http.MaxBytesError", readErr)
		}
	})
	t.Run("never reads the body itself", func(t *testing.T) {
		// A trickling client must not be able to hold BodyLimit in a read.
		pr, pw := io.Pipe()
		defer pw.Close()
		r := httptest.NewRequest("POST", "/mcp", pr)
		r.ContentLength = -1
		done := make(chan struct{})
		go func() {
			serve(BodyLimit(limit, okHandler), r) // okHandler never reads
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("BodyLimit blocked on the request body")
		}
	})
	t.Run("unknown length within limit is delivered intact", func(t *testing.T) {
		var got string
		echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			got = string(b)
		})
		r := httptest.NewRequest("POST", "/mcp", io.NopCloser(strings.NewReader("hello")))
		r.ContentLength = -1
		if w := serve(BodyLimit(limit, echo), r); w.Code != 200 || got != "hello" {
			t.Fatalf("status = %d, body = %q", w.Code, got)
		}
	})
}

func TestLimitSerializes(t *testing.T) {
	var (
		mu          sync.Mutex
		cur, maxCur int
	)
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cur++
		maxCur = max(maxCur, cur)
		mu.Unlock()
		entered <- struct{}{}
		<-release
		mu.Lock()
		cur--
		mu.Unlock()
	})
	h := Limit(1, slow)

	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() { serve(h, httptest.NewRequest("POST", "/mcp", nil)) })
	}
	<-entered // first request is inside the handler
	select {
	case <-entered:
		t.Fatal("second request entered while the first was still running")
	case <-time.After(150 * time.Millisecond):
	}
	release <- struct{}{} // finish the first; the second may now enter
	<-entered
	release <- struct{}{}
	wg.Wait()
	if maxCur != 1 {
		t.Fatalf("max concurrent handlers = %d, want 1", maxCur)
	}
}

func TestLimitWaitTimeout(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	inside := make(chan struct{})
	h := limit(1, 50*time.Millisecond, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inside)
		<-block
	}))
	go serve(h, httptest.NewRequest("POST", "/mcp", nil))
	<-inside
	w := serve(h, httptest.NewRequest("POST", "/mcp", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 after the wait budget", w.Code)
	}
}

func TestLimitCanceledWhileWaiting(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	inside := make(chan struct{})
	h := limit(1, time.Minute, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(inside)
		<-block
	}))
	go serve(h, httptest.NewRequest("POST", "/mcp", nil))
	<-inside
	ctx, cancel := context.WithCancel(t.Context())
	r := httptest.NewRequest("POST", "/mcp", nil).WithContext(ctx)
	done := make(chan int)
	go func() { done <- serve(h, r).Code }()
	select {
	case c := <-done:
		t.Fatalf("returned %d before the slot freed or the request was canceled", c)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case c := <-done:
		if c != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not return after its request was canceled")
	}
}
