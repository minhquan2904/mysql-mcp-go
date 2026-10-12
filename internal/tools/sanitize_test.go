package tools

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/minhquan2904/mysql-mcp-go/internal/guard"
	"github.com/minhquan2904/mysql-mcp-go/internal/source"
)

func TestSanitize(t *testing.T) {
	const unavailable = "database unavailable"
	passthrough := []struct {
		name  string
		err   error
		class string
	}{
		{"guard", fmt.Errorf("%w: keyword DELETE not allowed", guard.ErrNotReadOnly), "guard"},
		{"timeout", fmt.Errorf("source x: %w", source.ErrQueryTimeout), "timeout"},
		{"resolve", userError{errors.New(`unknown source "zzz"; available: a, b`)}, "user"},
		{"wrapped user", fmt.Errorf("ctx: %w", userError{errors.New("only SELECT statements can be explained")}), "user"},
		{"mysql table missing", &mysql.MySQLError{Number: 1146, Message: "Table 'testdb.nope' doesn't exist"}, "mysql error 1146"},
		{"mysql syntax, wrapped", fmt.Errorf("wrap: %w", &mysql.MySQLError{Number: 1064, Message: "near 'x'"}), "mysql error 1064"},
		{"mysql denied table", &mysql.MySQLError{Number: 1142, Message: "SELECT command denied"}, "mysql error 1142"},
		{"canceled", context.Canceled, "canceled"},
	}
	for _, tc := range passthrough {
		t.Run("passes "+tc.name, func(t *testing.T) {
			pub, class := sanitize(tc.err)
			if pub.Error() != tc.err.Error() {
				t.Errorf("public = %q, want the original %q", pub, tc.err)
			}
			if class != tc.class {
				t.Errorf("class = %q, want %q", class, tc.class)
			}
		})
	}

	hidden := []struct {
		name  string
		err   error
		class string
	}{
		{"access denied", &mysql.MySQLError{Number: 1045, Message: "Access denied for user 'mcp_ro'@'10.1.2.3'"}, "unavailable (mysql error 1045)"},
		{"db access denied", &mysql.MySQLError{Number: 1044, Message: "Access denied for user to database 'x'"}, "unavailable (mysql error 1044)"},
		{"host not allowed", &mysql.MySQLError{Number: 1130, Message: "Host '10.1.2.3' is not allowed"}, "unavailable (mysql error 1130)"},
		{"too many connections", &mysql.MySQLError{Number: 1203, Message: "max_user_connections"}, "unavailable (mysql error 1203)"},
		{"2002", &mysql.MySQLError{Number: 2002, Message: "Can't connect to server on 'secret-host'"}, "unavailable (mysql error 2002)"},
		{"2003", &mysql.MySQLError{Number: 2003, Message: "Can't connect to 'secret-host'"}, "unavailable (mysql error 2003)"},
		{"2005", &mysql.MySQLError{Number: 2005, Message: "Unknown host 'secret-host'"}, "unavailable (mysql error 2005)"},
		{"2013", &mysql.MySQLError{Number: 2013, Message: "Lost connection to 'secret-host'"}, "unavailable (mysql error 2013)"},
		{"net error", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused to secret-host:3306")}, "unavailable (*net.OpError)"},
		{"bad conn", driver.ErrBadConn, "unavailable (*errors.errorString)"},
		{"unknown", errors.New("secret-host exploded"), "unavailable (*errors.errorString)"},
	}
	for _, tc := range hidden {
		t.Run("hides "+tc.name, func(t *testing.T) {
			pub, class := sanitize(tc.err)
			if pub.Error() != unavailable {
				t.Errorf("public = %q, want %q", pub, unavailable)
			}
			if strings.Contains(class, "secret-host") || strings.Contains(class, "10.1.2.3") {
				t.Errorf("class leaks detail: %q", class)
			}
			if class != tc.class {
				t.Errorf("class = %q, want %q", class, tc.class)
			}
		})
	}

	if pub, class := sanitize(nil); pub != nil || class != "" {
		t.Errorf("sanitize(nil) = %v, %q", pub, class)
	}
}

// sessionWith serves one extra tool built from instrument, next to the real ones.
func sessionWith[Out any](t *testing.T, log *slog.Logger, h func(context.Context, noArgs) (Out, *source.Result, error)) *mcp.ClientSession {
	t.Helper()
	d := Deps{Reg: source.NewRegistry([]*source.Source{{Name: "a"}}), Log: log}
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "probe", Description: "probe"}, instrument(d, "probe", h))
	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func TestInstrumentReturnsSanitizedErrorAndLogsClass(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	cs := sessionWith(t, log, func(context.Context, noArgs) (listSourcesOut, *source.Result, error) {
		return listSourcesOut{}, nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused by secret-host:3306")}
	})
	res := call(t, cs, "probe", nil)
	if !res.IsError || text(res) != "database unavailable" {
		t.Fatalf("IsError=%v text=%q, want %q", res.IsError, text(res), "database unavailable")
	}
	if strings.Contains(buf.String(), "secret-host") {
		t.Errorf("log carries the error text: %s", buf.String())
	}
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["err"] != "unavailable (*net.OpError)" {
		t.Errorf("err = %v", rec["err"])
	}
}

