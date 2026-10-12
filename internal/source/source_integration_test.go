//go:build integration

package source

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/minhquan2904/mysql-mcp-go/internal/config"
)

var testLimits = Limits{
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

// openSource opens a Source whose DSN went through the production allowlist
// (config.NormalizeDSN), exactly as the server does at startup.
func openSource(t *testing.T, key string, lim Limits) *Source {
	t.Helper()
	dsn, _, err := config.NormalizeDSN(envDSN(t, key), lim.Timeout)
	if err != nil {
		t.Fatalf("NormalizeDSN(%s): %v", key, err)
	}
	s, err := Open(context.Background(), config.Source{Name: "it", DSN: dsn}, lim, nil)
	if err != nil {
		t.Fatalf("Open(%s): %v", key, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func ro(t *testing.T, lim Limits) *Source   { t.Helper(); return openSource(t, "IT_DSN_RO", lim) }
func root(t *testing.T, lim Limits) *Source { t.Helper(); return openSource(t, "IT_DSN_ROOT", lim) }

// rootDB is a plain root connection (no read-only session) for out-of-band
// assertions and cleanup. Its pool is closed at the end of the test.
func rootDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", envDSN(t, "IT_DSN_ROOT"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { db.Close() })
	return db
}

func mysqlErrNumber(err error) uint16 {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return me.Number
	}
	return 0
}

func run(t *testing.T, s *Source, q string, args ...any) *Result {
	t.Helper()
	res, err := s.RunReadOnly(context.Background(), q, args...)
	if err != nil {
		t.Fatalf("RunReadOnly(%q): %v", q, err)
	}
	return res
}

// findConn polls the processlist (as root) for the connection running a
// statement that contains marker, and returns its id.
func findConn(t *testing.T, db *sql.DB, marker string) int64 {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var id int64
		err := db.QueryRow(`SELECT ID FROM performance_schema.processlist
			WHERE INFO LIKE CONCAT('%', ?, '%') AND INFO NOT LIKE '%processlist%' LIMIT 1`, marker).Scan(&id)
		if err == nil {
			return id
		}
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("processlist: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no connection running %q appeared in the processlist", marker)
	return 0
}

// waitGone fails unless connection id has left the processlist within d.
func waitGone(t *testing.T, db *sql.DB, id int64, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM performance_schema.processlist WHERE ID = ?`, id).Scan(&n); err != nil {
			t.Fatalf("processlist: %v", err)
		}
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("connection %d still in the processlist after %v", id, d)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func marker(t *testing.T) string {
	return fmt.Sprintf("it-%s-%d", strings.ReplaceAll(t.Name(), "/", "_"), time.Now().UnixNano())
}

// 1
func TestRunReadOnlyBasicSelect(t *testing.T) {
	res := run(t, ro(t, testLimits), "SELECT id, email FROM customers ORDER BY id")
	if want := []string{"id", "email"}; !reflect.DeepEqual(res.Columns, want) {
		t.Errorf("Columns = %v, want %v", res.Columns, want)
	}
	if res.RowCount != 20 || len(res.Rows) != 20 || res.Truncated {
		t.Fatalf("RowCount=%d rows=%d truncated=%v, want 20/20/false", res.RowCount, len(res.Rows), res.Truncated)
	}
	if res.Rows[0]["email"] != "user1@example.com" {
		t.Errorf("first email = %v", res.Rows[0]["email"])
	}
}

// 2
func TestRunReadOnlyMaxRows(t *testing.T) {
	lim := testLimits
	lim.MaxRows = 5
	res := run(t, ro(t, lim), "SELECT id FROM customers ORDER BY id")
	if len(res.Rows) != 5 || res.RowCount != 5 || !res.Truncated {
		t.Errorf("rows=%d RowCount=%d truncated=%v, want 5/5/true", len(res.Rows), res.RowCount, res.Truncated)
	}
}

// 3
func TestRunReadOnlyBlobAndJSON(t *testing.T) {
	s := ro(t, testLimits)

	res := run(t, s, "SELECT img, note FROM products_blob ORDER BY id")
	if len(res.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(res.Rows))
	}
	if got, want := res.Rows[0]["img"], map[string]string{"base64": "/wD+"}; !reflect.DeepEqual(got, want) {
		t.Errorf("img[0] = %#v, want %#v", got, want)
	}
	if res.Rows[0]["note"] != "binary, not valid UTF-8" {
		t.Errorf("note[0] = %#v", res.Rows[0]["note"])
	}
	if res.Rows[1]["img"] != "plain text blob" {
		t.Errorf("img[1] = %#v, want valid UTF-8 blob as string", res.Rows[1]["img"])
	}
	if v, ok := res.Rows[2]["note"]; !ok || v != nil {
		t.Errorf("note[2] = %#v, want nil", v)
	}

	res = run(t, s, "SELECT payload FROM audit_log WHERE event = 'seed_event_1'")
	want := map[string]any{"n": json.Number("1"), "tags": []any{"a", "b"}, "nested": map[string]any{"ok": true}}
	if len(res.Rows) != 1 || !reflect.DeepEqual(res.Rows[0]["payload"], want) {
		t.Errorf("payload = %#v, want %#v", res.Rows, want)
	}
}

// Column types end to end: DATETIME as RFC3339 UTC, duplicate names.
func TestRunReadOnlyTimeAndDuplicateColumns(t *testing.T) {
	res := run(t, ro(t, testLimits), "SELECT created_at, created_at FROM customers WHERE id = 1")
	if want := []string{"created_at", "created_at_2"}; !reflect.DeepEqual(res.Columns, want) {
		t.Errorf("Columns = %v, want %v", res.Columns, want)
	}
	if res.Rows[0]["created_at"] != "2026-01-02T00:00:00Z" || res.Rows[0]["created_at_2"] != "2026-01-02T00:00:00Z" {
		t.Errorf("row = %#v", res.Rows[0])
	}
}

// 4
func TestRootWriteBlockedByReadOnlyTransaction(t *testing.T) {
	_, err := root(t, testLimits).RunReadOnly(context.Background(), "UPDATE it_scratch SET v = v WHERE id = 1")
	if n := mysqlErrNumber(err); n != 1792 {
		t.Fatalf("err = %v (number %d), want MySQL error 1792", err, n)
	}
}

// 5: session state must not survive into the next call. (Finding, MySQL 8.4:
// CREATE TEMPORARY TABLE is rejected inside a READ ONLY transaction with 1792,
// unlike the plan's assumption, so a user variable is the probe instead.)
func TestConnectionIsDiscardedAfterEveryQuery(t *testing.T) {
	s := root(t, testLimits)

	_, err := s.RunReadOnly(context.Background(), "CREATE TEMPORARY TABLE tmp1 (i INT)")
	if n := mysqlErrNumber(err); n != 1792 {
		t.Errorf("CREATE TEMPORARY TABLE in READ ONLY tx: err = %v (number %d), want 1792", err, n)
	}

	res := run(t, s, "SELECT @it_probe := 42 AS set_it")
	if res.Rows[0]["set_it"] != int64(42) {
		t.Fatalf("user variable not set: %#v", res.Rows[0])
	}
	res = run(t, s, "SELECT @it_probe AS v")
	if v := res.Rows[0]["v"]; v != nil {
		t.Errorf("@it_probe = %#v in the next call, want nil (connection must not be reused)", v)
	}
}

// 6: DDL and account statements run by a root session that has
// transaction_read_only=1 and an explicit READ ONLY transaction. Observed on
// MySQL 8.4: none takes effect, each fails with 1792 before doing anything (no
// implicit commit happens). The privilege self-check stays the backstop. The
// outcome of each statement is logged and the DB is restored in every case.
func TestRootDDLDocumentedBehavior(t *testing.T) {
	s := root(t, testLimits)
	adm := rootDB(t)

	cleanup := func() {
		for _, q := range []string{
			"DROP TABLE IF EXISTS testdb.ddl_probe",
			"DROP VIEW IF EXISTS testdb.v_ddl_probe",
			"DROP USER IF EXISTS 'ddl_probe_u'@'%'",
		} {
			if _, err := adm.Exec(q); err != nil {
				t.Errorf("cleanup %q: %v", q, err)
			}
		}
		// 1141/1147: the grant was never made.
		if _, err := adm.Exec("REVOKE SELECT ON testdb.it_scratch FROM 'mcp_ro'@'%'"); err != nil && mysqlErrNumber(err) != 1141 && mysqlErrNumber(err) != 1147 {
			t.Errorf("cleanup REVOKE: %v", err)
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	// A throwaway view stands in for v_order_totals so a surprising result can
	// never damage the seed.
	if _, err := adm.Exec("CREATE VIEW testdb.v_ddl_probe AS SELECT 1 AS x"); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		stmt   string
		runErr error
		effect func() bool // true when the statement took effect
	}
	exists := func(q string) func() bool {
		return func() bool {
			var n int
			if err := adm.QueryRow(q).Scan(&n); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			return n > 0
		}
	}
	outcomes := []*outcome{
		{stmt: "CREATE TABLE testdb.ddl_probe (i INT)",
			effect: exists("SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='testdb' AND TABLE_NAME='ddl_probe'")},
		{stmt: "DROP VIEW testdb.v_ddl_probe",
			effect: func() bool {
				return !exists("SELECT COUNT(*) FROM information_schema.VIEWS WHERE TABLE_SCHEMA='testdb' AND TABLE_NAME='v_ddl_probe'")()
			}},
		{stmt: "GRANT SELECT ON testdb.* TO 'mcp_ro'@'%'", // no-op grant, as in the plan; effect not observable
			effect: func() bool { return false }},
		{stmt: "GRANT SELECT ON testdb.it_scratch TO 'mcp_ro'@'%'",
			effect: exists("SELECT COUNT(*) FROM information_schema.TABLE_PRIVILEGES WHERE GRANTEE=\"'mcp_ro'@'%'\" AND TABLE_SCHEMA='testdb' AND TABLE_NAME='it_scratch'")},
		{stmt: "CREATE USER 'ddl_probe_u'@'%'",
			effect: exists("SELECT COUNT(*) FROM mysql.user WHERE User='ddl_probe_u'")},
	}
	for _, o := range outcomes {
		_, o.runErr = s.RunReadOnly(context.Background(), o.stmt)
		took := o.effect()
		t.Logf("FINDING root via RunReadOnly: %-60s err=%v took_effect=%v", o.stmt, o.runErr, took)
		if took {
			t.Errorf("%q took effect through a READ ONLY transaction; the privilege self-check is the only backstop", o.stmt)
		}
		// Finding (MySQL 8.4.x): every one is refused with 1792 up front.
		if n := mysqlErrNumber(o.runErr); n != 1792 {
			t.Errorf("%q: err = %v (number %d), want 1792", o.stmt, o.runErr, n)
		}
	}
}

// 7
func TestTimeoutKillsQuery(t *testing.T) {
	lim := testLimits
	lim.Timeout = 2 * time.Second
	s := ro(t, lim)
	adm := rootDB(t)
	m := marker(t)

	type out struct {
		err error
		d   time.Duration
	}
	done := make(chan out, 1)
	go func() {
		start := time.Now()
		_, err := s.RunReadOnly(context.Background(), "SELECT SLEEP(30) /* "+m+" */")
		done <- out{err, time.Since(start)}
	}()
	id := findConn(t, adm, m)

	o := <-done
	if o.err == nil || !errors.Is(o.err, ErrQueryTimeout) || !strings.Contains(o.err.Error(), "exceeded timeout") {
		t.Fatalf("err = %v, want ErrQueryTimeout (\"query exceeded timeout\")", o.err)
	}
	if o.d > 4*time.Second {
		t.Errorf("returned after %v, want <= 4s", o.d)
	}
	kills := s.kills.Load()
	serverTimeout := mysqlErrNumber(o.err) == 3024
	if kills != 1 && !serverTimeout {
		t.Errorf("kills = %d and no server max_execution_time error: err = %v", kills, o.err)
	}
	t.Logf("elapsed %v, kills=%d, server max_execution_time error=%v", o.d, kills, serverTimeout)
	waitGone(t, adm, id, 3*time.Second)
}

// 8
func TestCancelKillsQuery(t *testing.T) {
	s := ro(t, testLimits)
	adm := rootDB(t)
	m := marker(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.RunReadOnly(ctx, "SELECT SLEEP(30) /* "+m+" */")
		done <- err
	}()
	id := findConn(t, adm, m)
	time.Sleep(time.Second)
	start := time.Now()
	cancel()

	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > 4*time.Second {
		t.Errorf("returned %v after cancel, want <= 4s", d)
	}
	if got := s.kills.Load(); got != 1 {
		t.Errorf("kills = %d, want 1", got)
	}
	waitGone(t, adm, id, 3*time.Second)
}

// 9
func TestRunReadOnlyWithArgs(t *testing.T) {
	res := run(t, ro(t, testLimits), "SELECT * FROM customers WHERE id = ?", 3)
	if len(res.Rows) != 1 || res.Rows[0]["email"] != "user3@example.com" {
		t.Errorf("rows = %#v", res.Rows)
	}
}

// 10
func TestSessionLockDoesNotSurviveTheQuery(t *testing.T) {
	s := ro(t, testLimits)
	adm := rootDB(t)
	name := fmt.Sprintf("it_lock_%d", time.Now().UnixNano())

	res := run(t, s, "SELECT GET_LOCK(?, 0) AS got", name) // bypasses the guard on purpose
	if res.Rows[0]["got"] != int64(1) {
		t.Fatalf("GET_LOCK = %#v, want 1", res.Rows[0]["got"])
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var holder sql.NullInt64
		if err := adm.QueryRow("SELECT IS_USED_LOCK(?)", name).Scan(&holder); err != nil {
			t.Fatal(err)
		}
		if !holder.Valid {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock %q still held by connection %d: the physical connection was not closed", name, holder.Int64)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// 11
func TestMinimalGrantsDenySensitiveReads(t *testing.T) {
	s := ro(t, testLimits)
	_, err := s.RunReadOnly(context.Background(), "SELECT * FROM mysql.user")
	if n := mysqlErrNumber(err); n != 1142 {
		t.Errorf("mysql.user: err = %v (number %d), want 1142", err, n)
	}
	if _, err := s.RunReadOnly(context.Background(), "SHOW CREATE USER root"); err == nil {
		t.Error("SHOW CREATE USER root succeeded for the read-only user")
	}
	_, err = s.RunReadOnly(context.Background(), "SELECT * FROM performance_schema.user_variables_by_thread")
	if n := mysqlErrNumber(err); n != 1142 {
		t.Errorf("user_variables_by_thread: err = %v (number %d), want 1142", err, n)
	}
}

// 12
func TestSaturationAllReturnWithinTimeout(t *testing.T) {
	lim := testLimits
	lim.Timeout = 2 * time.Second
	lim.MaxConcurrent = 4
	s := ro(t, lim)

	// more callers than the pool holds (MaxConcurrent+2): the extra ones wait
	// for a connection and must still end with the timeout
	n := lim.MaxConcurrent + 4
	start := time.Now()
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			_, errs[i] = s.RunReadOnly(context.Background(), "SELECT SLEEP(30)")
		})
	}
	wg.Wait()
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("all %d queries took %v, want <= 5s", n, d)
	}
	for i, err := range errs {
		if !errors.Is(err, ErrQueryTimeout) {
			t.Errorf("query %d: err = %v, want ErrQueryTimeout", i, err)
		}
	}
	t.Logf("%d parallel SLEEP(30): %v, kills=%d", n, time.Since(start), s.kills.Load())
}

// M1: stopping at MaxRows must not drain millions of remaining rows.
func TestEarlyStopDoesNotDrainTheResult(t *testing.T) {
	lim := testLimits
	lim.MaxRows = 5
	lim.Timeout = 10 * time.Second
	s := ro(t, lim)
	adm := rootDB(t)
	m := marker(t)

	// 125^3 * 20 * 50 = about 2 billion rows
	q := "SELECT a.order_id FROM order_items a, order_items b, order_items c, customers d, orders e /* " + m + " */"
	start := time.Now()
	res, err := s.RunReadOnly(context.Background(), q)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("err = %v, want a truncated success", err)
	}
	if len(res.Rows) != 5 || !res.Truncated {
		t.Errorf("rows=%d truncated=%v, want 5/true", len(res.Rows), res.Truncated)
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %v, want < 2s (the rest of the result must be killed, not drained)", elapsed)
	}
	if got := s.kills.Load(); got != 1 {
		t.Errorf("kills = %d, want 1", got)
	}
	// the server side is gone too
	deadline := time.Now().Add(3 * time.Second)
	for {
		var n int
		if err := adm.QueryRow(`SELECT COUNT(*) FROM performance_schema.processlist
			WHERE INFO LIKE CONCAT('%', ?, '%') AND INFO NOT LIKE '%processlist%'`, m).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the early-stopped query is still running on the server")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Same for the byte cap.
func TestEarlyStopOnResultBytesDoesNotDrain(t *testing.T) {
	lim := testLimits
	lim.MaxCellBytes = 100
	lim.MaxResultBytes = 200
	lim.Timeout = 10 * time.Second
	s := ro(t, lim)
	start := time.Now()
	res, err := s.RunReadOnly(context.Background(),
		"SELECT a.order_id FROM order_items a, order_items b, order_items c, customers d, orders e")
	if err != nil {
		t.Fatalf("err = %v, want a truncated success", err)
	}
	if !res.Truncated || len(res.Rows) == 0 || time.Since(start) > 2*time.Second {
		t.Errorf("rows=%d truncated=%v elapsed=%v", len(res.Rows), res.Truncated, time.Since(start))
	}
}

// Both killer connections are open before the first timeout needs them.
func TestOpenPrewarmsKillerPool(t *testing.T) {
	s := ro(t, testLimits)
	if got := s.killer.Stats().OpenConnections; got != 2 {
		t.Errorf("killer open connections = %d, want 2", got)
	}
}

// 13
func TestCheckPrivileges(t *testing.T) {
	if err := ro(t, testLimits).CheckPrivileges(context.Background()); err != nil {
		t.Errorf("read-only user: %v, want nil", err)
	}
	err := root(t, testLimits).CheckPrivileges(context.Background())
	if err == nil {
		t.Fatal("root passed the privilege self-check")
	}
	t.Logf("root self-check: %v", err)
}

func TestRegistryHealthAgainstRealSource(t *testing.T) {
	s := ro(t, testLimits)
	reg := NewRegistry([]*Source{s})
	got := reg.Health(context.Background())
	if len(got) != 1 || got["it"] != nil {
		t.Errorf("Health = %v, want {it: nil}", got)
	}
}

func TestOpenErrorDoesNotLeakPassword(t *testing.T) {
	cfg, err := mysql.ParseDSN(envDSN(t, "IT_DSN_RO"))
	if err != nil {
		t.Fatal(err)
	}
	const wrong = "wrong-password-canary"
	cfg.Passwd = wrong
	dsn, _, err := config.NormalizeDSN(cfg.FormatDSN(), testLimits.Timeout)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Open(context.Background(), config.Source{Name: "it", DSN: dsn}, testLimits, nil)
	if err == nil {
		t.Fatal("Open succeeded with a wrong password")
	}
	if strings.Contains(err.Error(), wrong) {
		t.Errorf("error leaks the password: %v", err)
	}
	if mysqlErrNumber(err) != 1045 {
		t.Errorf("err = %v, want access denied (1045)", err)
	}
}
