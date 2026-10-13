// Command bench measures the end-to-end latency and throughput of the MCP tool
// calls of the read-only MySQL MCP server and prints one JSON row per cell (see
// Row in stats.go for the fields).
//
// A cell is one scenario at one concurrency. Each cell runs a warmup window
// whose calls are discarded, then the measured window; every worker holds its
// own go-sdk client session over the Streamable HTTP transport, so the numbers
// include the HTTP round trip, the SQL guard, the read-only transaction and the
// serialization of the result, but not the connection setup.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// callTimeout bounds one tool call so a hung server cannot hang the bench. It
// is far above any query the server itself allows to finish.
const callTimeout = 2 * time.Minute

// scenario is one load scenario. wantErr marks the scenario whose calls must
// all be rejected: a call counts as an error when it does not end the way its
// scenario expects.
type scenario struct {
	id      string
	desc    string
	tool    string
	args    map[string]any
	wantErr bool
}

// s4SQL returns 1000 rows as long as order_items holds at least 32 rows: the
// self cross join yields one row per pair and the LIMIT caps it. The seed data
// has ~122 rows in order_items, so the cap is what limits the result.
const s4SQL = `SELECT a.order_id AS order_id, a.line_no AS line_no, a.sku AS sku, a.qty AS qty,
       b.order_id AS other_order_id, b.line_no AS other_line_no
FROM order_items AS a CROSS JOIN order_items AS b
LIMIT 1000`

// scenarios is the load table of the benchmark plan, in order.
var scenarios = []scenario{
	{id: "S1", desc: "list_sources", tool: "list_sources", args: map[string]any{}},
	{id: "S2", desc: "execute_sql SELECT 1", tool: "execute_sql", args: map[string]any{"sql": "SELECT 1"}},
	{id: "S3", desc: "execute_sql SELECT * FROM orders", tool: "execute_sql", args: map[string]any{"sql": "SELECT * FROM orders"}},
	{id: "S4", desc: "execute_sql 1000 rows (order_items self cross join)", tool: "execute_sql", args: map[string]any{"sql": s4SQL}},
	{id: "S5", desc: "list_tables detailed", tool: "list_tables", args: map[string]any{"output_format": "detailed"}},
	{id: "S6", desc: "execute_sql DELETE FROM it_scratch (guard rejection)", tool: "execute_sql", args: map[string]any{"sql": "DELETE FROM it_scratch"}, wantErr: true},
}

type options struct {
	url         string
	token       string
	scenarios   []scenario
	concurrency []int
	duration    time.Duration
	warmup      time.Duration
	out         string
}

