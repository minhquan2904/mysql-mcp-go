package guard

import (
	"errors"
	"strings"
	"testing"
)

func TestAllow(t *testing.T) {
	for _, sql := range AllowCases {
		t.Run(sql, func(t *testing.T) {
			if err := Check(sql); err != nil {
				t.Errorf("Check(%q) = %v, want nil", sql, err)
			}
		})
	}
}

func TestReject(t *testing.T) {
	for _, tc := range RejectCases {
		name := tc.SQL
		if len(name) > 40 {
			name = name[:40]
		}
		t.Run(name, func(t *testing.T) {
			err := Check(tc.SQL)
			if err == nil {
				t.Fatalf("Check(%q) = nil, want rejection containing %q", tc.SQL, tc.Reason)
			}
			if !errors.Is(err, ErrNotReadOnly) {
				t.Errorf("error %v does not wrap ErrNotReadOnly", err)
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.Reason)) {
				t.Errorf("error %q does not contain %q", err, tc.Reason)
			}
		})
	}
}

// The two lexings disagree on these: each is allowed by one mode but the
// overall verdict must be a rejection.
func TestBothLexingsMustPass(t *testing.T) {
	for _, sql := range []string{
		`SELECT 'a\' , GET_LOCK('x',1), '`,
		`SELECT "a\" , GET_LOCK('x',1), "`,
		"SELECT `a\\` , GET_LOCK('app',1), `b`",
	} {
		if err := Check(sql); err == nil {
			t.Errorf("Check(%q) = nil, want rejection", sql)
		}
	}
	// Backslash mode alone sees only literals here.
	toks, err := lex(`SELECT 'a\' , GET_LOCK('x',1), '`, true)
	if err != nil || rules(toks) != nil {
		t.Errorf("backslash-mode lexing should pass rules, got err=%v rules=%v", err, rules(toks))
	}
	toks, _ = lex(`SELECT 'a\' , GET_LOCK('x',1), '`, false)
	if rules(toks) == nil {
		t.Error("no-backslash lexing must expose GET_LOCK")
	}
}

func TestExtraRejects(t *testing.T) {
	for _, tc := range []Case{
		{"SELECT 1;;", "multiple statements"},
		{"(DELETE FROM t)", "DELETE"},
		{"EXPLAIN FORMAT=JSON DELETE FROM t", "DELETE"},
		{"EXPLAIN DELETE t", "DELETE"},
		{"EXPLAIN FOR CONNECTION 5", "EXPLAIN"},
		{"(WITH c AS (SELECT 1) DELETE FROM t)", "DELETE"},
		{"EXPLAIN WITH c AS (SELECT 1) UPDATE t SET a=1", "UPDATE"},
		{"SELECT \"GET_LOCK\"('x',1)", "GET_LOCK"},
		{"SELECT 1 INTO\n@v", "INTO @"},
		{"SELECT 1 INTO/**/OUTFILE '/tmp/x'", "OUTFILE"},
		{"SELECT * FROM t LOCK/**/IN SHARE MODE", "LOCK IN SHARE MODE"},
		{"SELECT RELEASE_ALL_LOCKS()", "RELEASE_ALL_LOCKS"},
		{"SELECT 1 FROM t WHERE x:=1", "assignment"},
		{"(", "empty"},
		{"WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c WHERE n<3) DELETE FROM t", "DELETE"},
		{"SHOW", "SHOW"},
		{"SHOW FULL", "SHOW FULL not allowed"},
		{"SHOW CREATE", "SHOW CREATE not allowed"},
		{"SHOW CREATE DATABASE d", "SHOW CREATE DATABASE not allowed"},
		{"SHOW FULL TABLESPACES", "SHOW FULL TABLESPACES not allowed"},
		{"SELECT * FROM \"PROCESSLIST\"", "access to PROCESSLIST not allowed"},
		{"SELECT * FROM t \"FOR\" \"UPDATE\"", "FOR UPDATE"},
		{"SELECT 1 INTO \"OUTFILE\" 'x'", "INTO OUTFILE"},
		{"SELECT x FROM t WHERE y IN (SELECT 1 FROM innodb_trx)", "access to INNODB_TRX not allowed"},
		{"SELECT x FROM events_statements_current", "access to EVENTS_STATEMENTS_CURRENT not allowed"},
		{"SELECT x FROM events_statements_history_long", "access to EVENTS_STATEMENTS_HISTORY_LONG not allowed"},
		{"SELECT query_sample_text FROM t", "access to QUERY_SAMPLE_TEXT not allowed"},
		{"SELECT x FROM user_variables_by_thread", "access to USER_VARIABLES_BY_THREAD not allowed"},
		{"SELECT x FROM session_connect_attrs", "access to SESSION_CONNECT_ATTRS not allowed"},
		{"SELECT 1\r\n\x01", "control character not allowed"},
	} {
		err := Check(tc.SQL)
		if err == nil || !errors.Is(err, ErrNotReadOnly) || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.Reason)) {
			t.Errorf("Check(%q) = %v, want rejection containing %q", tc.SQL, err, tc.Reason)
		}
	}
}

