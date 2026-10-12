package source

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// fakeRows implements rowScanner over canned rows.
type fakeRows struct {
	rows    [][]any
	pos     int // rows consumed by Next
	scanned int
	err     error // returned by Err()
	scanErr error
}

func (f *fakeRows) Next() bool {
	if f.pos >= len(f.rows) {
		return false
	}
	f.pos++
	return true
}

func (f *fakeRows) Scan(dest ...any) error {
	if f.scanErr != nil {
		return f.scanErr
	}
	f.scanned++
	row := f.rows[f.pos-1]
	for i := range dest {
		*(dest[i].(*any)) = row[i]
	}
	return nil
}

func (f *fakeRows) Err() error { return f.err }

func cols(names ...string) []colInfo {
	out := make([]colInfo, len(names))
	for i, n := range names {
		out[i] = colInfo{Name: n}
	}
	return out
}

var bigLimits = Limits{MaxRows: 1000, MaxCellBytes: 1 << 20, MaxResultBytes: 1 << 20}

func mustCollect(t *testing.T, rows [][]any, c []colInfo, lim Limits) *Result {
	t.Helper()
	res, err := collect(&fakeRows{rows: rows}, c, lim)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	return res
}

func TestCollectValueConversion(t *testing.T) {
	ts := time.Date(2026, 3, 1, 9, 30, 0, 0, time.FixedZone("X", 7*3600))
	ts6 := time.Date(2026, 3, 1, 9, 30, 0, 123456000, time.UTC)
	c := []colInfo{
		{"nul", "VARCHAR"}, {"js", "JSON"}, {"badjs", "JSON"}, {"txt", "TEXT"},
		{"bin", "BLOB"}, {"ts", "DATETIME"}, {"ts6", "DATETIME"}, {"i", "INT"}, {"f", "DOUBLE"}, {"u", "BIGINT"},
	}
	res := mustCollect(t, [][]any{{
		nil, []byte(`{"a":1}`), []byte(`not json`), []byte("héllo"),
		[]byte{0xFF, 0x00, 0xFE}, ts, ts6, int64(7), 1.5, uint64(1 << 63),
	}}, c, bigLimits)

	want := map[string]any{
		"nul":   nil,
		"js":    map[string]any{"a": json.Number("1")},
		"badjs": "not json",
		"txt":   "héllo",
		"bin":   map[string]string{"base64": "/wD+"},
		"ts":    "2026-03-01T02:30:00Z",
		"ts6":   "2026-03-01T09:30:00.123456Z",
		"i":     int64(7),
		"f":     1.5,
		"u":     uint64(1 << 63),
	}
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(res.Rows))
	}
	if !reflect.DeepEqual(res.Rows[0], want) {
		t.Errorf("row = %#v\nwant  %#v", res.Rows[0], want)
	}
	if res.Truncated {
		t.Error("Truncated = true, want false")
	}
	if res.RowCount != 1 {
		t.Errorf("RowCount = %d, want 1", res.RowCount)
	}
}

func TestCollectJSONArrayColumn(t *testing.T) {
	res := mustCollect(t, [][]any{{[]byte(`[1,"x",null]`)}}, []colInfo{{"j", "JSON"}}, bigLimits)
	want := []any{json.Number("1"), "x", nil}
	if !reflect.DeepEqual(res.Rows[0]["j"], want) {
		t.Errorf("got %#v, want %#v", res.Rows[0]["j"], want)
	}
}

