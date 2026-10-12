package source

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CheckGrants verifies that the SHOW GRANTS lines of the connected user cannot
// write or reach sensitive schemas. It is an allowlist: the only privileges
// accepted are
//
//   - USAGE and PROCESS on *.* (PROCESS powers list_active_queries);
//   - SELECT and SHOW VIEW on a schema (`db`.*) or table (`db`.`t`), except
//     schemas mysql and sys (checked against wildcard patterns too, so
//     `%`.* is rejected), and except whole-schema grants on
//     performance_schema, whose tables expose other sessions' statements and
//     user variables. On performance_schema only table-level grants on
//     the tables listed in allowedPerfTables are accepted; any other table is
//     a problem.
//
// Everything else is rejected: global SELECT, ALL PRIVILEGES, INSERT, UPDATE,
// DELETE, DDL, FILE, SUPER, dynamic privileges (*_ADMIN), WITH GRANT OPTION,
// role grants, and any line the parser does not understand (fail closed).
// All problems are reported together. Errors never echo the grant lines.
func CheckGrants(grants []string) error {
	var problems []string
	seen := make(map[string]bool)
	for i, line := range grants {
		for _, p := range checkGrant(i+1, line) {
			if !seen[p] {
				seen[p] = true
				problems = append(problems, p)
			}
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("unsafe privileges: %s", strings.Join(problems, "; "))
}

// CheckPrivileges runs SHOW GRANTS for the connected user and applies
// CheckGrants.
func (s *Source) CheckPrivileges(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	rows, err := s.db.QueryContext(ctx, "SHOW GRANTS")
	if err != nil {
		return fmt.Errorf("SHOW GRANTS: %w", err)
	}
	defer rows.Close()

	var grants []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return fmt.Errorf("SHOW GRANTS: %w", err)
		}
		grants = append(grants, g)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("SHOW GRANTS: %w", err)
	}
	return CheckGrants(grants)
}

// grant is one parsed "GRANT privs ON object TO user [WITH GRANT OPTION]" line.
type grant struct {
	privs       []string // upper case, column lists removed
	db, table   string   // unquoted; "*" is a wildcard only when the flag is set
	dbStar      bool
	tableStar   bool
	grantOption bool
}

func (g grant) global() bool { return g.dbStar && g.tableStar }

func (g grant) object() string {
	if g.global() {
		return "*.*"
	}
	t := "`" + g.table + "`"
	if g.tableStar {
		t = "*"
	}
	return "`" + g.db + "`." + t
}

// allowedPerfTables is the complete list of performance_schema tables the
// account may be granted, one per tool that reads it (the tools run fixed SQL;
// execute_sql cannot name performance_schema, see the guard):
//
//	data_locks                           list_all_locks (LOCK_DATA is never selected)
//	table_io_waits_summary_by_table      list_table_stats
//	events_statements_summary_by_digest  show_query_stats (DIGEST_TEXT only)
//
// Every other table is rejected: most of them show other sessions' statements
// with literals, variables, locked key values or connection attributes.
var allowedPerfTables = map[string]bool{
	"data_locks":                          true,
	"table_io_waits_summary_by_table":     true,
	"events_statements_summary_by_digest": true,
}

var errRoleGrant = errors.New("roles not supported (grant privileges to the user directly)")

func checkGrant(n int, line string) []string {
	g, err := parseGrant(line)
	if err != nil {
		if errors.Is(err, errRoleGrant) {
			return []string{fmt.Sprintf("grant %d: %v", n, err)}
		}
		return []string{fmt.Sprintf("grant %d: unrecognized statement", n)}
	}

	var problems []string
	usesSchema := false
	for _, p := range g.privs {
		switch {
		case g.global() && (p == "USAGE" || p == "PROCESS"):
		case g.global():
			problems = append(problems, fmt.Sprintf("global %s is not allowed", p))
		case p == "USAGE":
		case p == "SELECT" || p == "SHOW VIEW":
			usesSchema = true
		default:
			problems = append(problems, fmt.Sprintf("%s on %s is not allowed", p, g.object()))
		}
	}
	if usesSchema {
		for _, schema := range []string{"mysql", "sys"} {
			if schemaMatches(g.db, schema) {
				problems = append(problems, fmt.Sprintf("%s reaches the %s schema", g.object(), schema))
			}
		}
		if g.tableStar && schemaMatches(g.db, "performance_schema") {
			problems = append(problems, fmt.Sprintf("%s grants the whole performance_schema; grant individual tables", g.object()))
		}
	}
	if usesSchema && !g.tableStar && !allowedPerfTables[strings.ToLower(g.table)] && schemaMatches(g.db, "performance_schema") {
		problems = append(problems, fmt.Sprintf("%s is not one of the performance_schema tables the tools read and is not allowed", g.object()))
	}
	if g.grantOption {
		problems = append(problems, fmt.Sprintf("WITH GRANT OPTION on %s is not allowed", g.object()))
	}
	return problems
}