func TestInstrumentRecoversFromPanic(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	n := 0
	cs := sessionWith(t, log, func(context.Context, noArgs) (listSourcesOut, *source.Result, error) {
		n++
		if n == 1 {
			var m map[string]int
			m["x"] = 1 // panics: assignment to a nil map
		}
		return listSourcesOut{Sources: []string{}}, nil, nil
	})
	res := call(t, cs, "probe", nil)
	if !res.IsError || text(res) != "internal error" {
		t.Fatalf("IsError=%v text=%q, want %q", res.IsError, text(res), "internal error")
	}
	if !strings.Contains(buf.String(), `"msg":"tool panic"`) || !strings.Contains(buf.String(), `"tool":"probe"`) {
		t.Errorf("panic not logged with the tool name: %s", buf.String())
	}
	if strings.Contains(buf.String(), "nil map") || strings.Contains(buf.String(), "assignment") {
		t.Errorf("panic value leaked into the log: %s", buf.String())
	}
	// The process (and the session) is still alive.
	if res := call(t, cs, "probe", nil); res.IsError {
		t.Fatalf("second call failed: %s", text(res))
	}
}

// fakeDB counts how often a statement reached the database layer.
type fakeDB struct{ calls []string }

func (f *fakeDB) exec(_ context.Context, _ *source.Source, query string, _ ...any) (*source.Result, error) {
	f.calls = append(f.calls, query)
	return &source.Result{Columns: []string{"x"}, Rows: []map[string]any{{"x": 1}}, RowCount: 1}, nil
}

func fakeSession(t *testing.T) (*mcp.ClientSession, *fakeDB) {
	t.Helper()
	f := &fakeDB{}
	d := Deps{Reg: source.NewRegistry([]*source.Source{{Name: "a"}}), exec: f.exec}
	return newSession(t, d), f
}

func TestRejectCasesNeverReachTheDatabase(t *testing.T) {
	cs, f := fakeSession(t)
	for _, tool := range []string{"execute_sql", "get_query_plan"} {
		for _, c := range guard.RejectCases {
			if !utf8.ValidString(c.SQL) {
				// JSON cannot carry invalid UTF-8: the client would send U+FFFD,
				// a different (valid) statement. The guard's own tests cover it.
				continue
			}
			res := call(t, cs, tool, map[string]any{"sql": c.SQL})
			if !res.IsError {
				t.Errorf("%s %q: not an error result", tool, c.SQL)
				continue
			}
			if !strings.Contains(text(res), "statement not allowed") {
				t.Errorf("%s %q: text %q is not the guard's rejection", tool, c.SQL, text(res))
			}
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("rejected SQL reached the database layer: %q", f.calls)
	}
}

func TestAllowedSQLReachesTheDatabaseLayer(t *testing.T) {
	cs, f := fakeSession(t)
	for _, q := range guard.AllowCases {
		if res := call(t, cs, "execute_sql", map[string]any{"sql": q}); res.IsError {
			t.Errorf("%q: %s", q, text(res))
		}
	}
	if len(f.calls) != len(guard.AllowCases) {
		t.Fatalf("hook saw %d statements, want %d", len(f.calls), len(guard.AllowCases))
	}
}

func TestGetQueryPlanWrapsInExplainJSON(t *testing.T) {
	f := &fakeDB{}
	d := Deps{Reg: source.NewRegistry([]*source.Source{{Name: "a"}}), exec: func(ctx context.Context, s *source.Source, q string, a ...any) (*source.Result, error) {
		f.calls = append(f.calls, q)
		return &source.Result{Columns: []string{"EXPLAIN"}, Rows: []map[string]any{{"EXPLAIN": `{"query_block":{"select_id":1}}`}}, RowCount: 1}, nil
	}}
	res := call(t, newSession(t, d), "get_query_plan", map[string]any{"sql": "SELECT 1"})
	if res.IsError {
		t.Fatal(text(res))
	}
	if len(f.calls) != 1 || f.calls[0] != "EXPLAIN FORMAT=JSON SELECT 1" {
		t.Fatalf("statements = %q", f.calls)
	}
}