func TestCollectJSONKeepsExactBigIntegers(t *testing.T) {
	res := mustCollect(t, [][]any{{[]byte(`{"id":9007199254740993,"f":1.5}`)}}, []colInfo{{"j", "JSON"}}, bigLimits)
	b, err := json.Marshal(res.Rows[0]["j"])
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"f":1.5,"id":9007199254740993}`; string(b) != want {
		t.Errorf("marshaled = %s, want %s (no float64 rounding)", b, want)
	}
}

func TestCollectJSONWithTrailingDataIsText(t *testing.T) {
	res := mustCollect(t, [][]any{{[]byte(`{"a":1} x`)}}, []colInfo{{"j", "JSON"}}, bigLimits)
	if got := res.Rows[0]["j"]; got != `{"a":1} x` {
		t.Errorf("got %#v, want the raw text", got)
	}
}

func TestCollectColumnsAndDuplicates(t *testing.T) {
	res := mustCollect(t, [][]any{{int64(1), int64(2), int64(3)}}, cols("a", "a", "a"), bigLimits)
	if want := []string{"a", "a_2", "a_3"}; !reflect.DeepEqual(res.Columns, want) {
		t.Errorf("Columns = %v, want %v", res.Columns, want)
	}
	want := map[string]any{"a": int64(1), "a_2": int64(2), "a_3": int64(3)}
	if !reflect.DeepEqual(res.Rows[0], want) {
		t.Errorf("row = %v, want %v", res.Rows[0], want)
	}
}

func TestCollectDuplicateNameCollidesWithSuffix(t *testing.T) {
	res := mustCollect(t, nil, cols("a", "a", "a_2"), bigLimits)
	if want := []string{"a", "a_2", "a_2_2"}; !reflect.DeepEqual(res.Columns, want) {
		t.Errorf("Columns = %v, want %v", res.Columns, want)
	}
}

func intRows(n int) [][]any {
	rows := make([][]any, n)
	for i := range rows {
		rows[i] = []any{int64(i)}
	}
	return rows
}

func TestCollectMaxRows(t *testing.T) {
	for _, tc := range []struct {
		name      string
		n, max    int
		wantRows  int
		wantTrunc bool
	}{
		{"over", 7, 5, 5, true},
		{"exact", 5, 5, 5, false},
		{"under", 3, 5, 3, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fr := &fakeRows{rows: intRows(tc.n)}
			res, err := collect(fr, cols("i"), Limits{MaxRows: tc.max, MaxCellBytes: 100, MaxResultBytes: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Rows) != tc.wantRows || res.RowCount != tc.wantRows {
				t.Errorf("rows=%d RowCount=%d, want %d", len(res.Rows), res.RowCount, tc.wantRows)
			}
			if res.Truncated != tc.wantTrunc {
				t.Errorf("Truncated = %v, want %v", res.Truncated, tc.wantTrunc)
			}
			if tc.name == "over" && fr.scanned != 5 {
				t.Errorf("scanned %d rows, want 5 (lookahead row must not be scanned)", fr.scanned)
			}
		})
	}
}

func TestCollectZeroRowsIsEmptyArray(t *testing.T) {
	res := mustCollect(t, nil, cols("a", "b"), bigLimits)
	if res.Rows == nil {
		t.Fatal("Rows is nil, want empty slice")
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"columns":["a","b"],"rows":[],"row_count":0,"truncated":false}`
	if string(b) != want {
		t.Errorf("json = %s, want %s", b, want)
	}
}

