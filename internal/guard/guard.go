package guard

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrNotReadOnly is wrapped by every rejection returned from Check.
var ErrNotReadOnly = errors.New("statement not allowed: read-only server")

// MaxSQLBytes is the longest statement Check accepts.
const MaxSQLBytes = 64 << 10

// A false positive (rejecting a valid query) is acceptable here; a false
// negative is caught by the later layers (read-only session, single-use
// connection, minimal grants).
var (
	allowedFirst = set("SELECT", "SHOW", "DESCRIBE", "DESC", "EXPLAIN", "WITH")
	dmlWords     = set("UPDATE", "DELETE", "INSERT", "REPLACE")
	deniedFuncs  = set(
		"GET_LOCK", "RELEASE_LOCK", "RELEASE_ALL_LOCKS", "IS_FREE_LOCK", "IS_USED_LOCK",
		"LOAD_FILE", "BENCHMARK", "SOURCE_POS_WAIT", "MASTER_POS_WAIT",
		"WAIT_FOR_EXECUTED_GTID_SET", "WAIT_UNTIL_SQL_THREAD_AFTER_GTIDS",
		"SERVICE_GET_READ_LOCKS", "SERVICE_GET_WRITE_LOCKS", "SERVICE_RELEASE_LOCKS",
	)
	// Server-internal function families, denied by name prefix.
	deniedFuncPrefixes = []string{
		"VERSION_TOKENS_", "KEYRING_", "AUDIT_LOG_", "GROUP_REPLICATION_",
		"ASYNCHRONOUS_CONNECTION_FAILOVER_", "SERVICE_",
	}
	// Objects that expose other sessions' statement text. The server grants
	// PROCESS for list_active_queries, which redacts literals; execute_sql
	// must not be a way around that redaction.
	deniedIdents = set(
		"PROCESSLIST", "INNODB_TRX", "EVENTS_STATEMENTS_CURRENT", "EVENTS_STATEMENTS_HISTORY",
		"EVENTS_STATEMENTS_HISTORY_LONG", "PROCESSLIST_INFO", "SQL_TEXT", "QUERY_SAMPLE_TEXT",
		"TRX_QUERY", "USER_VARIABLES_BY_THREAD", "SESSION_CONNECT_ATTRS",
	)
	// SHOW is allowed only for these objects (anything may follow them).
	showObjects = [][]string{
		{"TABLES"}, {"FULL", "TABLES"}, {"COLUMNS"}, {"FULL", "COLUMNS"}, {"FIELDS"}, {"FULL", "FIELDS"},
		{"INDEX"}, {"INDEXES"}, {"KEYS"}, {"CREATE", "TABLE"}, {"CREATE", "VIEW"},
		{"TABLE", "STATUS"}, {"DATABASES"}, {"SCHEMAS"},
	}
	// Adjacent-token sequences; "@" matches the @ symbol.
	deniedSeqs = [][]string{
		{"INTO", "OUTFILE"}, {"INTO", "DUMPFILE"}, {"INTO", "@"},
		{"FOR", "UPDATE"}, {"FOR", "SHARE"}, {"LOCK", "IN", "SHARE", "MODE"},
	}
)

func set(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

func reject(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNotReadOnly, fmt.Sprintf(format, args...))
}

// Check returns nil if sql is a single read-only statement. Otherwise the
// returned error wraps ErrNotReadOnly and says why.
//
// The server may run with NO_BACKSLASH_ESCAPES, which changes where string
// literals end, so the statement is lexed both ways and must pass both.
func Check(sql string) error {
	if len(sql) > MaxSQLBytes {
		return reject("statement too long (max %d bytes)", MaxSQLBytes)
	}
	if !utf8.ValidString(sql) {
		return reject("invalid UTF-8")
	}
	for i := 0; i < len(sql); i++ {
		// MySQL treats some control bytes as whitespace or comment
		// delimiters; refuse them rather than model each one.
		if c := sql[i]; c < 0x20 && c != '\t' && c != '\n' && c != '\r' || c == 0x7f {
			return reject("control character not allowed")
		}
	}
	for _, backslashEscapes := range []bool{true, false} {
		toks, err := lex(sql, backslashEscapes)
		if err != nil {
			// Tokens before the error are well-formed; a rule violation in
			// them is a more specific reason than the lexing error.
			if len(toks) > 0 {
				if rerr := rules(toks); rerr != nil {
					return rerr
				}
			}
			return reject("%s", err)
		}
		if err := rules(toks); err != nil {
			return err
		}
	}
	return nil
}

// rules applies the statement-level checks to one lexing of the statement.
func rules(toks []token) error {
	if n := len(toks); n > 0 && isSym(toks[n-1], ";") {
		toks = toks[:n-1]
	}
	for _, t := range toks {
		if isSym(t, ";") {
			return reject("multiple statements not allowed")
		}
	}
	first := 0
	for first < len(toks) && isSym(toks[first], "(") {
		first++
	}
	if first == len(toks) {
		return reject("empty statement")
	}
	kw := toks[first]
	if kw.kind != kWord {
		return reject("statement must start with an allowed keyword")
	}
	if !allowedFirst[kw.text] {
		return reject("keyword %s not allowed", kw.text)
	}

	with := -1 // index of the WITH that starts the statement, if any
	switch kw.text {
	case "SHOW":
		if err := checkShow(toks[first+1:]); err != nil {
			return err
		}
	case "WITH":
		with = first
	case "EXPLAIN", "DESCRIBE", "DESC":
		var err error
		if with, err = explainTarget(toks, first+1); err != nil {
			return err
		}
	}
	if with >= 0 {
		if err := withHasDML(toks, with); err != nil {
			return err
		}
	}
	return denylist(toks)
}

