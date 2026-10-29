package facts

import "regexp"

// blankComments replaces Solidity comments and string literals with spaces, keeping newlines, so offsets and line
// numbers still match the original and commented-out code is not mistaken for live code.
func blankComments(src string) string {
	b := []byte(src)
	for i := 0; i < len(b); i++ {
		switch {
		case b[i] == '/' && i+1 < len(b) && b[i+1] == '/':
			for ; i < len(b) && b[i] != '\n'; i++ {
				b[i] = ' '
			}
		case b[i] == '/' && i+1 < len(b) && b[i+1] == '*':
			for ; i < len(b) && !(b[i] == '*' && i+1 < len(b) && b[i+1] == '/'); i++ {
				if b[i] != '\n' {
					b[i] = ' '
				}
			}
			if i+1 < len(b) {
				b[i], b[i+1] = ' ', ' '
				i++
			}
		case b[i] == '"' || b[i] == '\'':
			q := b[i]
			for i++; i < len(b) && b[i] != q && b[i] != '\n'; i++ {
				if b[i] == '\\' && i+1 < len(b) {
					b[i] = ' '
					i++
				}
				b[i] = ' '
			}
		}
	}
	return string(b)
}

var unitDecl = regexp.MustCompile(`\b(contract|interface|library)\s+[A-Za-z_]\w*[^{;]*\{`)

// span is a byte range [start, end).
type span struct{ start, end int }

// declarationsOnly returns the bodies of interfaces and libraries in comment-free source. Functions declared there
// are not functions of the token: an interface only names another contract's functions (whose selectors still show
// up in the token's bytecode when it calls them), and library code is not dispatched by selector.
func declarationsOnly(src string) []span {
	var out []span
	pos := 0
	for pos < len(src) {
		loc := unitDecl.FindStringSubmatchIndex(src[pos:])
		if loc == nil {
			break
		}
		kind := src[pos+loc[2] : pos+loc[3]]
		open := pos + loc[1] - 1
		end := matchBrace(src, open)
		if kind != "contract" {
			out = append(out, span{open, end})
		}
		pos = end
	}
	return out
}

// matchBrace returns the offset just past the brace that closes the one at open, or len(src).
func matchBrace(src string, open int) int {
	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return len(src)
}

func inSpans(spans []span, off int) bool {
	for _, s := range spans {
		if off >= s.start && off < s.end {
			return true
		}
	}
	return false
}
