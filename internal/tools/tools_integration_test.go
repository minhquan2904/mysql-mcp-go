//go:build integration

package tools

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	_ "github.com/go-sql-driver/mysql"

	"github.com/minhquan2904/mysql-mcp-go/internal/config"
	"github.com/minhquan2904/mysql-mcp-go/internal/guard"
	"github.com/minhquan2904/mysql-mcp-go/internal/source"
)

var itLimits = source.Limits{
	MaxRows:        1000,
	MaxCellBytes:   64 << 10,
	MaxResultBytes: 4 << 20,
	MaxConcurrent:  4,
	Timeout:        10 * time.Second,
}

func envDSN(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("%s not set (run through scripts/dev.sh it)", key)
	}
	return v
}

// openIT opens a Source whose DSN went through the production allowlist, like
// the server does. It does NOT run CheckPrivileges, so a root DSN is usable.
func openIT(t *testing.T, key, name string) *source.Source {
	t.Helper()
	dsn, _, err := config.NormalizeDSN(envDSN(t, key), itLimits.Timeout)
	if err != nil {
		t.Fatalf("NormalizeDSN(%s): %v", key, err)
	}
	s, err := source.Open(context.Background(), config.Source{Name: name, DSN: dsn}, itLimits, nil)
	if err != nil {
		t.Fatalf("Open(%s): %v", key, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// rootDB is a plain root connection (no read-only session) for out-of-band
// assertions and cleanup.
func rootDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", envDSN(t, "IT_DSN_ROOT"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(2)
	t.Cleanup(func() { db.Close() })
	return db
}

func roDeps(t *testing.T, log *slog.Logger) Deps {
	t.Helper()
	return Deps{Reg: source.NewRegistry([]*source.Source{openIT(t, "IT_DSN_RO", "it")}), Log: log, MaxRows: itLimits.MaxRows}
}

func TestExecuteSQLCustomers(t *testing.T) {
	cs := newSession(t, roDeps(t, nil))
	res := call(t, cs, "execute_sql", map[string]any{"sql": "SELECT * FROM customers ORDER BY id"})
	if res.IsError {
		t.Fatalf("IsError: %s", text(res))
	}
	sc, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent = %T", res.StructuredContent)
	}
	rows, _ := sc["rows"].([]any)
	if len(rows) != 20 || sc["row_count"].(float64) != 20 || sc["truncated"] != false {
		t.Fatalf("rows=%d row_count=%v truncated=%v, want 20/20/false", len(rows), sc["row_count"], sc["truncated"])
	}
	cols, _ := sc["columns"].([]any)
	if len(cols) != 4 || cols[0] != "id" {
		t.Errorf("columns = %v", cols)
	}
}

// Every rejected statement must be refused by the guard itself, not merely
// fail later in the database: assert the guard's message, on both tools.
func TestRejectCasesThroughTools(t *testing.T) {
	cs := newSession(t, roDeps(t, nil))
	for _, tool := range []string{"execute_sql", "get_query_plan"} {
		for _, c := range guard.RejectCases {
			if !utf8.ValidString(c.SQL) {
				continue // JSON cannot carry invalid UTF-8; covered by the guard tests
			}
			res := call(t, cs, tool, map[string]any{"sql": c.SQL})
			if !res.IsError {
				t.Errorf("%s %q passed through: %s", tool, c.SQL, text(res))
				continue
			}
			if !strings.Contains(text(res), "statement not allowed") {
				t.Errorf("%s %q: %q is not the guard's rejection", tool, c.SQL, text(res))
			}
		}
	}
}

func TestExecuteSQLDBErrorIsToolError(t *testing.T) {
	cs := newSession(t, roDeps(t, nil))
	res := call(t, cs, "execute_sql", map[string]any{"sql": "SELECT * FROM no_such_table"})
	if !res.IsError || res.StructuredContent != nil || !strings.Contains(text(res), "no_such_table") {
		t.Fatalf("IsError=%v structured=%v text=%q", res.IsError, res.StructuredContent, text(res))
	}
}

func TestGetQueryPlan(t *testing.T) {
	cs := newSession(t, roDeps(t, nil))

	res := call(t, cs, "get_query_plan", map[string]any{"sql": "SELECT * FROM orders WHERE status='paid'"})
	if res.IsError {
		t.Fatalf("IsError: %s", text(res))
	}
	sc, _ := res.StructuredContent.(map[string]any)
	plan, _ := sc["plan"].(map[string]any)
	if _, ok := plan["query_block"]; !ok {
		t.Fatalf("plan lacks query_block: %v", sc)
	}

	// A UNION in parentheses and a WITH statement are explainable too.
	for _, q := range []string{"(SELECT 1) UNION (SELECT 2)", "WITH c AS (SELECT 1 AS x) SELECT * FROM c"} {
		if r := call(t, cs, "get_query_plan", map[string]any{"sql": q}); r.IsError {
			t.Errorf("%q: %s", q, text(r))
		}
	}

	for _, q := range []string{"DELETE FROM orders", "SHOW TABLES", "EXPLAIN SELECT 1"} {
		if r := call(t, cs, "get_query_plan", map[string]any{"sql": q}); !r.IsError {
			t.Errorf("%q must be an error", q)
		}
	}
}

func TestLoggingNeverCarriesSQLAtInfo(t *testing.T) {
	const literal = "sentinel-literal-xyz"
	run := func(level slog.Level) string {
		var buf bytes.Buffer
		log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level}))
		cs := newSession(t, roDeps(t, log))
		call(t, cs, "execute_sql", map[string]any{"sql": "SELECT '" + literal + "' AS v"})
		// A syntax error: the MySQL message quotes a fragment of the SQL.
		call(t, cs, "execute_sql", map[string]any{"sql": "SELECT '" + literal + "' FROM FROM"})
		return buf.String()
	}

	info := run(slog.LevelInfo)
	if strings.Contains(info, literal) {
		t.Errorf("info log carries SQL text:\n%s", info)
	}
	lines := strings.Split(strings.TrimSpace(info), "\n")
	if len(lines) != 2 {
		t.Fatalf("want one line per call, got %d:\n%s", len(lines), info)
	}
	var rec map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &rec)
	if rec["tool"] != "execute_sql" || rec["source"] != "it" || rec["rows"] != float64(1) || rec["truncated"] != false || rec["err"] != "" {
		t.Errorf("first record = %v", rec)
	}
	_ = json.Unmarshal([]byte(lines[1]), &rec)
	if e, _ := rec["err"].(string); e != "mysql error 1064" {
		t.Errorf("second record err = %q, want %q", e, "mysql error 1064")
	}

	if debug := run(slog.LevelDebug); !strings.Contains(debug, `"sql":"SELECT '`+literal) {
		t.Errorf("debug log lacks the statement:\n%s", debug)
	}
}
