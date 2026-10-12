//go:build integration

package tools

import (
	"context"
	"database/sql"
	"maps"
	"reflect"
	"strings"
	"testing"
)

// Statements that must never take effect even if the guard were bypassed.
// They are run straight through source.RunReadOnly. Errors are expected and
// allowed; only the invariants after the whole corpus matter.
var bypassCorpus = []string{
	"UPDATE it_scratch SET v = v + 1",
	"DELETE FROM audit_log",
	"INSERT INTO it_scratch VALUES (99, 1)",
	"SELECT GET_LOCK('bypass', 0)",
	"SELECT @x := 1",
	"SELECT * FROM v_definer",
	"EXPLAIN ANALYZE UPDATE it_scratch SET v = v + 1",
	"CREATE TABLE bypass_t (i int)",
}

// state is what the corpus must leave untouched.
type state struct {
	Checksums map[string]int64  // CHECKSUM TABLE of every base table
	Objects   map[string]string // table/view name -> type
	AuditRows int
	LockUsed  sql.NullInt64 // IS_USED_LOCK('bypass')
}

func snapshot(t *testing.T, db *sql.DB) state {
	t.Helper()
	ctx := context.Background()
	st := state{Checksums: map[string]int64{}, Objects: map[string]string{}}

	rows, err := db.QueryContext(ctx, `SELECT TABLE_NAME, TABLE_TYPE FROM information_schema.TABLES WHERE TABLE_SCHEMA = 'testdb'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			t.Fatal(err)
		}
		st.Objects[name] = typ
		if typ == "BASE TABLE" {
			tables = append(tables, "testdb.`"+name+"`")
		}
	}
	rows.Close()

	rows, err = db.QueryContext(ctx, "CHECKSUM TABLE "+strings.Join(tables, ", "))
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		var sum sql.NullInt64
		if err := rows.Scan(&name, &sum); err != nil {
			t.Fatal(err)
		}
		st.Checksums[name] = sum.Int64
	}
	rows.Close()

	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM testdb.audit_log`).Scan(&st.AuditRows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT IS_USED_LOCK('bypass')`).Scan(&st.LockUsed); err != nil {
		t.Fatal(err)
	}
	return st
}

// cleanup undoes anything a surprise could have done, so one failing run does
// not poison the next.
func cleanup(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, q := range []string{
		"DROP TABLE IF EXISTS testdb.bypass_t",
		"DELETE FROM testdb.it_scratch WHERE id <> 1",
		"UPDATE testdb.it_scratch SET v = 1 WHERE id = 1",
		"DELETE FROM testdb.audit_log WHERE event = 'definer'",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Logf("cleanup %q: %v", q, err)
		}
	}
}

func auditCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM testdb.audit_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestBypassCorpus(t *testing.T) {
	for _, tc := range []struct{ name, key string }{
		{"ro", "IT_DSN_RO"},
		{"root", "IT_DSN_ROOT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := rootDB(t)
			cleanup(t, db)
			t.Cleanup(func() { cleanup(t, db) })
			src := openIT(t, tc.key, tc.name)

			before := snapshot(t, db)
			if before.LockUsed.Valid {
				t.Fatalf("precondition: lock 'bypass' already held by connection %d", before.LockUsed.Int64)
			}

			for _, q := range bypassCorpus {
				res, err := src.RunReadOnly(context.Background(), q)
				switch {
				case err != nil:
					t.Logf("%-5s %-55s -> error: %v", tc.name, q, err)
				default:
					t.Logf("%-5s %-55s -> ok (%d rows)", tc.name, q, res.RowCount)
				}
			}

			after := snapshot(t, db)
			if !reflect.DeepEqual(before.Checksums, after.Checksums) {
				for k := range maps.Keys(before.Checksums) {
					if before.Checksums[k] != after.Checksums[k] {
						t.Errorf("CHECKSUM TABLE %s changed: %d -> %d", k, before.Checksums[k], after.Checksums[k])
					}
				}
			}
			if before.AuditRows != after.AuditRows {
				t.Errorf("audit_log rows %d -> %d", before.AuditRows, after.AuditRows)
			}
			if !reflect.DeepEqual(before.Objects, after.Objects) {
				t.Errorf("tables/views changed:\nbefore %v\nafter  %v", before.Objects, after.Objects)
			}
			if after.LockUsed.Valid {
				t.Errorf("IS_USED_LOCK('bypass') = %d, want NULL (single-use connection must release it)", after.LockUsed.Int64)
			}
		})
	}
}

func TestDefinerViewThroughTool(t *testing.T) {
	db := rootDB(t)
	cleanup(t, db)
	t.Cleanup(func() { cleanup(t, db) })
	before := auditCount(t, db)

	cs := newSession(t, roDeps(t, nil))
	res := call(t, cs, "execute_sql", map[string]any{"sql": "SELECT * FROM v_definer"})
	if !res.IsError {
		t.Errorf("SELECT * FROM v_definer succeeded: %s", text(res))
	}
	t.Logf("v_definer via tool: %s", text(res))
	if after := auditCount(t, db); after != before {
		t.Errorf("audit_log rows %d -> %d: the DEFINER function wrote", before, after)
	}
}

// The invariants must be able to fail: apply real changes as root, out of
// band, and check that the snapshot notices each kind.
func TestSnapshotDetectsChanges(t *testing.T) {
	db := rootDB(t)
	cleanup(t, db)
	t.Cleanup(func() { cleanup(t, db) })
	base := snapshot(t, db)

	for _, tc := range []struct {
		name, stmt string
		differs    func(a, b state) bool
	}{
		{"checksum", "INSERT INTO testdb.it_scratch VALUES (98, 1)",
			func(a, b state) bool { return !reflect.DeepEqual(a.Checksums, b.Checksums) }},
		{"audit rows", "INSERT INTO testdb.audit_log (event, at) VALUES ('definer', NOW())",
			func(a, b state) bool { return a.AuditRows != b.AuditRows }},
		{"objects", "CREATE TABLE testdb.bypass_t (i int)",
			func(a, b state) bool { return !reflect.DeepEqual(a.Objects, b.Objects) }},
	} {
		if _, err := db.Exec(tc.stmt); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !tc.differs(base, snapshot(t, db)) {
			t.Errorf("snapshot did not notice a change of kind %q", tc.name)
		}
		cleanup(t, db)
	}
	if !reflect.DeepEqual(base, snapshot(t, db)) {
		t.Error("cleanup did not restore the seed state")
	}
}
