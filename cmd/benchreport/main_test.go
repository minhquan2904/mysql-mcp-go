package main

import "testing"

// row is one cell as cmd/bench emits it.
const row = `{"scenario":"S2","concurrency":8,"n":100,"rps":10.5,"p50_ms":1,"p95_ms":2,"p99_ms":3,"max_ms":4,"errors":7}`

// TestLoadRows covers the two input shapes -rows documents: the JSON array
// .bench/bench-results.json holds and JSON values one per line. The array was
// rejected, which failed the report step of scripts/bench.sh.
func TestLoadRows(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want int
	}{
		{"array", "[" + row + "]\n", 1},
		{"lines", row + "\n" + row + "\n", 2},
		{"empty", " \n\t", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := loadRows([]byte(tc.in))
			if err != nil {
				t.Fatalf("loadRows(%s) returned %v", tc.name, err)
			}
			if len(rows) != tc.want {
				t.Fatalf("loadRows(%s) returned %d rows, want %d", tc.name, len(rows), tc.want)
			}
		})
	}
}

// TestLoadRowsKeepsValues proves the array path decodes the fields the report
// renders, not only the row count.
func TestLoadRowsKeepsValues(t *testing.T) {
	rows, err := loadRows([]byte("[" + row + "]"))
	if err != nil {
		t.Fatalf("loadRows returned %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("loadRows returned %d rows, want 1", len(rows))
	}
	got := rows[0]
	if got.Scenario != "S2" || got.Concurrency != 8 || got.N != 100 || got.RPS != 10.5 || got.Errors != 7 {
		t.Fatalf("loadRows decoded %+v", got)
	}
	if got.P50MS != 1 || got.P95MS != 2 || got.P99MS != 3 || got.MaxMS != 4 {
		t.Fatalf("loadRows decoded latencies %+v", got)
	}
}

// TestLoadRowsRejectsIncompleteRow keeps a malformed row out of the published
// report: a row without a scenario would render an unnamed table row.
func TestLoadRowsRejectsIncompleteRow(t *testing.T) {
	if _, err := loadRows([]byte(`[{"concurrency":8,"n":1}]`)); err == nil {
		t.Fatal("loadRows accepted a row without a scenario")
	}
	if _, err := loadRows([]byte(`[{"scenario":"S2","concurrency":0,"n":1}]`)); err == nil {
		t.Fatal("loadRows accepted concurrency 0")
	}
}

// TestOneLineSurfacesFailures keeps the README summary honest: a cell where
// half the calls failed must not render as a plain throughput number.
func TestOneLineSurfacesFailures(t *testing.T) {
	failing := oneLine(loadRow{Scenario: "S2", Concurrency: 8, N: 20441, RPS: 760.6, P50MS: 7.4, P99MS: 33, Errors: 9807})
	if want := "7.4 ms / 33.0 ms / 760.6 rps (48.0% of calls failed)"; failing.value != want {
		t.Fatalf("oneLine value = %q, want %q", failing.value, want)
	}
	clean := oneLine(loadRow{Scenario: "S2", Concurrency: 1, N: 19621, RPS: 654, P50MS: 1.3, P99MS: 3.5})
	if want := "1.3 ms / 3.5 ms / 654.0 rps"; clean.value != want {
		t.Fatalf("oneLine value = %q, want %q", clean.value, want)
	}
}

// TestLoadTableRendersEveryCell keeps the README's load table honest: one row
// per scenario, one column per concurrency, a dash for a cell that was not
// measured, and the failure rate on a cell that has failures.
func TestLoadTableRendersEveryCell(t *testing.T) {
	rows := []loadRow{
		{Scenario: "S2", Concurrency: 8, N: 22821, RPS: 760.6, P50MS: 7.4, Errors: 8706},
		{Scenario: "S1", Concurrency: 1, N: 80721, RPS: 2690.7, P50MS: 0.3},
		{Scenario: "S2", Concurrency: 1, N: 19621, RPS: 654, P50MS: 1.3},
	}
	want := "Load, every cell (RPS / p50 / errors):\n" +
		"\n" +
		"| Scenario | c=1 | c=8 |\n" +
		"|---|---|---|\n" +
		"| S1 list_sources | 2690.7 rps / 0.3 ms / 0 err | — |\n" +
		"| S2 execute_sql SELECT 1 | 654.0 rps / 1.3 ms / 0 err | 760.6 rps / 7.4 ms / 8706 err (38.1%) |\n"
	if got := loadTable(rows); got != want {
		t.Fatalf("loadTable =\n%q\nwant\n%q", got, want)
	}
	if got := loadTable(nil); got != "" {
		t.Fatalf("loadTable(nil) = %q, want empty", got)
	}
}
