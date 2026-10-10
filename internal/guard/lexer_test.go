package guard

import (
	"strings"
	"testing"
)

// show renders tokens compactly: W:word Q:quoted S:string N:number Y:symbol.
func show(toks []token) string {
	parts := make([]string, len(toks))
	for i, t := range toks {
		p := "Y"
		switch t.kind {
		case kWord:
			p = "W"
		case kQuoted:
			p = "Q"
		case kString:
			p = "S"
		case kNumber:
			p = "N"
		}
		if t.kind == kString {
			parts[i] = p
		} else {
			parts[i] = p + ":" + t.text
		}
	}
	return strings.Join(parts, " ")
}

func TestLex(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		bs   bool
		want string // token rendering
		err  string // error substring, "" = no error
	}{
		{"semicolon in string and line comment", "SELECT 'a;b' -- c;\n", true, "W:SELECT S", ""},
		{"double dash without space is two minus", "SELECT 2--1", true, "W:SELECT N:2 Y:- Y:- N:1", ""},
		{"double dash then tab", "SELECT 1 --\tx", true, "W:SELECT N:1", ""},
		{"double dash then newline", "SELECT 1 --\nFROM t", true, "W:SELECT N:1 W:FROM W:T", ""},
		{"double dash at EOF", "SELECT 1 --", true, "W:SELECT N:1", ""},
		{"backslash literal in backtick", "SELECT `a\\`", true, `W:SELECT Q:A\`, ""},
		{"backslash literal in backtick, no-backslash mode", "SELECT `a\\`", false, `W:SELECT Q:A\`, ""},
		{"doubled backtick", "SELECT `a``b`", true, "W:SELECT Q:A`B", ""},
		{"backslash quote, backslash mode", `SELECT 'a\'b'`, true, "W:SELECT S", ""},
		{"backslash quote, no-backslash mode", `SELECT 'a\'b'`, false, "W:SELECT S W:B", "unterminated"},
		{"doubled single quote", "SELECT 'a''b'", false, "W:SELECT S", ""},
		{"doubled double quote", `SELECT "a""b"`, true, "W:SELECT S", ""},
		{"backslash escape at EOF", `SELECT 'a\`, true, "W:SELECT", "unterminated"},
		{"hash comment", "SELECT 1 # DROP; x\nFROM t", true, "W:SELECT N:1 W:FROM W:T", ""},
		{"block comment", "SELECT /* multi\nline; */ 1", true, "W:SELECT N:1", ""},
		{"versioned comment", "SELECT 1 /*!50000 DROP TABLE t */", true, "W:SELECT N:1", "versioned"},
		{"optimizer comment", "SELECT /*+ BKA(t) */ 1", true, "W:SELECT", "optimizer"},
		{"unterminated block comment", "SELECT 1 /* x", true, "W:SELECT N:1", "unterminated"},
		{"unterminated string", "SELECT 'x", true, "W:SELECT", "unterminated"},
		{"unterminated backtick", "SELECT `x", true, "W:SELECT", "unterminated"},
		{"assign operator", "SELECT @x := 1", true, "W:SELECT Y:@ W:X Y::= N:1", ""},
		{"colon and equals apart", "SELECT : =", true, "W:SELECT Y:: Y:=", ""},
		{"lowercase uppercased", "select Foo_1$ from t", true, "W:SELECT W:FOO_1$ W:FROM W:T", ""},
		{"punctuation", "f(a,b);", true, "W:F Y:( W:A Y:, W:B Y:) Y:;", ""},
		{"number then exponent", "1e9", true, "N:1E9", ""},
		{"double-quoted literal text", `"get_lock"(`, true, "S Y:(", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			toks, err := lex(tc.sql, tc.bs)
			if got := show(toks); got != tc.want {
				t.Errorf("tokens = %q, want %q", got, tc.want)
			}
			switch {
			case tc.err == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
				t.Errorf("error = %v, want substring %q", err, tc.err)
			}
		})
	}
}

func TestLexStringText(t *testing.T) {
	toks, err := lex(`"get_lock"`, true)
	if err != nil || len(toks) != 1 || toks[0].kind != kString || !toks[0].dq || toks[0].text != "GET_LOCK" {
		t.Fatalf("got %+v, %v", toks, err)
	}
	toks, _ = lex(`'x'`, true)
	if len(toks) != 1 || toks[0].dq {
		t.Fatalf("single-quoted string must not be dq: %+v", toks)
	}
}
