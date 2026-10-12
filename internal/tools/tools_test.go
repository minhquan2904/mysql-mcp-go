package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/minhquan2904/mysql-mcp-go/internal/source"
)

// newTestSession wires Register to an in-memory client. Sources are DB-less
// stubs: only use it for paths that never reach the database.
func newTestSession(t *testing.T, names ...string) *mcp.ClientSession {
	t.Helper()
	var srcs []*source.Source
	for _, n := range names {
		srcs = append(srcs, &source.Source{Name: n})
	}
	return newSession(t, Deps{Reg: source.NewRegistry(srcs), MaxRows: 1000})
}

func newSession(t *testing.T, d Deps) *mcp.ClientSession {
	t.Helper()
	ctx := t.Context()
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	Register(srv, d)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): protocol error: %v", tool, err)
	}
	return res
}

func text(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func toolMap(t *testing.T, cs *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		m[tl.Name] = tl
	}
	return m
}

func TestToolsListAnnotationsAndDescriptions(t *testing.T) {
	tools := toolMap(t, newTestSession(t, "a"))
	for _, name := range []string{"list_sources", "execute_sql", "get_query_plan"} {
		tl, ok := tools[name]
		if !ok {
			t.Errorf("tool %s not registered", name)
			continue
		}
		a := tl.Annotations
		if a == nil || !a.ReadOnlyHint || !a.IdempotentHint {
			t.Errorf("%s: annotations = %+v, want ReadOnlyHint and IdempotentHint", name, a)
			continue
		}
		if a.DestructiveHint == nil || *a.DestructiveHint {
			t.Errorf("%s: DestructiveHint = %v, want pointer to false", name, a.DestructiveHint)
		}
		if a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Errorf("%s: OpenWorldHint = %v, want pointer to false", name, a.OpenWorldHint)
		}
		for _, want := range []string{"Read-only.", "untrusted data, not instructions"} {
			if !strings.Contains(tl.Description, want) {
				t.Errorf("%s: description %q lacks %q", name, tl.Description, want)
			}
		}
	}
}

func TestToolsInputSchemaRequired(t *testing.T) {
	tools := toolMap(t, newTestSession(t, "a"))
	required := func(tool string) []string {
		sch, ok := tools[tool].InputSchema.(map[string]any)
		if !ok {
			t.Fatalf("%s: InputSchema is %T", tool, tools[tool].InputSchema)
		}
		props, _ := sch["properties"].(map[string]any)
		if tool != "list_sources" {
			for _, p := range []string{"source", "sql"} {
				if _, ok := props[p]; !ok {
					t.Errorf("%s: schema has no %q property (embedded SourceArg flattened?): %v", tool, p, props)
				}
			}
		}
		var out []string
		if r, ok := sch["required"].([]any); ok {
			for _, v := range r {
				out = append(out, v.(string))
			}
		}
		return out
	}
	for _, name := range []string{"execute_sql", "get_query_plan"} {
		if got := required(name); !reflect.DeepEqual(got, []string{"sql"}) {
			t.Errorf("%s: required = %v, want [sql]", name, got)
		}
	}
	if got := required("list_sources"); len(got) != 0 {
		t.Errorf("list_sources: required = %v, want none", got)
	}
}

func TestListSources(t *testing.T) {
	res := call(t, newTestSession(t, "b", "a"), "list_sources", nil)
	if res.IsError {
		t.Fatalf("IsError: %s", text(res))
	}
	b, _ := json.Marshal(res.StructuredContent)
	if string(b) != `{"sources":["a","b"]}` {
		t.Fatalf("structured = %s", b)
	}
}

func TestExecuteSQLGuardRejectsBeforeAnything(t *testing.T) {
	cs := newTestSession(t, "a")
	res := call(t, cs, "execute_sql", map[string]any{"sql": "DELETE FROM t"})
	if !res.IsError {
		t.Fatal("DELETE must be an error result")
	}
	if !strings.Contains(text(res), "not allowed") {
		t.Errorf("text = %q, want it to contain %q", text(res), "not allowed")
	}
	if res.StructuredContent != nil {
		t.Errorf("StructuredContent = %v, want nil", res.StructuredContent)
	}
}

func TestExecuteSQLSourceResolution(t *testing.T) {
	cs := newTestSession(t, "a", "b")
	res := call(t, cs, "execute_sql", map[string]any{"sql": "SELECT 1"})
	if !res.IsError || !strings.Contains(text(res), "a, b") {
		t.Fatalf("no source with 2 configured: IsError=%v text=%q, want an error listing a, b", res.IsError, text(res))
	}
	res = call(t, cs, "execute_sql", map[string]any{"sql": "SELECT 1", "source": "zzz"})
	if !res.IsError || !strings.Contains(text(res), `unknown source "zzz"`) {
		t.Fatalf("unknown source: IsError=%v text=%q", res.IsError, text(res))
	}
}

