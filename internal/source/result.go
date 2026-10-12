// Package source runs read-only queries against configured MySQL databases.
package source

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"time"
	"unicode/utf8"
)

// Limits bounds what one query may cost and return.
type Limits struct {
	MaxRows        int
	MaxCellBytes   int
	MaxResultBytes int
	MaxConcurrent  int // sizes the connection pool (MaxConcurrent+2)
	Timeout        time.Duration
}

// Result is the JSON shape returned to tool callers.
type Result struct {
	Columns   []string         `json:"columns"`
	Rows      []map[string]any `json:"rows"`
	RowCount  int              `json:"row_count"`
	Truncated bool             `json:"truncated"`

	// stoppedEarly is set when collect stopped reading while rows may still be
	// pending on the wire (row or byte cap): the caller must kill the query
	// rather than let rows.Close drain it.
	stoppedEarly bool
}

// colInfo is the part of sql.ColumnType that collect needs.
type colInfo struct{ Name, DBType string }

// rowScanner is the subset of *sql.Rows that collect uses (fakeable in tests).
type rowScanner interface {
	Next() bool
	Scan(...any) error
	Err() error
}

const (
	truncatedMarker = "…[truncated]"
	scalarOverhead  = 8 // approximate JSON cost per cell, on top of its bytes
)

// collect reads rs into a Result, converting values by column type and
// enforcing the row, cell and total-size caps. It never closes rs.
//
// Row cap: MaxRows rows are returned; one more Next() call (without Scan)
// detects whether the result was cut. Size cap: each cell costs its length plus
// a fixed overhead; a row that would push the total over MaxResultBytes is
// dropped and the result is marked truncated.
func collect(rs rowScanner, cols []colInfo, lim Limits) (*Result, error) {
	names := uniqueNames(cols)
	res := &Result{Columns: names, Rows: []map[string]any{}}

	vals := make([]any, len(cols))
	dests := make([]any, len(cols))
	for i := range vals {
		dests[i] = &vals[i]
	}

	total := 0
	for rs.Next() {
		if len(res.Rows) >= lim.MaxRows {
			res.Truncated = true
			res.stoppedEarly = true
			break
		}
		for i := range vals {
			vals[i] = nil
		}
		if err := rs.Scan(dests...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		rowSize := 0
		for i, v := range vals {
			cv, size, cut := convert(v, cols[i].DBType, lim.MaxCellBytes)
			row[names[i]] = cv
			rowSize += size + scalarOverhead
			if cut {
				res.Truncated = true
			}
		}
		if total+rowSize > lim.MaxResultBytes {
			res.Truncated = true
			res.stoppedEarly = true
			break
		}
		total += rowSize
		res.Rows = append(res.Rows, row)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	res.RowCount = len(res.Rows)
	return res, nil
}

// uniqueNames returns the column names with duplicates suffixed: a, a_2, a_3.
// A generated name that collides with a real column keeps counting (a_2_2).
func uniqueNames(cols []colInfo) []string {
	names := make([]string, len(cols))
	seen := make(map[string]bool, len(cols))
	for i, c := range cols {
		name := c.Name
		for n := 2; seen[name]; n++ {
			name = fmt.Sprintf("%s_%d", c.Name, n)
		}
		seen[name] = true
		names[i] = name
	}
	return names
}

// convert maps one driver value to its JSON-friendly form. size is the
// approximate encoded length; cut reports that the cell cap was applied.
func convert(v any, dbType string, maxCell int) (out any, size int, cut bool) {
	switch x := v.(type) {
	case nil:
		return nil, 0, false
	case []byte:
		return convertBytes(x, dbType, maxCell)
	case string:
		s, cut := capString(x, maxCell)
		return s, len(s), cut
	case time.Time:
		s := x.UTC().Format(time.RFC3339Nano)
		return s, len(s), false
	default:
		return v, scalarOverhead, false
	}
}

func convertBytes(b []byte, dbType string, maxCell int) (any, int, bool) {
	if len(b) > maxCell {
		// A cut document is not valid JSON, so oversized JSON becomes text.
		if prefix := b[:maxCell]; !utf8.Valid(cutRuneBoundary(prefix)) {
			// Binary: keep the first bytes, no marker (it would corrupt base64).
			s := base64.StdEncoding.EncodeToString(prefix)
			return map[string]string{"base64": s}, len(s), true
		}
		s := string(cutRuneBoundary(b[:maxCell])) + truncatedMarker
		return s, len(s), true
	}
	if dbType == "JSON" {
		if doc, ok := decodeJSON(b); ok {
			return doc, len(b), false
		}
		// Invalid JSON text falls through and is treated like any other bytes.
	}
	if utf8.Valid(b) {
		return string(b), len(b), false
	}
	s := base64.StdEncoding.EncodeToString(b)
	return map[string]string{"base64": s}, len(s), false
}

// decodeJSON parses one JSON document. Numbers stay json.Number so integers
// beyond 2^53 survive exactly; trailing data makes it invalid.
func decodeJSON(b []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return doc, true
}

// capString cuts s to at most max bytes on a rune boundary and appends the
// truncation marker.
func capString(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	return string(cutRuneBoundary([]byte(s[:max]))) + truncatedMarker, true
}

// cutRuneBoundary drops a trailing incomplete UTF-8 sequence (at most 3
// bytes) so a byte cut never splits a rune. Invalid bytes are left alone.
func cutRuneBoundary(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if utf8.FullRune(b[i:]) {
				return b
			}
			return b[:i]
		}
	}
	return b
}