func main() {
	opts, err := parseFlags(os.Args[1:], os.Getenv("MCP_AUTH_TOKEN"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(2)
	}
	emit, closeOut, err := newRowWriter(opts.out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
	defer closeOut()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, opts, os.Stderr, emit); err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func parseFlags(args []string, envToken string) (options, error) {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: bench [flags]

Measures MCP tool-call latency and throughput against the read-only MySQL MCP
server. One JSON row is printed per cell (one scenario at one concurrency):

  {"scenario","concurrency","n","rps","p50_ms","p95_ms","p99_ms","max_ms","errors"}

Scenarios:
  S1  list_sources
  S2  execute_sql SELECT 1
  S3  execute_sql SELECT * FROM orders
  S4  execute_sql 1000 rows (order_items self cross join)
  S5  list_tables detailed
  S6  execute_sql DELETE FROM it_scratch (guard rejection, no database round trip)

Each cell runs -warmup first (discarded), then measures for -duration with one
client session per worker. Rows go to stdout as JSON lines with -out json, or
are appended to a file when -out is a path.

Flags:
`)
		fs.PrintDefaults()
	}
	url := fs.String("url", "http://127.0.0.1:8080/mcp", "`URL` of the MCP endpoint")
	token := fs.String("token", envToken, "bearer `token` for the MCP endpoint (default $MCP_AUTH_TOKEN)")
	scenFlag := fs.String("scenario", "all", "comma-separated scenario IDs (S1..S6) or `all`")
	concFlag := fs.String("concurrency", "1", "comma-separated worker `counts`; one cell per value and scenario")
	duration := fs.Duration("duration", 30*time.Second, "measured `window` per cell")
	warmup := fs.Duration("warmup", 5*time.Second, "warmup `window` per cell, discarded (0 disables)")
	out := fs.String("out", "json", "`destination` of the JSON rows: \"json\" for stdout, or a file path to append to")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	if fs.NArg() > 0 {
		return options{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	var opts options
	opts.url = *url
	opts.token = *token
	opts.duration = *duration
	opts.warmup = *warmup
	opts.out = *out
	if opts.url == "" {
		return options{}, fmt.Errorf("-url must not be empty")
	}
	if opts.token == "" {
		return options{}, fmt.Errorf("no bearer token: pass -token or set MCP_AUTH_TOKEN")
	}
	if opts.duration <= 0 {
		return options{}, fmt.Errorf("-duration must be positive")
	}
	if opts.warmup < 0 {
		return options{}, fmt.Errorf("-warmup must not be negative")
	}
	if opts.out == "" {
		return options{}, fmt.Errorf("-out must not be empty")
	}
	var err error
	if opts.scenarios, err = parseScenarios(*scenFlag); err != nil {
		return options{}, err
	}
	if opts.concurrency, err = parseConcurrency(*concFlag); err != nil {
		return options{}, err
	}
	return opts, nil
}

// parseScenarios resolves a comma-separated list of scenario IDs, or "all".
func parseScenarios(s string) ([]scenario, error) {
	if strings.EqualFold(strings.TrimSpace(s), "all") {
		return scenarios, nil
	}
	byID := make(map[string]scenario, len(scenarios))
	for _, sc := range scenarios {
		byID[sc.id] = sc
	}
	var out []scenario
	for _, id := range strings.Split(s, ",") {
		id = strings.ToUpper(strings.TrimSpace(id))
		sc, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("unknown scenario %q: use S1..S6 or all", id)
		}
		out = append(out, sc)
	}
	return out, nil
}

func parseConcurrency(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		n, err := strconv.Atoi(part)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("bad concurrency %q: want positive integers", part)
		}
		out = append(out, n)
	}
	return out, nil
}

// newRowWriter returns the sink for the JSON rows (one object per line) and the
// function closing it.
func newRowWriter(out string) (emit func(Row) error, closeOut func() error, err error) {
	if out == "json" {
		enc := json.NewEncoder(os.Stdout)
		return func(r Row) error { return enc.Encode(r) }, func() error { return nil }, nil
	}
	f, err := os.OpenFile(out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}
	enc := json.NewEncoder(f)
	return func(r Row) error { return enc.Encode(r) }, f.Close, nil
}

// run measures every selected scenario at every requested concurrency and emits
// one row per cell as soon as the cell is done. Progress goes to progress, so
// the JSON output stays parseable.
func run(ctx context.Context, opts options, progress io.Writer, emit func(Row) error) error {
	for _, sc := range opts.scenarios {
		for _, concurrency := range opts.concurrency {
			if err := ctx.Err(); err != nil {
				return err
			}
			fmt.Fprintf(progress, "bench: %s %s @ c=%d (warmup %s, then %s)\n",
				sc.id, sc.desc, concurrency, opts.warmup, opts.duration)
			lat, errs, kinds, elapsed, err := measureCell(ctx, opts, sc, concurrency, progress)
			if err != nil {
				return fmt.Errorf("%s @ c=%d: %w", sc.id, concurrency, err)
			}
			row := NewRow(sc.id, concurrency, lat, errs, elapsed)
			fmt.Fprintf(progress, "bench: %s @ c=%d: n=%d rps=%.1f p50=%.1fms p95=%.1fms p99=%.1fms max=%.1fms errors=%d\n",
				sc.id, concurrency, row.N, row.RPS, row.P50MS, row.P95MS, row.P99MS, row.MaxMS, row.Errors)
			if rate := ErrorRate(errs, row.N); rate > 0 {
				fmt.Fprintf(progress, "bench: %s @ c=%d: %.1f%% of the calls failed (%d/%d)\n",
					sc.id, concurrency, 100*rate, errs, row.N)
				for _, k := range topKinds(kinds, 3) {
					fmt.Fprintf(progress, "bench: %s @ c=%d:   %d× %s\n", sc.id, concurrency, k.n, k.msg)
				}
			}
			if err := emit(row); err != nil {
				return err
			}
		}
	}
	return nil
}

// probeTimeout bounds the retries of the one call that checks the scenario's
// expected behaviour. The server refuses /mcp requests above
// MAX_CONCURRENT_QUERIES and can hold a refusal for up to 30s, so a busy window
// must not fail the cell.
const probeTimeout = 45 * time.Second

// measureCell runs one cell: it opens one client session per worker, checks
// that the scenario behaves as expected, runs the warmup window whose calls are
// thrown away, then measures. The sessions stay open across both windows, so
// the measured calls reuse what the warmup opened and no connection setup lands
// in a latency.
func measureCell(ctx context.Context, opts options, sc scenario, concurrency int, progress io.Writer) ([]time.Duration, int, map[string]int, time.Duration, error) {
	sessions, err := connect(ctx, opts, concurrency)
	if err != nil {
		return nil, 0, nil, 0, err
	}
	defer func() {
		for _, sess := range sessions {
			sess.Close()
		}
	}()
	deadline := time.Now().Add(probeTimeout)
	for {
		_, err := callOnce(ctx, sc, sessions[0])
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return nil, 0, nil, 0, fmt.Errorf("probe call for %s: %w", sc.id, err)
		}
		fmt.Fprintf(progress, "bench: %s @ c=%d: probe retry: %v\n", sc.id, concurrency, err)
		time.Sleep(500 * time.Millisecond)
	}
	if opts.warmup > 0 {
		blast(ctx, sessions, sc, opts.warmup)
	}
	lat, failed, kinds, elapsed := blast(ctx, sessions, sc, opts.duration)
	return lat, failed, kinds, elapsed, nil
}

// blast has every worker call the tool until the window is over. It returns the
// latency of every completed call (failed ones included), how many of them
// failed, the reason of each failure keyed by message, and the wall time of the
// window including the drain of the calls that were still running when it
// closed.
func blast(ctx context.Context, sessions []*mcp.ClientSession, sc scenario, window time.Duration) ([]time.Duration, int, map[string]int, time.Duration) {
	windowCtx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	lat := make([][]time.Duration, len(sessions))
	errs := make([]int, len(sessions))
	kinds := map[string]int{}
	start := time.Now()
	for i := range sessions {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for windowCtx.Err() == nil {
				// The call itself is not tied to the window: one still running
				// when the window closes must be measured to its end.
				d, err := callOnce(ctx, sc, sessions[i])
				lat[i] = append(lat[i], d)
				if err != nil {
					errs[i]++
					mu.Lock()
					kinds[shortErr(err)]++
					mu.Unlock()
				}
			}
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	var all []time.Duration
	var failed int
	for i := range lat {
		all = append(all, lat[i]...)
		failed += errs[i]
	}
	return all, failed, kinds, elapsed
}

// callOnce times one tool call. It returns the call's latency and nil when the
// call ended the way its scenario expects, and the reason otherwise.
func callOnce(ctx context.Context, sc scenario, sess *mcp.ClientSession) (time.Duration, error) {
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	start := time.Now()
	res, err := sess.CallTool(callCtx, &mcp.CallToolParams{Name: sc.tool, Arguments: sc.args})
	d := time.Since(start)
	if err != nil {
		return d, err
	}
	if res.IsError != sc.wantErr {
		return d, fmt.Errorf("unexpected tool result (IsError=%v, want %v): %s", res.IsError, sc.wantErr, toolError(res))
	}
	return d, nil
}

// toolError renders the text of a tool result: that is where the server puts
// the message of an isError result.
func toolError(res *mcp.CallToolResult) string {
	parts := make([]string, 0, len(res.Content))
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	if len(parts) == 0 {
		return "(no text content)"
	}
	return strings.Join(parts, " ")
}

// shortErr renders an error as one comparable line: errors that differ only in
// a variable part (a duration, a port) must not split the tally.
func shortErr(err error) string {
	const max = 200
	msg := strings.Join(strings.Fields(err.Error()), " ")
	if len(msg) > max {
		msg = msg[:max] + "..."
	}
	return msg
}

// kindCount is one failure reason and how often it happened.
type kindCount struct {
	msg string
	n   int
}

// topKinds returns at most n failure reasons, most frequent first.
func topKinds(kinds map[string]int, n int) []kindCount {
	top := make([]kindCount, 0, n)
	for msg, count := range kinds {
		top = append(top, kindCount{msg: msg, n: count})
	}
	for i := 1; i < len(top); i++ {
		for j := i; j > 0 && top[j].n > top[j-1].n; j-- {
			top[j], top[j-1] = top[j-1], top[j]
		}
	}
	if len(top) > n {
		top = top[:n]
	}
	return top
}

// connect opens n client sessions, one per worker. The setup happens outside
// the measured window.
func connect(ctx context.Context, opts options, n int) ([]*mcp.ClientSession, error) {
	sessions := make([]*mcp.ClientSession, 0, n)
	for range n {
		sess, err := newSession(ctx, opts)
		if err != nil {
			for _, s := range sessions {
				s.Close()
			}
			return nil, err
		}
		sessions = append(sessions, sess)
	}
	return sessions, nil
}

// newSession opens one session. Retries are off so that a transient failure
// shows up as an error instead of silently doubling a latency, and the
// standalone SSE stream is off because the server is stateless and answers GET
// with 405.
func newSession(ctx context.Context, opts options) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "mysql-mcp-bench", Version: "1"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:             opts.url,
		HTTPClient:           &http.Client{Transport: &bearerTransport{token: opts.token, base: http.DefaultTransport}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}
	return client.Connect(ctx, transport, nil)
}

// bearerTransport adds the Authorization header to every request.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header = req.Header.Clone()
	r.Header.Set("Authorization", "Bearer "+t.token)
	return t.base.RoundTrip(r)
}
