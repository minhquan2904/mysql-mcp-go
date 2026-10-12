//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type server struct {
	addr string
	logs *syncBuffer
	done chan error
	stop context.CancelFunc
}

func envDSN(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("%s not set (run through scripts/dev.sh it)", key)
	}
	return v
}

// start runs the real run() on an ephemeral loopback port and waits for the
// "ready" log line. Extra env entries are appended.
func start(t *testing.T, dsn string, extra ...string) *server {
	t.Helper()
	env := append([]string{
		"MCP_AUTH_TOKEN=" + goodToken,
		"MYSQL_DSN_TESTDB=" + dsn,
		"LISTEN_ADDR=127.0.0.1:0",
	}, extra...)
	ctx, cancel := context.WithCancel(t.Context())
	s := &server{logs: &syncBuffer{}, done: make(chan error, 1), stop: cancel}
	go func() { s.done <- run(ctx, env, s.logs) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-s.done:
		case <-time.After(15 * time.Second):
			t.Error("run did not return after cancel")
		}
	})

	deadline := time.After(20 * time.Second)
	for {
		select {
		case err := <-s.done:
			t.Fatalf("run returned before ready: %v\nlogs: %s", err, s.logs)
		case <-deadline:
			t.Fatalf("no ready line:\n%s", s.logs)
		case <-time.After(20 * time.Millisecond):
		}
		for line := range strings.SplitSeq(s.logs.String(), "\n") {
			var rec struct {
				Msg, Addr string
				StartupMS *int64 `json:"startup_ms"`
			}
			if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "ready" {
				if rec.StartupMS == nil {
					t.Fatalf("ready line lacks startup_ms: %s", line)
				}
				s.addr = rec.Addr
				return s
			}
		}
	}
}

type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func do(t *testing.T, method, url, bearer, origin string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(`{}`))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// The whole stack over a real socket: middleware, Streamable HTTP handler,
// tools, source, database.
func TestEndToEndOverHTTP(t *testing.T) {
	s := start(t, envDSN(t, "IT_DSN_RO"))
	base := "http://" + s.addr

	t.Run("healthz", func(t *testing.T) {
		code, body := do(t, "GET", base+"/healthz", "", "")
		if code != 200 || strings.TrimSpace(body) != `{"status":"ok"}` {
			t.Errorf("public: %d %s", code, body)
		}
		code, body = do(t, "GET", base+"/healthz", goodToken, "")
		if code != 200 || !strings.Contains(body, `"testdb":"ok"`) {
			t.Errorf("with token: %d %s", code, body)
		}
	})

	t.Run("hardening", func(t *testing.T) {
		if code, _ := do(t, "POST", base+"/mcp", "", ""); code != 401 {
			t.Errorf("no token: %d, want 401", code)
		}
		if code, _ := do(t, "POST", base+"/mcp", "wrong", ""); code != 401 {
			t.Errorf("wrong token: %d, want 401", code)
		}
		if code, _ := do(t, "POST", base+"/mcp", goodToken, "http://evil.com"); code != 403 {
			t.Errorf("evil origin: %d, want 403", code)
		}
		post := func(bearer string, body io.Reader, chunked bool) int {
			req, _ := http.NewRequest("POST", base+"/mcp", body)
			if chunked {
				req.ContentLength = -1
			}
			if bearer != "" {
				req.Header.Set("Authorization", "Bearer "+bearer)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			return resp.StatusCode
		}
		big := func() io.Reader { return strings.NewReader(`{"x":"` + strings.Repeat("a", 1<<20) + `"}`) }
		if code := post("", big(), false); code != 401 {
			t.Errorf("1MB+ body without token: %d, want 401", code)
		}
		if code := post(goodToken, big(), false); code != 413 {
			t.Errorf("1MB+ body with token: %d, want 413", code)
		}
		if code := post(goodToken, io.NopCloser(big()), true); code != 413 {
			t.Errorf("1MB+ chunked body with token: %d, want 413", code)
		}
	})

	t.Run("mcp client", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		client := mcp.NewClient(&mcp.Implementation{Name: "it", Version: "0"}, nil)
		cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:             base + "/mcp",
			HTTPClient:           &http.Client{Transport: bearerTransport{goodToken}},
			DisableStandaloneSSE: true,
		}, nil)
		if err != nil {
			t.Fatalf("connect: %v\nlogs: %s", err, s.logs)
		}
		defer cs.Close()

		list, err := cs.ListTools(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, tl := range list.Tools {
			names[tl.Name] = true
			if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
				t.Errorf("%s: not readOnlyHint", tl.Name)
			}
		}
		for _, want := range []string{"list_sources", "execute_sql", "get_query_plan"} {
			if !names[want] {
				t.Errorf("tools/list lacks %s: %v", want, names)
			}
		}

		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "execute_sql", Arguments: map[string]any{"sql": "SELECT 1 AS one"}})
		if err != nil || res.IsError {
			t.Fatalf("execute_sql: err=%v res=%+v", err, res)
		}
		b, _ := json.Marshal(res.StructuredContent)
		if string(b) != `{"columns":["one"],"row_count":1,"rows":[{"one":1}],"truncated":false}` {
			t.Errorf("structured = %s", b)
		}

		res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "execute_sql", Arguments: map[string]any{"sql": "DROP TABLE customers"}})
		if err != nil || !res.IsError {
			t.Errorf("DROP TABLE: err=%v res=%+v, want IsError", err, res)
		}

		res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_query_plan", Arguments: map[string]any{"sql": "SELECT * FROM orders"}})
		if err != nil || res.IsError {
			t.Fatalf("get_query_plan: err=%v res=%+v", err, res)
		}
		if plan, _ := res.StructuredContent.(map[string]any)["plan"].(map[string]any); plan["query_block"] == nil {
			t.Errorf("plan = %v", res.StructuredContent)
		}
	})

	for _, secret := range []string{goodToken, os.Getenv("MCP_RO_PASSWORD")} {
		if secret != "" && strings.Contains(s.logs.String(), secret) {
			t.Errorf("a secret leaked into the logs")
		}
	}
}