// explainTarget validates what follows EXPLAIN/DESCRIBE/DESC at toks[i]:
// a SELECT/WITH statement or a table name (DESC tbl [column]). It returns
// the index of the WITH keyword when the target is a WITH statement, else -1.
func explainTarget(toks []token, i int) (int, error) {
	// Skip [ANALYZE] [EXTENDED] [FORMAT = x] in any order.
	for i < len(toks) {
		if isWord(toks[i], "ANALYZE") || isWord(toks[i], "EXTENDED") {
			i++
		} else if isWord(toks[i], "FORMAT") && i+2 < len(toks) && isSym(toks[i+1], "=") {
			i += 3
		} else {
			break
		}
	}
	if i >= len(toks) {
		return -1, reject("EXPLAIN/DESCRIBE needs a statement or table name")
	}
	if toks[i].kind == kWord && dmlWords[toks[i].text] {
		return -1, reject("keyword %s not allowed in EXPLAIN/DESCRIBE", toks[i].text)
	}
	j := i
	for j < len(toks) && isSym(toks[j], "(") {
		j++
	}
	if j < len(toks) {
		switch {
		case isWord(toks[j], "SELECT"):
			return -1, nil
		case isWord(toks[j], "WITH"):
			return j, nil
		}
	}
	if j > i {
		return -1, reject("EXPLAIN/DESCRIBE target not allowed")
	}
	// Table form: name [. name] [column | 'pattern'] then end of statement.
	if !isIdent(toks[i]) {
		return -1, reject("EXPLAIN/DESCRIBE target not allowed")
	}
	i++
	if i+1 < len(toks) && isSym(toks[i], ".") && isIdent(toks[i+1]) {
		i += 2
	}
	if i < len(toks) && (isIdent(toks[i]) || toks[i].kind == kString) {
		i++
	}
	if i != len(toks) {
		return -1, reject("EXPLAIN/DESCRIBE target not allowed")
	}
	return -1, nil
}

// withHasDML rejects WITH ... UPDATE/DELETE/INSERT/REPLACE: DML after the
// CTE list sits outside the parentheses that contain the CTE bodies.
func withHasDML(toks []token, start int) error {
	depth := 0
	for _, t := range toks[start:] {
		switch {
		case isSym(t, "("):
			depth++
		case isSym(t, ")"):
			depth--
		case depth <= 0 && t.kind == kWord && dmlWords[t.text]:
			return reject("keyword %s not allowed in WITH statement", t.text)
		}
	}
	return nil
}

// checkShow allows SHOW only for the objects in showObjects. rest is the
// token list after SHOW.
func checkShow(rest []token) error {
	for _, obj := range showObjects {
		if len(rest) >= len(obj) {
			match := true
			for i, w := range obj {
				match = match && isWord(rest[i], w)
			}
			if match {
				return nil
			}
		}
	}
	if len(rest) == 0 {
		return reject("SHOW needs an object")
	}
	x := rest[0].text
	if (isWord(rest[0], "FULL") || isWord(rest[0], "CREATE")) && len(rest) > 1 {
		x += " " + rest[1].text
	}
	return reject("SHOW %s not allowed", x)
}

// denylist rejects write/lock/exfiltration constructs anywhere in the statement.
func denylist(toks []token) error {
	for i := range toks {
		if reason := denied(toks, i); reason != "" {
			return reject("%s", reason)
		}
	}
	return nil
}

// denied returns why the construct starting at toks[i] is not allowed, or "".
func denied(toks []token, i int) string {
	t := toks[i]
	if isSym(t, ":=") {
		return "user variable assignment not allowed"
	}
	for _, seq := range deniedSeqs {
		if matchSeq(toks[i:], seq) {
			return strings.Join(seq, " ") + " not allowed"
		}
	}
	if !isName(t) {
		return ""
	}
	if deniedIdents[t.text] {
		return "access to " + t.text + " not allowed"
	}
	// A function call is a name followed by "(" (whitespace and comments
	// between them are not tokens).
	if i+1 < len(toks) && isSym(toks[i+1], "(") && (deniedFuncs[t.text] || hasDeniedPrefix(t.text)) {
		return "function " + t.text + " not allowed"
	}
	return ""
}

func hasDeniedPrefix(name string) bool {
	for _, p := range deniedFuncPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func matchSeq(toks []token, seq []string) bool {
	if len(toks) < len(seq) {
		return false
	}
	for i, want := range seq {
		t := toks[i]
		if !(isName(t) || t.kind == kSymbol) || t.text != want {
			return false
		}
	}
	return true
}

func isSym(t token, s string) bool  { return t.kind == kSymbol && t.text == s }
func isWord(t token, s string) bool { return t.kind == kWord && t.text == s }
func isIdent(t token) bool          { return t.kind == kWord || t.kind == kQuoted }

// isName: identifier-like token. A "..." literal counts because it is an
// identifier under ANSI_QUOTES, which the server may enable; '...' never does.
func isName(t token) bool { return isIdent(t) || (t.kind == kString && t.dq) }