func TestExtraAllows(t *testing.T) {
	for _, sql := range []string{
		"EXPLAIN db.orders",
		"DESC orders id",
		"DESC orders 'id%'",
		"EXPLAIN FORMAT=TREE SELECT 1",
		"EXPLAIN EXTENDED SELECT 1",
		"EXPLAIN ANALYZE FORMAT=TREE SELECT 1",
		"EXPLAIN (SELECT 1)",
		"((SELECT 1))",
		"SELECT 'get_lock(' FROM t",
		"SELECT get_lock_col FROM t",
		"SELECT 1 -- DELETE\n",
		"SELECT 1 # DELETE",
		"SELECT /* DELETE; */ 1",
		"WITH c AS (SELECT 1) SELECT * FROM c;",
		"WITH c AS (SELECT 1) SELECT * FROM c WHERE a = 'UPDATE'",
		"SELECT * FROM `delete` WHERE `for` = 1",
		"SELECT x FROM t FOR\nUPDATE_AT",
		"SHOW FULL TABLES",
		"SHOW FULL COLUMNS FROM t",
		"SHOW FIELDS FROM t",
		"SHOW FULL FIELDS FROM t",
		"SHOW INDEXES FROM t",
		"SHOW KEYS FROM t",
		"SHOW CREATE TABLE db.t",
		"SHOW CREATE VIEW v",
		"SHOW DATABASES",
		"SHOW SCHEMAS LIKE 'a%'",
		"SHOW TABLES FROM db WHERE Tables_in_db = 'x'",
		"SELECT 1\t\r\n",
		"SELECT 'ünï' FROM t",
	} {
		if err := Check(sql); err != nil {
			t.Errorf("Check(%q) = %v, want nil", sql, err)
		}
	}
}

func TestSizeLimit(t *testing.T) {
	exact := "SELECT 1" + strings.Repeat(" ", MaxSQLBytes-len("SELECT 1"))
	if len(exact) != MaxSQLBytes {
		t.Fatalf("setup: len = %d", len(exact))
	}
	if err := Check(exact); err != nil {
		t.Errorf("statement of exactly MaxSQLBytes: %v", err)
	}
	if err := Check(exact + " "); err == nil || !errors.Is(err, ErrNotReadOnly) || !strings.Contains(err.Error(), "too long") {
		t.Errorf("MaxSQLBytes+1 = %v, want too long", err)
	}
}

