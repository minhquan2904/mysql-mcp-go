package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "0123456789abcdef0123456789abcdef"

type healthStub struct {
	calls atomic.Int32
	res   map[string]error
}

func (h *healthStub) fn(ctx context.Context) map[string]error {
	h.calls.Add(1)
	return h.res
}

func newHandler(h *healthStub) http.Handler {
	mcp := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	return New(Options{
		Addr: "127.0.0.1:0", MCP: mcp, Health: h.fn, Token: testToken,
		AllowedOrigins: []string{"http://ok.example"}, MaxConcurrent: 2,
	}).Handler
}

func get(h http.Handler, path, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return serve(h, r)
}

func TestHealthzPublicHidesSources(t *testing.T) {
	h := newHandler(&healthStub{res: map[string]error{"a": nil, "b": nil}})
	w := get(h, "/healthz", "")
	if w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"status":"ok"}` {
		t.Fatalf("body = %s", got)
	}
	if strings.Contains(w.Body.String(), `"a"`) || strings.Contains(w.Body.String(), "sources") {
		t.Fatalf("public body leaks source info: %s", w.Body)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestHealthzDegraded(t *testing.T) {
	secret := errors.New("dial tcp: password=hunter2")
	h := newHandler(&healthStub{res: map[string]error{"a": nil, "b": secret}})

	w := get(h, "/healthz", "")
	if w.Code != 503 || strings.TrimSpace(w.Body.String()) != `{"status":"degraded"}` {
		t.Fatalf("public: %d %s", w.Code, w.Body)
	}

	w = get(h, "/healthz", testToken)
	if w.Code != 503 {
		t.Fatalf("authed status = %d", w.Code)
	}
	var body struct {
		Status  string            `json:"status"`
		Sources map[string]string `json:"sources"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "degraded" || body.Sources["a"] != "ok" || body.Sources["b"] != "down" || len(body.Sources) != 2 {
		t.Fatalf("body = %+v", body)
	}
	if strings.Contains(w.Body.String(), "hunter2") || strings.Contains(w.Body.String(), "dial") {
		t.Fatalf("error detail leaked: %s", w.Body)
	}
}

func TestHealthzWrongBearerIsPublic(t *testing.T) {
	h := newHandler(&healthStub{res: map[string]error{"a": nil}})
	w := get(h, "/healthz", "wrong")
	if w.Code != 200 || strings.Contains(w.Body.String(), "sources") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestHealthzCachesFiveSeconds(t *testing.T) {
	st := &healthStub{res: map[string]error{"a": nil}}
	h := newHandler(st)
	get(h, "/healthz", "")
	get(h, "/healthz", testToken)
	if n := st.calls.Load(); n != 1 {
		t.Fatalf("Health called %d times, want 1", n)
	}
}

func TestHealthCacheExpires(t *testing.T) {
	st := &healthStub{res: map[string]error{"a": nil}}
	now := time.Unix(1000, 0)
	c := &healthCache{fn: st.fn, ttl: 5 * time.Second, now: func() time.Time { return now }}
	c.get(context.Background())
	now = now.Add(4900 * time.Millisecond)
	c.get(context.Background())
	if st.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 within ttl", st.calls.Load())
	}
	now = now.Add(200 * time.Millisecond)
	st.res = map[string]error{"a": errors.New("x")}
	res := c.get(context.Background())
	if st.calls.Load() != 2 || res["a"] == nil {
		t.Fatalf("calls = %d res = %v, want refresh after ttl", st.calls.Load(), res)
	}
}

