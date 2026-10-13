package main

import (
	"math"
	"slices"
	"time"
)

// Row is one benchmark cell: the JSON row cmd/bench emits (one per line) and
// cmd/benchreport reads. Latencies are milliseconds, Errors counts the calls
// that failed. The field names are a wire contract: do not rename them.
type Row struct {
	Scenario    string  `json:"scenario"`
	Concurrency int     `json:"concurrency"`
	N           int     `json:"n"`
	RPS         float64 `json:"rps"`
	P50MS       float64 `json:"p50_ms"`
	P95MS       float64 `json:"p95_ms"`
	P99MS       float64 `json:"p99_ms"`
	MaxMS       float64 `json:"max_ms"`
	Errors      int     `json:"errors"`
}

// NewRow summarizes one benchmark cell. lat holds the latency of every call
// that completed (in any order, failed ones included), errs is how many of
// them failed, and elapsed is the wall time the cell took.
func NewRow(scenario string, concurrency int, lat []time.Duration, errs int, elapsed time.Duration) Row {
	return Row{
		Scenario:    scenario,
		Concurrency: concurrency,
		N:           len(lat),
		RPS:         RPS(len(lat), elapsed),
		P50MS:       msFloat(Percentile(lat, 0.50)),
		P95MS:       msFloat(Percentile(lat, 0.95)),
		P99MS:       msFloat(Percentile(lat, 0.99)),
		MaxMS:       msFloat(maxDuration(lat)),
		Errors:      errs,
	}
}

// Percentile returns the p-quantile of ds (0 <= p <= 1) by the nearest-rank
// method: the value at index ceil(p*N)-1 of the sorted slice, so with N=100
// the p95 is the 95th smallest latency. There is no interpolation. ds is not
// modified; an empty slice yields 0.
func Percentile(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sorted := slices.Clone(ds)
	slices.Sort(sorted)
	rank := int(math.Ceil(p * float64(len(sorted))))
	rank = min(max(rank, 1), len(sorted))
	return sorted[rank-1]
}

// RPS returns completed calls per second. It is zero when no call completed or
// when elapsed is not positive.
func RPS(n int, elapsed time.Duration) float64 {
	if n <= 0 || elapsed <= 0 {
		return 0
	}
	return float64(n) / elapsed.Seconds()
}

// ErrorRate returns the share of failed calls as a fraction of n (0..1). It is
// zero when n is not positive.
func ErrorRate(errs, n int) float64 {
	if n <= 0 {
		return 0
	}
	return float64(errs) / float64(n)
}

func maxDuration(ds []time.Duration) time.Duration {
	var max time.Duration
	for _, d := range ds {
		if d > max {
			max = d
		}
	}
	return max
}

func msFloat(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
