//go:build integration

package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/minhquan2904/mysql-mcp-go/internal/source"
)

func okStructured(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res.IsError {
		t.Fatalf("IsError: %s", text(res))
	}
	sc, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent = %T", res.StructuredContent)
	}
	return sc
}

func rowsFrom(t *testing.T, res *mcp.CallToolResult) []map[string]any {
	t.Helper()
	sc := okStructured(t, res)
	raw, ok := sc["rows"].([]any)
	if !ok {
		t.Fatalf("rows is %T", sc["rows"])
	}
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		out[i] = r.(map[string]any)
	}
	return out
}

func column(rows []map[string]any, col string) []string {
	var out []string
	for _, r := range rows {
		out = append(out, fmt.Sprint(r[col]))
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestListTablesIT(t *testing.T) {
	cs := newSession(t, roDeps(t, nil))

	rows := rowsFrom(t, call(t, cs, "list_tables", map[string]any{"output_format": "simple"}))
	names := column(rows, "object_name")
	for _, want := range []string{"customers", "orders", "order_items", "audit_log", "products_blob", "it_scratch"} {
		if !contains(names, want) {
			t.Errorf("simple list lacks %s: %v", want, names)
		}
	}
	for _, view := range []string{"v_order_totals", "v_definer"} {
		if contains(names, view) {
			t.Errorf("simple list contains the view %s", view)
		}
	}
	if d, ok := rows[0]["object_details"].(map[string]any); !ok || d["name"] == nil {
		t.Errorf("simple object_details = %#v, want {name: ...}", rows[0]["object_details"])
	}

	rows = rowsFrom(t, call(t, cs, "list_tables", map[string]any{"table_names": "orders"}))
	if len(rows) != 1 {
		t.Fatalf("got %d rows for table_names=orders", len(rows))
	}
	d, ok := rows[0]["object_details"].(map[string]any)
	if !ok {
		t.Fatalf("object_details is %T, want an object", rows[0]["object_details"])
	}
	list := func(key string) []map[string]any {
		var out []map[string]any
		arr, _ := d[key].([]any)
		for _, v := range arr {
			out = append(out, v.(map[string]any))
		}
		return out
	}
	if cols := list("columns"); len(cols) < 5 {
		t.Errorf("columns = %d, want at least 5", len(cols))
	}
	var fk bool
	for _, c := range list("constraints") {
		cc, _ := c["constraint_columns"].([]any)
		if c["constraint_type"] == "FOREIGN KEY" && len(cc) == 1 && cc[0] == "customer_id" && c["foreign_key_referenced_table"] == "customers" {
			fk = true
		}
	}
	if !fk {
		t.Errorf("no FOREIGN KEY customer_id -> customers in %v", d["constraints"])
	}
	if !contains(column(list("indexes"), "index_name"), "idx_status_created") {
		t.Errorf("indexes = %v", d["indexes"])
	}
	// information_schema.TRIGGERS only shows triggers to an account holding the
	// TRIGGER privilege, which the read-only account must not have: the list is
	// present but empty. The next test proves the statement finds the trigger.
	if _, ok := d["triggers"].([]any); !ok {
		t.Errorf("triggers = %#v, want an array", d["triggers"])
	}

	if res := call(t, cs, "list_tables", map[string]any{"output_format": "bad"}); !res.IsError {
		t.Error("output_format=bad must be an error")
	}
}

// The statement itself reports triggers; only the least-privilege account
// cannot see them. Root is used here to prove the port is intact.
func TestListTablesTriggersVisibleToPrivilegedAccountIT(t *testing.T) {
	d := Deps{Reg: source.NewRegistry([]*source.Source{openIT(t, "IT_DSN_ROOT", "root")}), MaxRows: itLimits.MaxRows}
	rows := rowsFrom(t, call(t, newSession(t, d), "list_tables", map[string]any{"table_names": "orders"}))
	obj, _ := rows[0]["object_details"].(map[string]any)
	trg, _ := obj["triggers"].([]any)
	if len(trg) != 1 || trg[0].(map[string]any)["trigger_name"] != "trg_orders_bi" {
		t.Errorf("triggers = %v", obj["triggers"])
	}
}

func TestListActiveQueriesRedactsIT(t *testing.T) {
	const secret = "secret-literal-xyz"
	cs := newSession(t, roDeps(t, nil))
	bg := openIT(t, "IT_DSN_RO", "bg")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		bg.RunReadOnly(ctx, "SELECT SLEEP(10), '"+secret+"'")
	}()
	defer func() { cancel(); <-done }()

	var found string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		res := call(t, cs, "list_active_queries", map[string]any{"min_duration_secs": 1})
		out := text(res)
		if strings.Contains(out, secret) {
			t.Fatalf("output contains the literal: %s", out)
		}
		for _, r := range rowsFrom(t, res) {
			if q, _ := r["query"].(string); strings.Contains(q, "SLEEP") {
				found = q
			}
		}
		if found != "" {
			break
		}
	}
	if !strings.Contains(found, "SLEEP(?)") {
		t.Fatalf("no redacted SLEEP query seen, last = %q", found)
	}
}

