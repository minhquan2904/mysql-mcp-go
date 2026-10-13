package main

import (
	"encoding/json"
	"testing"
	"time"
)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

func TestPercentileNearestRank(t *testing.T) {
	var lat []time.Duration
	for i := 1; i <= 100; i++ {
		lat = append(lat, ms(i))
	}
	for _, tc := range []struct {
		p    float64
		want time.Duration
	}{
		{0, ms(1)},
		{0.5, ms(50)},
		{0.95, ms(95)},
		{0.99, ms(99)},
		{1, ms(100)},
	} {
		if got := Percentile(lat, tc.p); got != tc.want {
			t.Errorf("Percentile(lat, %v) = %v, want %v", tc.p, got, tc.want)
		}
	}
}

func TestPercentileRankIsCeilOfPN(t *testing.T) {
	// N=4: rank(0.5) = ceil(2) = 2, so the p50 is the second value, not the
	// midpoint of the two middle ones (nearest rank, no interpolation).
	lat := []time.Duration{ms(10), ms(20), ms(30), ms(40)}
	if got := Percentile(lat, 0.5); got != ms(20) {
		t.Errorf("p50 = %v, want %v", got, ms(20))
	}
	if got := Percentile(lat, 0.75); got != ms(30) {
		t.Errorf("p75 = %v, want %v", got, ms(30))
	}
	if got := Percentile(lat, 0.95); got != ms(40) {
		t.Errorf("p95 = %v, want %v", got, ms(40))
	}
}

func TestPercentileSortsACopy(t *testing.T) {
	lat := []time.Duration{ms(30), ms(10), ms(20)}
	if got := Percentile(lat, 0.5); got != ms(20) {
		t.Errorf("p50 = %v, want %v", got, ms(20))
	}
	want := []time.Duration{ms(30), ms(10), ms(20)}
	for i := range lat {
		if lat[i] != want[i] {
			t.Fatalf("Percentile modified its input: %v, want %v", lat, want)
		}
	}
}

func TestPercentileSingleValueIsEveryQuantile(t *testing.T) {
	lat := []time.Duration{ms(7)}
	for _, p := range []float64{0, 0.5, 0.95, 0.99, 1} {
		if got := Percentile(lat, p); got != ms(7) {
			t.Errorf("Percentile([7ms], %v) = %v, want 7ms", p, got)
		}
	}
}

func TestPercentileEmptyIsZeroWithoutPanic(t *testing.T) {
	for _, p := range []float64{0, 0.5, 0.95, 0.99, 1} {
		if got := Percentile(nil, p); got != 0 {
			t.Errorf("Percentile(nil, %v) = %v, want 0", p, got)
		}
	}
}

func TestRPS(t *testing.T) {
	for _, tc := range []struct {
		name    string
		n       int
		elapsed time.Duration
		want    float64
	}{
		{"two seconds", 100, 2 * time.Second, 50},
		{"sub-second window", 3, 250 * time.Millisecond, 12},
		{"no calls", 0, 2 * time.Second, 0},
		{"no time", 100, 0, 0},
		{"negative time", 100, -time.Second, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RPS(tc.n, tc.elapsed); got != tc.want {
				t.Errorf("RPS(%d, %v) = %v, want %v", tc.n, tc.elapsed, got, tc.want)
			}
		})
	}
}

func TestErrorRate(t *testing.T) {
	for _, tc := range []struct {
		name string
		errs int
		n    int
		want float64
	}{
		{"all good", 0, 10, 0},
		{"quarter failed", 1, 4, 0.25},
		{"all failed", 10, 10, 1},
		{"no calls", 5, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ErrorRate(tc.errs, tc.n); got != tc.want {
				t.Errorf("ErrorRate(%d, %d) = %v, want %v", tc.errs, tc.n, got, tc.want)
			}
		})
	}
}

func TestNewRowSummarizesLatencies(t *testing.T) {
	var lat []time.Duration
	for i := 1; i <= 100; i++ {
		lat = append(lat, ms(i))
	}
	row := NewRow("S2", 8, lat, 5, 2*time.Second)
	if row.Scenario != "S2" || row.Concurrency != 8 {
		t.Errorf("identity = %q c=%d, want S2 c=8", row.Scenario, row.Concurrency)
	}
	if row.N != 100 {
		t.Errorf("n = %d, want 100", row.N)
	}
	if row.RPS != 50 {
		t.Errorf("rps = %v, want 50", row.RPS)
	}
	if row.P50MS != 50 || row.P95MS != 95 || row.P99MS != 99 || row.MaxMS != 100 {
		t.Errorf("latency = p50 %v p95 %v p99 %v max %v, want 50/95/99/100 ms",
			row.P50MS, row.P95MS, row.P99MS, row.MaxMS)
	}
	if row.Errors != 5 {
		t.Errorf("errors = %d, want 5", row.Errors)
	}
}

func TestNewRowEmptyIsAllZeros(t *testing.T) {
	row := NewRow("S1", 1, nil, 0, 0)
	if row.N != 0 || row.RPS != 0 || row.P50MS != 0 || row.P95MS != 0 ||
		row.P99MS != 0 || row.MaxMS != 0 || row.Errors != 0 {
		t.Errorf("row = %+v, want all zeros", row)
	}
}

func TestRowJSONFieldNames(t *testing.T) {
	// The wire contract shared with cmd/benchreport: these names must not
	// drift. The values are exactly representable in float64 so the encoding
	// is unambiguous.
	row := Row{
		Scenario:    "S2",
		Concurrency: 8,
		N:           2130,
		RPS:         71.5,
		P50MS:       109.25,
		P95MS:       141.75,
		P99MS:       180.375,
		MaxMS:       402.9375,
		Errors:      0,
	}
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"scenario":"S2","concurrency":8,"n":2130,"rps":71.5,"p50_ms":109.25,"p95_ms":141.75,"p99_ms":180.375,"max_ms":402.9375,"errors":0}`
	if string(b) != want {
		t.Errorf("json = %s\nwant %s", b, want)
	}
}
