package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

const (
	maxBodyBytes  = 1 << 20
	healthTTL     = 5 * time.Second
	healthTimeout = 5 * time.Second
	readTimeout   = 30 * time.Second
	writeSlack    = 15 * time.Second
)

// HealthFunc reports per-source health; a nil error means healthy.
type HealthFunc func(ctx context.Context) map[string]error

// Options configures New.
type Options struct {
	Addr           string
	MCP            http.Handler // served at /mcp behind the middleware chain
	Health         HealthFunc   // may be nil (always healthy)
	Token          string       // bearer token for /mcp and detailed /healthz
	AllowedOrigins []string
	MaxConcurrent  int           // concurrent /mcp requests
	QueryTimeout   time.Duration // sizes WriteTimeout (a response may wait for a query)
}

// New assembles the HTTP server:
//
//	/mcp     OriginCheck -> BearerAuth -> BodyLimit(1MB) -> Limit -> MCP
//
// BodyLimit is behind auth on purpose: a client without the token must not be
// able to make the server read (or wait for) its body.
//
//	/healthz public {"status":...}; with a valid bearer it adds per-source state
func New(o Options) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/mcp", OriginCheck(o.AllowedOrigins,
		BearerAuth(o.Token,
			BodyLimit(maxBodyBytes,
				Limit(o.MaxConcurrent, o.MCP)))))
	mux.Handle("/healthz", healthz(&healthCache{fn: o.Health, ttl: healthTTL, now: time.Now}, o.Token))
	return &http.Server{
		Addr:              o.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// Responses are plain JSON (no SSE), so a request must arrive in full
		// within ReadTimeout and the whole exchange fits in the query timeout
		// plus slack.
		ReadTimeout:  readTimeout,
		WriteTimeout: o.QueryTimeout + writeSlack,
		IdleTimeout:  2 * time.Minute,
	}
}

// healthCache memoizes the Health result for ttl so a probe loop cannot turn
// into a ping storm against the databases.
type healthCache struct {
	fn  HealthFunc
	ttl time.Duration
	now func() time.Time

	mu   sync.Mutex
	at   time.Time
	res  map[string]error
	have bool
}

func (c *healthCache) get(ctx context.Context) map[string]error {
	if c.fn == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.have && c.now().Sub(c.at) < c.ttl {
		return c.res
	}
	// The result is shared by later callers, so one caller going away must
	// not poison it: detach from ctx cancellation and bound the call instead.
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), healthTimeout)
	defer cancel()
	c.res, c.at, c.have = c.fn(hctx), c.now(), true
	return c.res
}

func healthz(c *healthCache, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		res := c.get(r.Context())
		status, code := "ok", http.StatusOK
		for _, err := range res {
			if err != nil {
				status, code = "degraded", http.StatusServiceUnavailable
				break
			}
		}
		body := map[string]any{"status": status}
		if validBearer(r, token) {
			sources := make(map[string]string, len(res))
			for name, err := range res {
				sources[name] = "ok"
				if err != nil {
					sources[name] = "down"
				}
			}
			body["sources"] = sources
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	})
}