func parseGrant(line string) (grant, error) {
	var g grant
	s := strings.TrimSpace(line)
	if len(s) < 6 || !strings.EqualFold(s[:5], "GRANT") || !isSpace(s[5]) {
		return g, errors.New("not a GRANT")
	}
	s = s[6:]
	mask := topMask(s)

	on := findWord(s, mask, "ON", 0)
	to := findWord(s, mask, "TO", 0)
	if on < 0 {
		if to >= 0 {
			return g, errRoleGrant // GRANT `role`@`%` TO `user`@`%`
		}
		return g, errors.New("no ON clause")
	}
	if to >= 0 && to < on {
		return g, errRoleGrant
	}

	for _, p := range splitTop(s[:on], mask[:on], ',') {
		if i := strings.IndexByte(p, '('); i >= 0 { // column list: SELECT (`a`, `b`)
			p = p[:i]
		}
		if p = strings.ToUpper(strings.Join(strings.Fields(p), " ")); p != "" {
			g.privs = append(g.privs, p)
		}
	}
	if len(g.privs) == 0 {
		return g, errors.New("no privileges")
	}

	rest := s[on+2:]
	rmask := mask[on+2:]
	to = findWord(rest, rmask, "TO", 0)
	if to < 0 {
		return g, errors.New("no TO clause")
	}
	obj := strings.TrimSpace(rest[:to])
	if i := strings.IndexFunc(obj, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }); i > 0 {
		switch strings.ToUpper(obj[:i]) {
		case "TABLE", "FUNCTION", "PROCEDURE":
			obj = strings.TrimSpace(obj[i:])
		}
	}
	if err := parseObject(obj, &g); err != nil {
		return g, err
	}

	for i := to; ; {
		w := findWord(rest, rmask, "WITH", i)
		if w < 0 {
			break
		}
		if f := strings.Fields(rest[w:]); len(f) >= 3 &&
			strings.EqualFold(f[1], "GRANT") && strings.EqualFold(f[2], "OPTION") {
			g.grantOption = true
		}
		i = w + 4
	}
	return g, nil
}

// parseObject parses `db`.`table`, `db`.*, db.* or *.* into g.
func parseObject(obj string, g *grant) error {
	db, dbQuoted, rest, ok := parseIdent(obj)
	if !ok || !strings.HasPrefix(rest, ".") {
		return errors.New("bad object")
	}
	table, tableQuoted, rest, ok := parseIdent(rest[1:])
	if !ok || rest != "" {
		return errors.New("bad object")
	}
	g.db, g.table = db, table
	g.dbStar = !dbQuoted && db == "*"
	g.tableStar = !tableQuoted && table == "*"
	if g.dbStar && !g.tableStar {
		return errors.New("bad object")
	}
	return nil
}

// parseIdent reads a backtick-quoted (a doubled backtick is a literal one) or bare
// identifier from the start of s.
func parseIdent(s string) (name string, quoted bool, rest string, ok bool) {
	if s == "" {
		return "", false, "", false
	}
	if s[0] != '`' {
		i := strings.IndexByte(s, '.')
		if i < 0 {
			i = len(s)
		}
		return s[:i], false, s[i:], i > 0
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] != '`' {
			b.WriteByte(s[i])
			continue
		}
		if i+1 < len(s) && s[i+1] == '`' {
			b.WriteByte('`')
			i++
			continue
		}
		return b.String(), true, s[i+1:], true
	}
	return "", false, "", false // unterminated
}

// topMask marks the bytes of s that are outside quotes (`...`, '...', "...")
// and parentheses. Delimiters themselves are not marked.
func topMask(s string) []bool {
	mask := make([]bool, len(s))
	var quote byte
	depth := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			switch {
			case c == '\\' && quote != '`':
				i++
			case c == quote && i+1 < len(s) && s[i+1] == quote:
				i++
			case c == quote:
				quote = 0
			}
			continue
		}
		switch c {
		case '`', '\'', '"':
			quote = c
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		default:
			mask[i] = depth == 0
		}
	}
	return mask
}

// findWord returns the index of the first top-level occurrence of kw (as a
// whole space-delimited word, case-insensitive) at or after from, or -1.
func findWord(s string, mask []bool, kw string, from int) int {
	for i := from; i+len(kw) <= len(s); i++ {
		if !mask[i] || (i > 0 && !isSpace(s[i-1])) || !strings.EqualFold(s[i:i+len(kw)], kw) {
			continue
		}
		if end := i + len(kw); end == len(s) || isSpace(s[end]) {
			return i
		}
	}
	return -1
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// splitTop splits s at top-level occurrences of sep and trims the parts.
func splitTop(s string, mask []bool, sep byte) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == sep && mask[i] {
			parts = append(parts, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	return append(parts, strings.TrimSpace(s[start:]))
}

// schemaMatches reports whether a GRANT schema pattern (% any string, _ any
// character, \% and \_ literal) can match the schema name, case-insensitively.
func schemaMatches(pattern, name string) bool {
	type tok struct {
		kind byte // '%' any run, '_' any one, 'l' literal r
		r    rune
	}
	var p []tok
	rs := []rune(strings.ToLower(pattern))
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '%':
			p = append(p, tok{kind: '%'})
		case '_':
			p = append(p, tok{kind: '_'})
		case '\\':
			if i+1 < len(rs) {
				i++
			}
			p = append(p, tok{kind: 'l', r: rs[i]})
		default:
			p = append(p, tok{kind: 'l', r: rs[i]})
		}
	}

	// Iterative glob match with one backtrack point: O(len(p)*len(s)), so a
	// pattern full of % cannot blow up.
	s := []rune(strings.ToLower(name))
	pi, si, star, mark := 0, 0, -1, 0
	for si < len(s) {
		switch {
		case pi < len(p) && p[pi].kind == '%':
			star, mark = pi, si
			pi++
		case pi < len(p) && (p[pi].kind == '_' || p[pi].r == s[si]):
			pi++
			si++
		case star >= 0:
			mark++
			pi, si = star+1, mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi].kind == '%' {
		pi++
	}
	return pi == len(p)
}
