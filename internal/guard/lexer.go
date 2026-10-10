// Package guard is a pure first-line filter that rejects SQL which is not a
// single read-only statement. It has no dependencies.
package guard

import (
	"errors"
	"strings"
)

type kind int

const (
	kWord   kind = iota // keyword or unquoted identifier (uppercased)
	kQuoted             // `identifier` (backticks stripped, uppercased)
	kString             // '...' or "..." literal (text: uppercased raw body)
	kNumber
	kSymbol
)

type token struct {
	kind kind
	text string
	dq   bool // kString delimited by double quotes (an identifier under ANSI_QUOTES)
}

var (
	errUnterminated = errors.New("unterminated string, identifier or comment")
	errVersioned    = errors.New("versioned/optimizer comment not allowed")
)

// lex splits sql into tokens following MySQL's lexical rules:
//   - '...' and "...": a doubled quote is an escape; a backslash escapes the
//     next character only when backslashEscapes is set (the server's
//     NO_BACKSLASH_ESCAPES mode is unknown, so the caller lexes both ways).
//   - `...`: only a doubled backtick is an escape; backslash is literal.
//   - "-- " is a comment only when "--" is followed by space, tab, CR, LF or
//     EOF; otherwise "2--1" is 2 - - 1. "#" runs to end of line.
//   - /* ... */ is a comment, but /*! (executed) and /*+ (optimizer hint)
//     are rejected because their content reaches the server.
//
// On error the tokens read before the error are still returned.
func lex(sql string, backslashEscapes bool) ([]token, error) {
	var toks []token
	for i := 0; i < len(sql); {
		c := sql[i]
		switch {
		case isSpace(c):
			i++
		case c == '#':
			i = skipLine(sql, i)
		case c == '-' && strings.HasPrefix(sql[i:], "--") && (i+2 == len(sql) || isDashSpace(sql[i+2])):
			i = skipLine(sql, i)
		case c == '/' && strings.HasPrefix(sql[i:], "/*"):
			if i+2 < len(sql) && (sql[i+2] == '!' || sql[i+2] == '+') {
				return toks, errVersioned
			}
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return toks, errUnterminated
			}
			i += 2 + end + 2
		case c == '\'' || c == '"':
			end, ok := scanQuoted(sql, i, c, backslashEscapes)
			if !ok {
				return toks, errUnterminated
			}
			toks = append(toks, token{kind: kString, text: strings.ToUpper(sql[i+1 : end-1]), dq: c == '"'})
			i = end
		case c == '`':
			end, ok := scanQuoted(sql, i, c, false)
			if !ok {
				return toks, errUnterminated
			}
			name := strings.ReplaceAll(sql[i+1:end-1], "``", "`")
			toks = append(toks, token{kind: kQuoted, text: strings.ToUpper(name)})
			i = end
		case isIdentChar(c):
			j := i
			for j < len(sql) && isIdentChar(sql[j]) {
				j++
			}
			k := kWord
			if sql[i] >= '0' && sql[i] <= '9' {
				k = kNumber
			}
			toks = append(toks, token{kind: k, text: strings.ToUpper(sql[i:j])})
			i = j
		case c == ':' && i+1 < len(sql) && sql[i+1] == '=':
			toks = append(toks, token{kind: kSymbol, text: ":="})
			i += 2
		default:
			toks = append(toks, token{kind: kSymbol, text: string(c)})
			i++
		}
	}
	return toks, nil
}

// scanQuoted returns the index just past the closing quote of the literal
// starting at sql[start] (the opening quote). ok is false if unterminated.
func scanQuoted(sql string, start int, quote byte, backslash bool) (end int, ok bool) {
	for i := start + 1; i < len(sql); i++ {
		switch {
		case backslash && sql[i] == '\\':
			i++ // skip the escaped character
		case sql[i] == quote:
			if i+1 < len(sql) && sql[i+1] == quote {
				i++ // doubled quote
				continue
			}
			return i + 1, true
		}
	}
	return 0, false
}

func skipLine(sql string, i int) int {
	if n := strings.IndexByte(sql[i:], '\n'); n >= 0 {
		return i + n + 1
	}
	return len(sql)
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

func isDashSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// isIdentChar: letters, digits, '_' and '$'; bytes >= 0x80 (UTF-8 multibyte)
// are valid identifier characters in MySQL.
func isIdentChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '$' || c >= 0x80
}
