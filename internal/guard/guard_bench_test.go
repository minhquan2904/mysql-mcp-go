package guard

import (
	"strconv"
	"strings"
	"testing"
)

// The three statement sizes the benchmark report quotes: a realistic point
// lookup, a bulk id list of 1 KiB and the longest statement Check accepts.
// Built once so every iteration measures Check, not test setup.
var benchStatements = []struct {
	name string
	sql  string
}{
	{"short", "SELECT id FROM orders WHERE id = 1"},
	{"medium", benchIDList(1 << 10)},
	{"max", benchIDList(MaxSQLBytes)},
}

// benchIDList builds "SELECT ... WHERE id IN (1, 2, ...)" of exactly size
// bytes: integer literals are appended while the closing paren still fits and
// the remainder is padded with spaces. The input is identical on every run, so
// the size classes stay comparable across runs.
func benchIDList(size int) string {
	const head = "SELECT id, email, name, created_at, status, total FROM orders WHERE id IN ("
	if size <= len(head)+1 {
		panic("guard bench: size smaller than the statement head")
	}
	var b strings.Builder
	b.Grow(size)
	b.WriteString(head)
	for n := 1; b.Len() < size-1; n++ {
		item := strconv.Itoa(n)
		if n > 1 {
			item = ", " + item
		}
		if b.Len()+len(item) > size-1 {
			break
		}
		b.WriteString(item)
	}
	b.WriteString(strings.Repeat(" ", size-b.Len()-1))
	b.WriteString(")")
	return b.String()
}

func BenchmarkCheck(b *testing.B) {
	for _, tc := range benchStatements {
		b.Run(tc.name, func(b *testing.B) {
			if err := Check(tc.sql); err != nil {
				b.Fatalf("Check(%d-byte %s statement): %v", len(tc.sql), tc.name, err)
			}
			b.SetBytes(int64(len(tc.sql)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if err := Check(tc.sql); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