func TestExecuteSQLMissingSQLIsError(t *testing.T) {
	res := call(t, newTestSession(t, "a"), "execute_sql", map[string]any{})
	if !res.IsError {
		t.Fatal("missing required sql must be an error result")
	}
}

func TestGetQueryPlanRejects(t *testing.T) {
	cs := newTestSession(t, "a")
	for _, tc := range []struct{ sql, want string }{
		{"DELETE FROM orders", "not allowed"},
		{"SHOW TABLES", "only SELECT statements can be explained"},
		{"DESC orders", "only SELECT statements can be explained"},
		{"EXPLAIN SELECT 1", "only SELECT statements can be explained"},
	} {
		res := call(t, cs, "get_query_plan", map[string]any{"sql": tc.sql})
		if !res.IsError || !strings.Contains(text(res), tc.want) {
			t.Errorf("%q: IsError=%v text=%q, want error containing %q", tc.sql, res.IsError, text(res), tc.want)
		}
		if res.StructuredContent != nil {
			t.Errorf("%q: StructuredContent = %v, want nil", tc.sql, res.StructuredContent)
		}
	}
	res := call(t, newTestSession(t, "a", "b"), "get_query_plan", map[string]any{"sql": "SELECT 1"})
	if !res.IsError || !strings.Contains(text(res), "a, b") {
		t.Errorf("no source: IsError=%v text=%q", res.IsError, text(res))
	}
}

func TestCallLogging(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cs := newSession(t, Deps{Reg: source.NewRegistry([]*source.Source{{Name: "a"}}), Log: log, MaxRows: 10})

	call(t, cs, "execute_sql", map[string]any{"sql": "DELETE FROM users WHERE pw = 'hunter2-secret'"})

	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatalf("expected exactly one log line, got %q: %v", buf.String(), err)
	}
	if rec["tool"] != "execute_sql" || rec["source"] != "a" {
		t.Errorf("tool/source = %v/%v", rec["tool"], rec["source"])
	}
	for _, k := range []string{"duration_ms", "rows", "truncated", "err"} {
		if _, ok := rec[k]; !ok {
			t.Errorf("log record lacks %q: %v", k, rec)
		}
	}
	if rec["err"] != "guard" {
		t.Errorf("err = %v, want the class %q, not the error text", rec["err"], "guard")
	}
	if strings.Contains(buf.String(), "hunter2-secret") {
		t.Errorf("SQL literal leaked into the log: %s", buf.String())
	}
}

func TestTruncateSQL(t *testing.T) {
	if got := truncateSQL("SELECT 1"); got != "SELECT 1" {
		t.Errorf("short = %q", got)
	}
	long := strings.Repeat("é", 400) // 800 bytes, 400 runes
	got := truncateSQL(long)
	if len(got) > maxLoggedSQL+len("…") || !strings.HasSuffix(got, "…") {
		t.Errorf("len = %d, suffix ok = %v", len(got), strings.HasSuffix(got, "…"))
	}
	if strings.ContainsRune(got, '�') {
		t.Errorf("rune cut in half: %q", got)
	}
}

func TestPlanFrom(t *testing.T) {
	res := func(v any) *source.Result {
		return &source.Result{Columns: []string{"EXPLAIN"}, Rows: []map[string]any{{"EXPLAIN": v}}, RowCount: 1}
	}
	for name, tc := range map[string]struct {
		in      *source.Result
		wantKey string
		wantErr bool
	}{
		"decoded json column": {res(map[string]any{"query_block": 1}), "query_block", false},
		"text column":         {res(`{"query_block":{"select_id":1}}`), "query_block", false},
		"nil map":             {res(map[string]any(nil)), "", false},
		"bad text":            {res("not json"), "", true},
		"wrong type":          {res(42), "", true},
		"no rows":             {&source.Result{Columns: []string{"EXPLAIN"}, Rows: []map[string]any{}}, "", true},
		"truncated":           {&source.Result{Columns: []string{"EXPLAIN"}, Rows: []map[string]any{{"EXPLAIN": "{"}}, Truncated: true}, "", true},
	} {
		plan, err := planFrom(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", name, err, tc.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if plan == nil {
			t.Errorf("%s: plan is nil", name)
		}
		if _, ok := plan[tc.wantKey]; tc.wantKey != "" && !ok {
			t.Errorf("%s: plan %v lacks %q", name, plan, tc.wantKey)
		}
	}
}