func TestCollectCellCap(t *testing.T) {
	lim := Limits{MaxRows: 10, MaxCellBytes: 10, MaxResultBytes: 1 << 20}

	t.Run("ascii", func(t *testing.T) {
		res := mustCollect(t, [][]any{{[]byte(strings.Repeat("x", 100))}}, cols("c"), lim)
		got := res.Rows[0]["c"].(string)
		if want := strings.Repeat("x", 10) + "…[truncated]"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if !res.Truncated {
			t.Error("Truncated = false, want true")
		}
	})
	t.Run("exactly at cap is untouched", func(t *testing.T) {
		res := mustCollect(t, [][]any{{strings.Repeat("x", 10)}}, cols("c"), lim)
		if res.Rows[0]["c"] != strings.Repeat("x", 10) || res.Truncated {
			t.Errorf("got %v truncated=%v", res.Rows[0]["c"], res.Truncated)
		}
	})
	t.Run("multibyte rune is not split", func(t *testing.T) {
		// 4 x 3-byte runes = 12 bytes; cap 10 must cut after 3 runes (9 bytes).
		res := mustCollect(t, [][]any{{[]byte("€€€€")}}, cols("c"), lim)
		got := res.Rows[0]["c"].(string)
		if want := "€€€…[truncated]"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
		if !utf8.ValidString(got) || !res.Truncated {
			t.Errorf("valid=%v truncated=%v", utf8.ValidString(got), res.Truncated)
		}
	})
	t.Run("string value", func(t *testing.T) {
		res := mustCollect(t, [][]any{{strings.Repeat("y", 50)}}, cols("c"), lim)
		if got := res.Rows[0]["c"].(string); !strings.HasSuffix(got, "…[truncated]") || !res.Truncated {
			t.Errorf("got %q truncated=%v", got, res.Truncated)
		}
	})
	t.Run("oversized json is cut as text", func(t *testing.T) {
		doc := `{"k":"` + strings.Repeat("z", 50) + `"}`
		res := mustCollect(t, [][]any{{[]byte(doc)}}, []colInfo{{"j", "JSON"}}, lim)
		got, ok := res.Rows[0]["j"].(string)
		if !ok || !strings.HasSuffix(got, "…[truncated]") || !res.Truncated {
			t.Errorf("got %#v truncated=%v", res.Rows[0]["j"], res.Truncated)
		}
	})
	t.Run("oversized binary keeps first bytes as base64", func(t *testing.T) {
		blob := make([]byte, 100)
		for i := range blob {
			blob[i] = 0xFF
		}
		res := mustCollect(t, [][]any{{blob}}, cols("c"), lim)
		got, ok := res.Rows[0]["c"].(map[string]string)
		if !ok || got["base64"] != "/////////////w==" || !res.Truncated { // 10 bytes of 0xFF
			t.Errorf("got %#v truncated=%v", res.Rows[0]["c"], res.Truncated)
		}
	})
}

func TestCollectResultBytesCap(t *testing.T) {
	rows := make([][]any, 10)
	for i := range rows {
		rows[i] = []any{strings.Repeat("a", 20)}
	}
	res := mustCollect(t, rows, cols("c"), Limits{MaxRows: 100, MaxCellBytes: 100, MaxResultBytes: 50})
	// each row costs 20+8 = 28: the first fits, the second would reach 56 > 50.
	if len(res.Rows) != 1 || res.RowCount != 1 {
		t.Errorf("rows = %d, want 1", len(res.Rows))
	}
	if !res.Truncated {
		t.Error("Truncated = false, want true")
	}
}

func TestCollectErrors(t *testing.T) {
	wantErr := errors.New("boom")

	_, err := collect(&fakeRows{rows: intRows(2), err: wantErr}, cols("i"), bigLimits)
	if !errors.Is(err, wantErr) {
		t.Errorf("Err() error: got %v, want %v", err, wantErr)
	}
	_, err = collect(&fakeRows{rows: intRows(2), scanErr: wantErr}, cols("i"), bigLimits)
	if !errors.Is(err, wantErr) {
		t.Errorf("Scan error: got %v, want %v", err, wantErr)
	}
}

// stoppedEarly tells RunReadOnly that rows are still pending on the wire, so
// the query must be killed instead of drained.
func TestCollectReportsEarlyStop(t *testing.T) {
	small := Limits{MaxRows: 5, MaxCellBytes: 100, MaxResultBytes: 1 << 20}
	for _, tc := range []struct {
		name string
		rows [][]any
		lim  Limits
		want bool
	}{
		{"row cap hit with more rows", intRows(7), small, true},
		{"exactly max rows", intRows(5), small, false},
		{"fewer rows", intRows(2), small, false},
		{"byte cap hit", [][]any{{strings.Repeat("a", 20)}, {strings.Repeat("a", 20)}},
			Limits{MaxRows: 100, MaxCellBytes: 100, MaxResultBytes: 40}, true},
		{"cell cut only is not an early stop", [][]any{{strings.Repeat("a", 50)}},
			Limits{MaxRows: 100, MaxCellBytes: 10, MaxResultBytes: 1 << 20}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := mustCollect(t, tc.rows, cols("c"), tc.lim)
			if res.stoppedEarly != tc.want {
				t.Errorf("stoppedEarly = %v, want %v", res.stoppedEarly, tc.want)
			}
		})
	}
}