func TestListAllLocksIT(t *testing.T) {
	cs := newSession(t, roDeps(t, nil))
	res := call(t, cs, "list_all_locks", nil)
	okStructured(t, res)
	if _, err := json.Marshal(res.StructuredContent); err != nil {
		t.Fatal(err)
	}

	// Hold a row lock as root, then look at it as the read-only account.
	db := rootDB(t)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("SELECT * FROM it_scratch WHERE id = 1 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	// information_schema.innodb_trx is served from a cache InnoDB refreshes at
	// most every 100ms, so give the new lock a moment to show up.
	var rows []map[string]any
	for deadline := time.Now().Add(3 * time.Second); len(rows) == 0 && time.Now().Before(deadline); time.Sleep(150 * time.Millisecond) {
		rows = rowsFrom(t, call(t, cs, "list_all_locks", map[string]any{"table_schema": "testdb", "table_name": "it_scratch"}))
	}
	if len(rows) == 0 {
		t.Fatal("the lock held on it_scratch is not listed")
	}
	for _, c := range []string{"thread_id", "process_id", "table_schema", "table_name", "lock_type", "lock_mode", "lock_status", "transaction_state", "current_operation", "query"} {
		if _, ok := rows[0][c]; !ok {
			t.Errorf("column %s missing: %v", c, rows[0])
		}
	}
}

func TestListTableFragmentationIT(t *testing.T) {
	cs := newSession(t, roDeps(t, nil))
	res := call(t, cs, "list_table_fragmentation", map[string]any{"table_schema": "testdb", "data_free_threshold_bytes": 0})
	sc := okStructured(t, res)
	var cols []string
	for _, c := range sc["columns"].([]any) {
		cols = append(cols, c.(string))
	}
	want := []string{"table_schema", "table_name", "data_size", "index_size", "data_free", "fragmentation_percentage"}
	if !reflect.DeepEqual(cols, want) {
		t.Errorf("columns = %v, want %v", cols, want)
	}
	if rows := rowsFrom(t, res); !contains(column(rows, "table_name"), "orders") {
		t.Errorf("tables = %v", column(rows, "table_name"))
	}
}

func TestListTablesMissingUniqueIndexesIT(t *testing.T) {
	cs := newSession(t, roDeps(t, nil))
	names := column(rowsFrom(t, call(t, cs, "list_tables_missing_unique_indexes", map[string]any{"table_schema": "testdb"})), "table_name")
	if !contains(names, "audit_log") || contains(names, "customers") {
		t.Errorf("tables = %v, want audit_log and not customers", names)
	}
}

func TestListTableStatsIT(t *testing.T) {
	cs := newSession(t, roDeps(t, nil))
	call(t, cs, "execute_sql", map[string]any{"sql": "SELECT * FROM orders"})

	res := call(t, cs, "list_table_stats", map[string]any{"table_schema": "testdb"})
	rows := rowsFrom(t, res)
	if !contains(column(rows, "table_name"), "orders") {
		t.Fatalf("tables = %v", column(rows, "table_name"))
	}
	for _, c := range []string{"table_schema", "table_name", "table_rows", "data_length", "index_length", "rows_fetched",
		"rows_inserted", "rows_updated", "rows_deleted", "write_ops"} {
		if _, ok := rows[0][c]; !ok {
			t.Errorf("column %s missing: %v", c, rows[0])
		}
	}
	for _, r := range rows {
		if r["table_name"] == "orders" && r["rows_fetched"] == nil {
			t.Errorf("orders has no io statistics: %v", r)
		}
	}
	for _, s := range []string{"row_count", "rows_fetched", "rows_inserted", "rows_updated", "rows_deleted"} {
		if res := call(t, cs, "list_table_stats", map[string]any{"table_schema": "testdb", "sort_by": s, "limit": 3}); res.IsError {
			t.Errorf("sort_by=%s: %s", s, text(res))
		}
	}
	if res := call(t, cs, "list_table_stats", map[string]any{"sort_by": "bogus"}); !res.IsError {
		t.Error("sort_by=bogus must be an error")
	}
	// A foreign schema is refused because the source's DSN names testdb.
	if res := call(t, cs, "list_table_stats", map[string]any{"table_schema": "mysql"}); !res.IsError {
		t.Error("table_schema=mysql must be an error")
	}
}

func TestShowQueryStatsIT(t *testing.T) {
	cs := newSession(t, roDeps(t, nil))
	for i := 0; i < 3; i++ {
		call(t, cs, "execute_sql", map[string]any{"sql": fmt.Sprintf("SELECT id, email FROM customers WHERE id = %d", i+1)})
	}
	res := call(t, cs, "show_query_stats", nil)
	rows := rowsFrom(t, res)
	if len(rows) == 0 {
		t.Fatal("no digest rows")
	}
	out := strings.ToLower(text(res))
	if strings.Contains(out, "query_sample_text") || strings.Contains(out, "sample") {
		t.Errorf("response mentions sample text: %s", out)
	}
	for _, r := range rows {
		if _, ok := r["query_sample_text"]; ok {
			t.Errorf("row has query_sample_text: %v", r)
		}
	}
	if !contains(column(rows, "db"), "testdb") {
		t.Errorf("databases = %v", column(rows, "db"))
	}
	if res := call(t, cs, "show_query_stats", map[string]any{"table_schema": "mysql"}); !res.IsError {
		t.Error("table_schema=mysql must be an error")
	}
}

// 6.8: the account's grants are exactly what the tools need, and the startup
// self-check still accepts them.
func TestFinalGrantsIT(t *testing.T) {
	db := rootDB(t)
	got := showGrants(t, db, "mcp_ro")
	want := []string{
		"GRANT PROCESS ON *.* TO `mcp_ro`@`%`",
		"GRANT SELECT, SHOW VIEW ON `testdb`.* TO `mcp_ro`@`%`",
		"GRANT SELECT ON `performance_schema`.`data_locks` TO `mcp_ro`@`%`",
		"GRANT SELECT ON `performance_schema`.`events_statements_summary_by_digest` TO `mcp_ro`@`%`",
		"GRANT SELECT ON `performance_schema`.`table_io_waits_summary_by_table` TO `mcp_ro`@`%`",
	}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SHOW GRANTS =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	if err := openIT(t, "IT_DSN_RO", "it").CheckPrivileges(context.Background()); err != nil {
		t.Errorf("CheckPrivileges: %v", err)
	}

	// Sensitive and unrelated tables stay closed.
	s := openIT(t, "IT_DSN_RO", "it")
	for _, q := range []string{
		"SELECT * FROM mysql.user",
		"SELECT * FROM performance_schema.user_variables_by_thread",
		"SELECT * FROM performance_schema.events_statements_history",
		"SELECT * FROM performance_schema.events_statements_current",
		"SELECT * FROM performance_schema.data_lock_waits",
		"SELECT * FROM sys.x$schema_table_statistics",
	} {
		_, err := s.RunReadOnly(context.Background(), q)
		var me *mysql.MySQLError
		if err == nil || !asMySQL(err, &me) || (me.Number != 1142 && me.Number != 1044 && me.Number != 1227) {
			t.Errorf("%s: err = %v, want a privilege error", q, err)
		}
	}
}

func asMySQL(err error, target **mysql.MySQLError) bool {
	for err != nil {
		if me, ok := err.(*mysql.MySQLError); ok {
			*target = me
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func showGrants(t *testing.T, db *sql.DB, user string) []string {
	t.Helper()
	rows, err := db.Query("SHOW GRANTS FOR '" + user + "'@'%'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			t.Fatal(err)
		}
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

// 6.1 risk: list_tables uses session variables; they must work in a
// read-only transaction on a single-use connection.
func TestToolboxToolsAllRunWithReadOnlyAccount(t *testing.T) {
	cs := newSession(t, roDeps(t, nil))
	for _, name := range []string{"list_tables", "list_active_queries", "list_all_locks", "list_table_fragmentation",
		"list_tables_missing_unique_indexes", "list_table_stats", "show_query_stats"} {
		res := call(t, cs, name, nil)
		if res.IsError {
			t.Errorf("%s: %s", name, text(res))
			continue
		}
		sc := okStructured(t, res)
		if rows, ok := sc["rows"].([]any); !ok || rows == nil {
			t.Errorf("%s: rows = %v", name, sc["rows"])
		}
	}
}

// execute_sql must not read the performance_schema tables the other tools are
// granted: their raw columns (QUERY_SAMPLE_TEXT, LOCK_DATA) hold literals from
// every schema.
func TestExecuteSQLCannotReadGrantedPerformanceSchemaTables(t *testing.T) {
	cs := newSession(t, roDeps(t, nil))
	for _, q := range []string{
		"SELECT * FROM performance_schema.events_statements_summary_by_digest",
		"SELECT QUERY_SAMPLE_TEXT FROM performance_schema.events_statements_summary_by_digest",
		"SELECT LOCK_DATA FROM performance_schema.data_locks",
		"SELECT * FROM performance_schema.data_locks",
		"SELECT * FROM `performance_schema`.`table_io_waits_summary_by_table`",
		"SELECT * FROM data_locks",
		"SELECT * FROM events_statements_summary_by_digest",
	} {
		for _, tool := range []string{"execute_sql", "get_query_plan"} {
			res := call(t, cs, tool, map[string]any{"sql": q})
			if !res.IsError || !strings.Contains(text(res), "statement not allowed") {
				t.Errorf("%s %q: IsError=%v text=%q, want the guard's rejection", tool, q, res.IsError, text(res))
			}
		}
	}
}

// A statement waiting for a lock is visible in innodb_trx.trx_query: it must
// come out redacted, and the lock listing must not carry LOCK_DATA values.
func TestListAllLocksRedactsWaitingStatementIT(t *testing.T) {
	const secret = "secret-lock-literal"
	cs := newSession(t, roDeps(t, nil))
	db := rootDB(t)
	ctx := context.Background()

	holder, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	htx, err := holder.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer htx.Rollback()
	if _, err := htx.Exec("SELECT * FROM it_scratch WHERE id = 1 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}

	// The waiter blocks on the same row; its statement text stays in trx_query.
	waiter, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Close()
	wtx, err := waiter.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		wtx.Exec("SELECT id FROM it_scratch WHERE id = 1 AND '" + secret + "' <> '' FOR UPDATE")
	}()
	finish := func() {
		htx.Rollback() // lets the waiter proceed
		<-done
		wtx.Rollback()
	}
	defer finish()

	var waiting map[string]any
	var out string
	for deadline := time.Now().Add(5 * time.Second); waiting == nil && time.Now().Before(deadline); time.Sleep(150 * time.Millisecond) {
		res := call(t, cs, "list_all_locks", map[string]any{"table_schema": "testdb", "table_name": "it_scratch"})
		out = text(res)
		if strings.Contains(out, secret) {
			t.Fatalf("output contains the waiting statement's literal: %s", out)
		}
		for _, r := range rowsFrom(t, res) {
			if r["lock_status"] == "WAITING" {
				waiting = r
			}
		}
	}
	if waiting == nil {
		t.Fatalf("no WAITING lock seen, last output: %s", out)
	}
	q, _ := waiting["query"].(string)
	if !strings.Contains(q, "it_scratch") || !strings.Contains(q, "?") {
		t.Errorf("waiting statement = %q, want it redacted but present", q)
	}
	if strings.Contains(strings.ToLower(out), "lock_data") {
		t.Errorf("output mentions lock_data: %s", out)
	}
}
