// Package redact removes literal values from SQL text, so statements seen in
// the process list or lock tables can be shown without the data they carry.
package redact

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// number matches a numeric literal at the start of a string: hexadecimal,
// binary, or decimal with optional fraction and exponent.
var number = regexp.MustCompile(`^(?:0[xX][0-9a-fA-F]+|0[bB][01]+|(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?)`)

// Byte classes produced by classify.
const (
	cPlain = iota // outside any literal, comment or quoted identifier
	cLit          // inside a string literal, quotes included
	cCom          // inside a comment, delimiters included
	cIdent        // inside a backtick-quoted identifier, backticks included
)

// SQL returns s with every string literal ('…' or "…", with doubled-quote and
// backslash escapes) and number literal replaced by ?, comments removed and
// whitespace collapsed, cut to maxLen runes (plus an ellipsis when cut).
// Identifiers (including backtick-quoted ones and names with digits such as
// t1.c2) are kept.
//
// Whether a backslash escapes a quote depends on the server's sql_mode
// (NO_BACKSLASH_ESCAPES), which is not known here, so s is lexed twice, with
// and without backslash escapes, and a byte is kept only when it is outside
// every literal and comment in both readings. When the readings disagree this
// masks more than either would. An unterminated literal or comment runs to the
// end of the text.
func SQL(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	with, without := classify(s, true), classify(s, false)
	class := make([]byte, len(s))
	for i := range class {
		a, b := with[i], without[i]
		switch {
		case a == cLit || b == cLit:
			class[i] = cLit
		case a == cCom || b == cCom:
			class[i] = cCom
		case a == cIdent || b == cIdent:
			class[i] = cIdent
		}
	}

	var out strings.Builder
	space := false // whitespace pending before the next emitted token
	emit := func(tok string) {
		if space && out.Len() > 0 {
			out.WriteByte(' ')
		}
		space = false
		out.WriteString(tok)
	}

	for i := 0; i < len(s); {
		// The run of bytes with the same class: hidden classes (literal and
		// comment) form one run, so adjacent hidden regions give one token.
		end := i + 1
		for end < len(s) && sameRun(class[i], class[end]) {
			end++
		}
		switch class[i] {
		case cLit, cCom:
			lit := false
			for _, c := range class[i:end] {
				lit = lit || c == cLit
			}
			if lit {
				emit("?")
			} else {
				space = true
			}
		case cIdent:
			emit(s[i:end])
		default:
			plain(s, i, end, emit, &space)
		}
		i = end
	}

	res := out.String()
	if utf8.RuneCountInString(res) > maxLen {
		cut := 0
		for n := 0; n < maxLen; n++ {
			_, sz := utf8.DecodeRuneInString(res[cut:])
			cut += sz
		}
		res = res[:cut] + "…"
	}
	return res
}

func sameRun(a, b byte) bool {
	hidden := func(c byte) bool { return c == cLit || c == cCom }
	if hidden(a) && hidden(b) {
		return true
	}
	return a == b
}

// plain tokenizes s[i:end], which lies outside every literal, comment and
// quoted identifier: whitespace is collapsed, numbers become ?, words and
// punctuation are kept.
func plain(s string, i, end int, emit func(string), space *bool) {
	for i < end {
		r, size := utf8.DecodeRuneInString(s[i:end])
		switch {
		case unicode.IsSpace(r):
			*space = true
			i += size

		case isIdentRune(r):
			j := i
			for j < end {
				rr, sz := utf8.DecodeRuneInString(s[j:end])
				if !isIdentRune(rr) {
					break
				}
				j += sz
			}
			if s[i] >= '0' && s[i] <= '9' {
				if n := number.FindString(s[i:]); n != "" && !identAt(s, i+len(n)) {
					emit("?")
					i += len(n)
					continue
				}
			}
			emit(s[i:j])
			i = j

		case r == '.' && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9' && !prevIsName(s, i):
			if n := number.FindString(s[i:]); n != "" && !identAt(s, i+len(n)) {
				emit("?")
				i += len(n)
				continue
			}
			emit(".")
			i++

		default:
			emit(s[i : i+size])
			i += size
		}
	}
}

// classify labels every byte of s as plain, literal, comment or quoted
// identifier. backslash selects whether a backslash escapes the next byte
// inside quoted strings.
func classify(s string, backslash bool) []byte {
	class := make([]byte, len(s))
	mark := func(from, to int, c byte) {
		for k := from; k < to; k++ {
			class[k] = c
		}
	}
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == '\'' || c == '"':
			j := endOfString(s, i, backslash)
			mark(i, j, cLit)
			i = j
		case c == '`':
			j := endOfBacktick(s, i)
			mark(i, j, cIdent)
			i = j
		case strings.HasPrefix(s[i:], "/*"):
			j := endOfBlockComment(s, i, backslash)
			mark(i, j, cCom)
			i = j
		case c == '#', strings.HasPrefix(s[i:], "--") && dashComment(s[i+2:]):
			j := len(s)
			if nl := strings.IndexByte(s[i:], '\n'); nl >= 0 {
				j = i + nl + 1
			}
			mark(i, j, cCom)
			i = j
		default:
			i++
		}
	}
	return class
}

// endOfBlockComment returns the index just past the block comment starting at
// i. A plain /* */ comment ends at the first */. The executable (/*!) and
// optimizer-hint (/*+) comments hold SQL, so a */ inside a string there does
// not end them: strings are skipped, which can only make the comment longer
// and hide more.
func endOfBlockComment(s string, i int, backslash bool) int {
	if !strings.HasPrefix(s[i:], "/*!") && !strings.HasPrefix(s[i:], "/*+") {
		if end := strings.Index(s[i+2:], "*/"); end >= 0 {
			return i + 2 + end + 2
		}
		return len(s)
	}
	for j := i + 3; j < len(s); {
		switch {
		case s[j] == '\'' || s[j] == '"':
			j = endOfString(s, j, backslash)
		case strings.HasPrefix(s[j:], "*/"):
			return j + 2
		default:
			j++
		}
	}
	return len(s)
}

// dashComment reports whether "--" followed by rest starts a comment: MySQL
// requires whitespace (or the end of the text) after the second dash.
func dashComment(rest string) bool {
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return unicode.IsSpace(r) || unicode.IsControl(r)
}

// endOfString returns the index just past the quoted string starting at i;
// backslash says whether a backslash escapes the next byte.
func endOfString(s string, i int, backslash bool) int {
	q := s[i]
	for j := i + 1; j < len(s); j++ {
		switch {
		case s[j] == '\\' && backslash:
			j++ // skip the escaped byte
		case s[j] == q:
			if j+1 < len(s) && s[j+1] == q { // doubled quote
				j++
				continue
			}
			return j + 1
		}
	}
	return len(s)
}

// endOfBacktick returns the index just past the backtick-quoted identifier
// starting at i; only a doubled backtick escapes.
func endOfBacktick(s string, i int) int {
	for j := i + 1; j < len(s); j++ {
		if s[j] == '`' {
			if j+1 < len(s) && s[j+1] == '`' {
				j++
				continue
			}
			return j + 1
		}
	}
	return len(s)
}

func isIdentRune(r rune) bool {
	return r == '_' || r == '$' || r >= utf8.RuneSelf || r >= '0' && r <= '9' ||
		r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
}

// identAt reports whether an identifier character sits at s[i].
func identAt(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return isIdentRune(r)
}

// prevIsName reports whether the character before s[i] ends a name, so that a
// "." there is a qualifier dot and not the start of a number like .5.
func prevIsName(s string, i int) bool {
	if i == 0 {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return isIdentRune(r) || r == '`'
}