// violation is an independent oracle for the denylist: it returns a
// description of the first denied construct in toks, or "".
func violation(toks []token) string {
	for i, tok := range toks {
		if tok.kind == kSymbol && tok.text == ":=" {
			return ":="
		}
		if tok.kind == kSymbol || tok.kind == kNumber || (tok.kind == kString && !tok.dq) {
			continue
		}
		if deniedIdents[tok.text] {
			return "identifier " + tok.text
		}
		if i+1 < len(toks) && toks[i+1].kind == kSymbol && toks[i+1].text == "(" {
			if deniedFuncs[tok.text] {
				return "function " + tok.text
			}
			for _, p := range deniedFuncPrefixes {
				if strings.HasPrefix(tok.text, p) {
					return "function " + tok.text
				}
			}
		}
		for _, seq := range deniedSeqs {
			if i+len(seq) > len(toks) {
				continue
			}
			ok := true
			for k, w := range seq {
				n := toks[i+k]
				if n.text != w || n.kind == kNumber || (n.kind == kString && !n.dq) {
					ok = false
				}
			}
			if ok {
				return "sequence " + strings.Join(seq, " ")
			}
		}
	}
	return ""
}

func FuzzCheck(f *testing.F) {
	for _, s := range AllowCases {
		f.Add(s)
	}
	for _, c := range RejectCases {
		f.Add(c.SQL)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		if Check(sql) != nil {
			return
		}
		// Allowed: no control bytes, and under both lexings the rules pass,
		// no denied construct is present, and no ";" token remains except
		// as the final token.
		for i := 0; i < len(sql); i++ {
			if c := sql[i]; c < 0x20 && c != '\t' && c != '\n' && c != '\r' || c == 0x7f {
				t.Fatalf("Check(%q) = nil but contains control byte %#x", sql, c)
			}
		}
		for _, bs := range []bool{true, false} {
			toks, err := lex(sql, bs)
			if err != nil {
				t.Fatalf("Check(%q) = nil but lex(backslash=%v) failed: %v", sql, bs, err)
			}
			if err := rules(toks); err != nil {
				t.Fatalf("Check(%q) = nil but rules(backslash=%v) failed: %v", sql, bs, err)
			}
			if v := violation(toks); v != "" {
				t.Fatalf("Check(%q) = nil but backslash=%v lexing contains %s", sql, bs, v)
			}
			for i, tok := range toks {
				if isSym(tok, ";") && i != len(toks)-1 {
					t.Fatalf("Check(%q) = nil but backslash=%v lexing has non-trailing ';'", sql, bs)
				}
			}
		}
	})
}

func TestStartsWithSelect(t *testing.T) {
	for sql, want := range map[string]bool{
		"SELECT 1":                              true,
		"  select * from t;":                    true,
		"(SELECT 1) UNION (SELECT 2)":           true,
		"((SELECT 1))":                          true,
		"WITH c AS (SELECT 1) SELECT * FROM c":  true,
		"/* c */ SELECT 1":                      true,
		"-- c\nSELECT 1":                        true,
		"# c\nSELECT 1":                         true,
		"SHOW TABLES":                           false,
		"EXPLAIN SELECT 1":                      false,
		"EXPLAIN ANALYZE SELECT 1":              false,
		"DESC orders":                           false,
		"DESCRIBE SELECT 1":                     false,
		"":                                      false,
		"((":                                    false,
		"'SELECT'":                              false,
		"`SELECT` 1":                            false,
		"/*!50000 SELECT 1 */":                  false,
		"SELECT 'unterminated":                  true, // first token is still SELECT; Check rejects the statement
		"UPDATE t SET a=1":                      false,
		"\xff SELECT 1":                         false,
		"SELECTED 1":                            false,
		"WITHOUT 1":                             false,
		"(":                                     false,
		"-- only a comment":                     false,
		"SELECT/**/1":                           true,
		"WITH RECURSIVE c AS (SELECT 1) SELECT": true,
	} {
		if got := StartsWithSelect(sql); got != want {
			t.Errorf("StartsWithSelect(%q) = %v, want %v", sql, got, want)
		}
	}
}
