package redact

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSQL(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"string and number", "SELECT * FROM u WHERE email='a@b.com' AND id=42", "SELECT * FROM u WHERE email=? AND id=?"},
		{"doubled quote", "SELECT 'it''s' AS v", "SELECT ? AS v"},
		// Read with backslash escapes the literal is 'a\'b'; without them it is
		// 'a\' followed by b and an unterminated literal. Both readings are
		// masked, so everything from the first quote on is hidden.
		{"backslash quote", `SELECT 'a\'b' AS v`, "SELECT ?"},
		{"double-quoted string", `SELECT "x y" , "a""b"`, "SELECT ? , ?"},
		{"backslash escaped double quote", `SELECT "a\"b"`, "SELECT ?"},
		{"backtick identifier kept", "SELECT `col1`, `we'ird` FROM `t 1`", "SELECT `col1`, `we'ird` FROM `t 1`"},
		{"backtick with doubled backtick", "SELECT `a``b'c` FROM t", "SELECT `a``b'c` FROM t"},
		{"number inside identifier kept", "SELECT t1.c2, col1 FROM t1", "SELECT t1.c2, col1 FROM t1"},
		{"decimal exponent hex", "SELECT 1.5, .5, 1e10, 2E-3, 0xFF, 0b101", "SELECT ?, ?, ?, ?, ?, ?"},
		{"limit list", "SELECT a FROM t LIMIT 10,20", "SELECT a FROM t LIMIT ?,?"},
		{"charset introducer and hex string", "SELECT _utf8mb4'sec', X'4142', N'nat'", "SELECT _utf8mb4?, X?, N?"},
		{"sleep", "SELECT SLEEP(10), 'secret-literal-xyz'", "SELECT SLEEP(?), ?"},
		{"whitespace collapsed", "SELECT   a\n\tFROM  t \n WHERE x = 1  ", "SELECT a FROM t WHERE x = ?"},
		{"unterminated string masks to the end", "SELECT 'abc def", "SELECT ?"},
		{"unterminated string ending in backslash", `SELECT 'abc\`, "SELECT ?"},
		{"block comment dropped", "SELECT /* user=bob */ a FROM t", "SELECT a FROM t"},
		{"executable comment dropped", "SELECT /*! 'x' */ a FROM t", "SELECT a FROM t"},
		{"unterminated block comment dropped", "SELECT a /* 'secret", "SELECT a"},
		{"dash comment dropped", "SELECT a -- 'secret'\nFROM t", "SELECT a FROM t"},
		{"hash comment dropped", "SELECT a # secret 42\nFROM t", "SELECT a FROM t"},
		{"double dash without space is arithmetic", "SELECT 1--1", "SELECT ?--?"},
		{"executable comment lexes strings", "SELECT /*! 'a*/b' 'SECRETEXEC' */ 1", "SELECT ?"},
		{"hint comment lexes strings", "SELECT /*+ SET_VAR(x='a*/b') 'SECRETHINT' */ 1", "SELECT ?"},
		{"plain comment ends at the first terminator", "SELECT /* 'a*/ b, 'c'", "SELECT b, ?"},
		{"empty", "", ""},
		{"only whitespace", " \t\n", ""},
		{"non ascii identifier kept", "SELECT é1 FROM t", "SELECT é1 FROM t"},
		{"non ascii literal", "SELECT 'héllo wörld'", "SELECT ?"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SQL(c.in, 1024); got != c.want {
				t.Errorf("SQL(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// Under NO_BACKSLASH_ESCAPES a backslash does not escape a quote, so
// 'C:\' is a complete literal and what follows is a second one. The text is
// masked as if either reading were true.
func TestSQLMasksBothBackslashReadings(t *testing.T) {
	for _, in := range []string{
		`INSERT INTO f VALUES ('C:\', 'tok_SECRETPATH')`,
		`SELECT 'a\', 'SECRETNBE' , 'z'`,
		`SELECT "a\", "SECRETNBE", "z"`,
		`SELECT 'x\' , ` + "`col`" + `, 'SECRETNBE2'`,
		`UPDATE t SET p = 'a\' WHERE k = 'SECRETNBE3' AND n = 5`,
		`SELECT 'a\', /* SECRETNBE4 */ 1`,
	} {
		got := SQL(in, 1024)
		if strings.Contains(got, "SECRET") || strings.ContainsAny(got, `'"`) {
			t.Errorf("SQL(%q) = %q leaks a literal", in, got)
		}
	}
	// A statement without backslashes is unaffected.
	if got := SQL("SELECT 'a', 'b' FROM t WHERE c = 1", 1024); got != "SELECT ?, ? FROM t WHERE c = ?" {
		t.Errorf("plain statement = %q", got)
	}
}

func TestSQLTruncates(t *testing.T) {
	long := strings.Repeat("a ", 1000) // 2000 runes, no literals
	got := SQL(long, 1024)
	if n := utf8.RuneCountInString(got); n != 1025 || !strings.HasSuffix(got, "…") {
		t.Fatalf("got %d runes, suffix %q; want 1024 + the ellipsis", n, got[len(got)-4:])
	}

	// Cut at a rune boundary, never inside a multi-byte rune.
	got = SQL(strings.Repeat("é", 50), 10)
	if got != strings.Repeat("é", 10)+"…" || !utf8.ValidString(got) {
		t.Errorf("multibyte cut = %q", got)
	}

	// Exactly maxLen runes is not cut.
	if got := SQL("SELECT 1", 8); got != "SELECT ?" {
		t.Errorf("no cut needed: %q", got)
	}
	// Redaction happens before the cut: a literal that straddles the limit is gone.
	if got := SQL("SELECT 'abcdefghijklmnop'", 12); strings.Contains(got, "abc") {
		t.Errorf("literal survived the cut: %q", got)
	}
	if got := SQL("SELECT 1", 0); got != "…" && got != "" {
		t.Errorf("maxLen 0 = %q", got)
	}
}

func FuzzSQL(f *testing.F) {
	for _, s := range []string{"SELECT 'a'", `SELECT "b\"`, "x/*", "'", "--", "0x", "1e", "`", "\\"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		out := SQL(s, 1024)
		if !utf8.ValidString(out) && utf8.ValidString(s) {
			t.Fatalf("invalid UTF-8 from valid input %q: %q", s, out)
		}
		if n := utf8.RuneCountInString(out); n > 1025 {
			t.Fatalf("output has %d runes, want at most 1025", n)
		}
	})
}
