package explain

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/amirradjou/exitcheck/internal/explorer"
	"github.com/amirradjou/exitcheck/internal/facts"
)

// chunk is one retrievable passage.
type chunk struct {
	ref, title, text string
	terms            []string
}

// maxChunkLines bounds a source passage: a model reading a whole token template stops citing the lines that matter.
const maxChunkLines = 14

// stopwords are words that say nothing about what a passage is for: English glue, and Solidity's own vocabulary,
// which is on every line of every contract.
var stopwords = func() map[string]bool {
	m := map[string]bool{}
	for w := range strings.FieldsSeq(`a an and are as at be been but by can could did do does for from had has have he her
		his how i if in into is it its may more most no nor not of on once one only or other our out over own she so
		some such than that the their them then there these they this those to too up very was we were what when where
		which while who whom why will with would you your about after again all also any because before being both
		each few just should now
		function public private internal external view pure payable returns return memory calldata storage override
		virtual require revert emit uint uint8 uint24 uint112 uint256 int256 bool address string bytes msg sender true
		false mapping struct event modifier constructor contract interface library pragma solidity import new var let`) {
		m[w] = true
	}
	return m
}()

// tokenize lowercases s and splits it into words, breaking identifiers apart (setSellTax → set, sell, tax), so that
// plain-language reasons match code.
func tokenize(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 1 {
			if w := string(cur); !stopwords[w] {
				out = append(out, w)
			}
		}
		cur = cur[:0]
	}
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case unicode.IsUpper(r):
			// A new word starts at an upper-case letter after a lower-case one, or at the last capital of an acronym.
			if i > 0 && (unicode.IsLower(rs[i-1]) || (i+1 < len(rs) && unicode.IsLower(rs[i+1]) && unicode.IsUpper(rs[i-1]))) {
				flush()
			}
			cur = append(cur, unicode.ToLower(r))
		case unicode.IsLower(r) || unicode.IsDigit(r):
			cur = append(cur, r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// stems is tokenize with each word stemmed, so a reason's "taxes" finds setSellTax.
func stems(s string) []string {
	out := tokenize(s)
	for i, w := range out {
		out[i] = stem(w)
	}
	return out
}

// rank orders chunks by BM25 against the query and returns the best n that share a term with it. Lexical on purpose:
// the query is made of the checks' own words and function names, the source uses the same names, and retrieval then
// needs no model and no network.
func rank(query string, chunks []chunk, n int) []chunk {
	const k1, b = 1.2, 0.75
	df := map[string]int{}
	total := 0
	for i := range chunks {
		if chunks[i].terms == nil {
			chunks[i].terms = stems(chunks[i].title + " " + chunks[i].text)
		}
		total += len(chunks[i].terms)
		seen := map[string]bool{}
		for _, t := range chunks[i].terms {
			if !seen[t] {
				seen[t] = true
				df[t]++
			}
		}
	}
	if len(chunks) == 0 || total == 0 {
		return nil
	}
	avg := float64(total) / float64(len(chunks))
	seen := map[string]bool{}
	var q []string
	for _, t := range stems(query) {
		if !seen[t] {
			seen[t] = true
			q = append(q, t)
		}
	}
	type scored struct {
		i int
		s float64
	}
	var hits []scored
	for i, c := range chunks {
		tf := map[string]int{}
		for _, t := range c.terms {
			tf[t]++
		}
		s := 0.0
		for _, t := range q {
			f := float64(tf[t])
			if f == 0 {
				continue
			}
			idf := math.Log(1 + (float64(len(chunks))-float64(df[t])+0.5)/(float64(df[t])+0.5))
			s += idf * f * (k1 + 1) / (f + k1*(1-b+b*float64(len(c.terms))/avg))
		}
		if s > 0 {
			hits = append(hits, scored{i, s})
		}
	}
	// The sort must not depend on map order or the source's file order changing: equal scores keep chunk order.
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].s > hits[b].s })
	var out []chunk
	for _, h := range hits[:min(n, len(hits))] {
		out = append(out, chunks[h.i])
	}
	return out
}

var functionRE = regexp.MustCompile(`\b(?:function\s+[A-Za-z_]\w*|constructor|receive|fallback|modifier\s+[A-Za-z_]\w*)\s*\(?`)

// sourceChunks splits verified source into functions, one chunk each. Comments and string literals are blanked first:
// they are the token author's free text, the natural place for instructions aimed at a language model, and the code
// says what the contract does anyway. Files are visited in name order, so a chunk's number never depends on map order.
func sourceChunks(src *explorer.Source) []chunk {
	if src == nil {
		return nil
	}
	var names []string
	for name := range src.Files {
		if strings.HasSuffix(name, ".sol") || !strings.Contains(name, ".") {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	var out []chunk
	for _, name := range names {
		code := facts.BlankComments(src.Files[name])
		// A file's name is the author's text too: it can hold a newline, a link or a claim.
		short := clean(name[strings.LastIndexByte(name, '/')+1:], 60)
		for _, loc := range functionRE.FindAllStringIndex(code, -1) {
			open := strings.IndexAny(code[loc[0]:], "{;")
			if open < 0 || code[loc[0]+open] == ';' {
				continue // a declaration without a body
			}
			end := matchBrace(code, loc[0]+open)
			first := 1 + strings.Count(code[:loc[0]], "\n")
			lines := strings.Split(code[loc[0]:end], "\n")
			var kept []string
			for _, l := range lines[:min(maxChunkLines, len(lines))] {
				if l = strings.TrimSpace(l); l != "" {
					kept = append(kept, l)
				}
			}
			last := first + min(maxChunkLines, len(lines)) - 1
			out = append(out, chunk{
				ref: fmt.Sprintf("%s:%d-%d", short, first, last), title: code[loc[0]:loc[1]],
				text: strings.Join(kept, "\n"),
			})
		}
	}
	return out
}

// matchBrace returns the offset just past the brace closing the one at open, or len(src).
func matchBrace(src string, open int) int {
	depth := 0
	for i := open; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				return i + 1
			}
		}
	}
	return len(src)
}
