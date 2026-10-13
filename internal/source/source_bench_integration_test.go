//go:build integration

package source

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/go-sql-driver/mysql"

	"github.com/minhquan2904/mysql-mcp-go/internal/config"
)

const benchSelect1 = "SELECT 1"

// benchRows is the row count of benchRowsQuery.
const benchRows = 1000

// benchRowsQuery crosses order_items with an eight-row loop table and caps the
// result with LIMIT 1000. deploy/mysql/init/01-schema.sql seeds 125 order_items
// rows (50 orders, 2-5 lines each), so the join would yield 125*8 = 1000 rows;
// the LIMIT keeps that count fixed if the seed grows. The SQL is constant, so
// every iteration reads the same result.
const benchRowsQuery = `SELECT oi.order_id, oi.line_no, oi.sku, oi.qty, oi.price
FROM order_items oi
CROSS JOIN (SELECT 1 AS m UNION ALL SELECT 2 UNION ALL SELECT 3 UNION ALL SELECT 4
            UNION ALL SELECT 5 UNION ALL SELECT 6 UNION ALL SELECT 7 UNION ALL SELECT 8) AS m
LIMIT 1000`

// BenchmarkDirectQuery is the baseline RunReadOnly is measured against: the
// same normalized DSN in a plain pooled *sql.DB, one QueryContext per
// iteration, connections reused.
func BenchmarkDirectQuery(b *testing.B) {
	db := benchPool(b)
	b.Run("select1", func(b *testing.B) { benchPooled(b, db, benchSelect1, 1, 1) })
	b.Run("rows1000", func(b *testing.B) { benchPooled(b, db, benchRowsQuery, 5, benchRows) })
}

// BenchmarkRunReadOnly is the production path: a single-use connection, a
// CONNECTION_ID() round trip, a READ ONLY transaction and a rollback that
// discards the physical connection, so every call pays its own handshake.
func BenchmarkRunReadOnly(b *testing.B) {
	s := benchSource(b)
	b.Run("select1", func(b *testing.B) { benchReadOnly(b, s, benchSelect1, 1) })
	b.Run("rows1000", func(b *testing.B) { benchReadOnly(b, s, benchRowsQuery, benchRows) })
}

// benchDSN is envDSN for benchmarks: envDSN takes *testing.T, which a
// benchmark cannot pass.
func benchDSN(b *testing.B, key string) string {
	b.Helper()
	v := os.Getenv(key)
	if v == "" {
		b.Skipf("%s not set (run through scripts/dev.sh it)", key)
	}
	return v
}

// benchNormalizedDSN returns the read-only integration DSN put through the
// production allowlist, exactly as the server does at startup.
func benchNormalizedDSN(b *testing.B) string {
	b.Helper()
	dsn, _, err := config.NormalizeDSN(benchDSN(b, "IT_DSN_RO"), testLimits.Timeout)
	if err != nil {
		b.Fatalf("NormalizeDSN(IT_DSN_RO): %v", err)
	}
	return dsn
}

// benchPool opens the read-only DSN as the pooled *sql.DB the direct-query
// baseline uses. The pool is sized like Source's query pool and keeps idle
// connections, so the iterations never show a cold-pool dial: what the two
// benchmark families differ by is the per-call connection work.
func benchPool(b *testing.B) *sql.DB {
	b.Helper()
	db, err := sql.Open("mysql", benchNormalizedDSN(b))
	if err != nil {
		b.Fatal(err)
	}
	db.SetMaxOpenConns(testLimits.MaxConcurrent + 2)
	db.SetMaxIdleConns(testLimits.MaxConcurrent + 2)
	b.Cleanup(func() { db.Close() })
	if err := db.PingContext(context.Background()); err != nil {
		b.Fatalf("ping: %v", err)
	}
	return db
}

// benchSource opens a Source exactly as the server does at startup, with the
// limits the integration tests use.
func benchSource(b *testing.B) *Source {
	b.Helper()
	s, err := Open(context.Background(), config.Source{Name: "bench", DSN: benchNormalizedDSN(b)}, testLimits, nil)
	if err != nil {
		b.Fatalf("Open(IT_DSN_RO): %v", err)
	}
	b.Cleanup(func() { s.Close() })
	return s
}

// benchPooled reads every row of query through the pooled db inside the
// measured loop. cols is the query's column count (fixed by the constant SQL
// above), so the scan targets are allocated once, outside the timer.
func benchPooled(b *testing.B, db *sql.DB, query string, cols, wantRows int) {
	b.Helper()
	vals := make([]any, cols)
	ptrs := make([]any, cols)
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		n, err := benchDrain(db, query, ptrs)
		if err != nil {
			b.Fatalf("drain %q: %v", query, err)
		}
		if n != wantRows {
			b.Fatalf("drain %q: %d rows, want %d", query, n, wantRows)
		}
	}
}

// benchDrain reads the whole result of query through the pool into dest and
// reports how many rows it read. Consuming every row is what a caller does and
// what returns the connection to the pool.
func benchDrain(db *sql.DB, query string, dest []any) (int, error) {
	rows, err := db.QueryContext(context.Background(), query)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

// benchReadOnly measures RunReadOnly, which performs no validation of its own:
// it is the connection, transaction and result materialization cost the guard
// sits on top of.
func benchReadOnly(b *testing.B, s *Source, query string, wantRows int) {
	b.Helper()
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		res, err := s.RunReadOnly(ctx, query)
		if err != nil {
			b.Fatalf("RunReadOnly(%q): %v", query, err)
		}
		if res.RowCount != wantRows {
			b.Fatalf("RunReadOnly(%q): %d rows, want %d", query, res.RowCount, wantRows)
		}
	}
}