func TestHealthzNoHealthFunc(t *testing.T) {
	h := New(Options{MCP: okHandler, Token: testToken, MaxConcurrent: 1}).Handler
	if w := get(h, "/healthz", ""); w.Code != 200 {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestHealthzMethod(t *testing.T) {
	h := newHandler(&healthStub{})
	r := httptest.NewRequest("POST", "/healthz", nil)
	if w := serve(h, r); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func TestMCPRoutingAndMiddlewareOrder(t *testing.T) {
	h := newHandler(&healthStub{})

	if w := get(h, "/mcp", ""); w.Code != 401 {
		t.Fatalf("no token: %d, want 401", w.Code)
	}
	if w := get(h, "/mcp", testToken); w.Code != 200 {
		t.Fatalf("token: %d, want 200", w.Code)
	}

	// Origin is checked before auth: a bad Origin is 403 even with no token.
	r := httptest.NewRequest("POST", "/mcp", nil)
	r.Header.Set("Origin", "http://evil.com")
	if w := serve(h, r); w.Code != 403 {
		t.Fatalf("evil origin: %d, want 403", w.Code)
	}

	// Auth comes before the body limit, so a client without a token never
	// gets as far as the body: 401 whatever it sends.
	r = httptest.NewRequest("POST", "/mcp", strings.NewReader(strings.Repeat("a", 1<<20+1)))
	if w := serve(h, r); w.Code != 401 {
		t.Fatalf("big body without token: %d, want 401", w.Code)
	}
	// With a token the declared length is refused with 413.
	r = httptest.NewRequest("POST", "/mcp", strings.NewReader(strings.Repeat("a", 1<<20+1)))
	r.Header.Set("Authorization", "Bearer "+testToken)
	if w := serve(h, r); w.Code != 413 {
		t.Fatalf("big body with token: %d, want 413", w.Code)
	}

	if w := get(h, "/nope", testToken); w.Code != 404 {
		t.Fatalf("unknown path: %d, want 404", w.Code)
	}
}

func TestNewTimeouts(t *testing.T) {
	srv := New(Options{Addr: ":0", MCP: okHandler, Token: testToken, MaxConcurrent: 1, QueryTimeout: 20 * time.Second})
	for name, c := range map[string][2]time.Duration{
		"ReadHeaderTimeout": {srv.ReadHeaderTimeout, 10 * time.Second},
		"ReadTimeout":       {srv.ReadTimeout, 30 * time.Second},
		"WriteTimeout":      {srv.WriteTimeout, 35 * time.Second}, // QueryTimeout + 15s
		"IdleTimeout":       {srv.IdleTimeout, 2 * time.Minute},
	} {
		if c[0] != c[1] {
			t.Errorf("%s = %v, want %v", name, c[0], c[1])
		}
	}
	if srv.Addr != ":0" {
		t.Errorf("Addr = %q", srv.Addr)
	}
}

// A client that has no token and trickles a chunked body is answered at once.
func TestUnauthenticatedSlowChunkedBodyGets401(t *testing.T) {
	ts := httptest.NewServer(newHandler(&healthStub{}))
	defer ts.Close()

	pr, pw := io.Pipe()
	defer pw.Close()
	go func() { pw.Write([]byte("x")) }() // one byte, then silence

	req, _ := http.NewRequest("POST", ts.URL+"/mcp", pr)
	req.ContentLength = -1
	client := &http.Client{Timeout: 3 * time.Second}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("no response to a trickling unauthenticated client: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 || time.Since(start) > time.Second {
		t.Fatalf("status = %d after %v, want 401 within 1s", resp.StatusCode, time.Since(start))
	}
}

// An authenticated chunked body over the limit is refused with 413 by the
// handler behind BodyLimit (the MCP handler does this via MaxRequestBodyBytes;
// the stub reads the body the way it does).
func TestAuthenticatedChunkedBodyOverLimit(t *testing.T) {
	reader := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
	})
	h := New(Options{MCP: reader, Token: testToken, MaxConcurrent: 1}).Handler
	r := httptest.NewRequest("POST", "/mcp", io.NopCloser(strings.NewReader(strings.Repeat("a", 1<<20+1))))
	r.ContentLength = -1
	r.Header.Set("Authorization", "Bearer "+testToken)
	if w := serve(h, r); w.Code != 413 {
		t.Fatalf("status = %d, want 413", w.Code)
	}
}