func TestRunGracefulShutdown(t *testing.T) {
	s := start(t, envDSN(t, "IT_DSN_RO"))
	s.stop()
	select {
	case err := <-s.done:
		if err != nil {
			t.Fatalf("run = %v, want nil after cancel", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no graceful shutdown")
	}
	s.done <- nil // satisfy the cleanup receive
}

func TestRunRefusesPrivilegedAccount(t *testing.T) {
	var buf syncBuffer
	err := run(t.Context(), []string{
		"MCP_AUTH_TOKEN=" + goodToken,
		"MYSQL_DSN_TESTDB=" + envDSN(t, "IT_DSN_ROOT"),
		"LISTEN_ADDR=127.0.0.1:0",
	}, &buf)
	if err == nil || !strings.Contains(err.Error(), "privilege") {
		t.Fatalf("err = %v, want a privilege self-check failure", err)
	}
	if !strings.Contains(buf.String(), "privilege") || strings.Contains(buf.String(), `"msg":"ready"`) {
		t.Errorf("logs: %s", buf.String())
	}
}

func TestRunAllowUnsafePrivilegesWarnsAndStarts(t *testing.T) {
	s := start(t, envDSN(t, "IT_DSN_ROOT"), "ALLOW_UNSAFE_PRIVILEGES=true")
	logs := s.logs.String()
	if !strings.Contains(logs, `"level":"WARN"`) || !strings.Contains(logs, "ALLOW_UNSAFE_PRIVILEGES") {
		t.Errorf("no WARN about the privileges: %s", logs)
	}
}

func TestRunListenAddressInUse(t *testing.T) {
	first := start(t, envDSN(t, "IT_DSN_RO"))
	var buf syncBuffer
	err := run(t.Context(), []string{
		"MCP_AUTH_TOKEN=" + goodToken,
		"MYSQL_DSN_TESTDB=" + envDSN(t, "IT_DSN_RO"),
		"LISTEN_ADDR=" + first.addr,
	}, &buf)
	if err == nil || !strings.Contains(buf.String(), "cannot listen") {
		t.Fatalf("err = %v logs = %s", err, buf.String())
	}
}
