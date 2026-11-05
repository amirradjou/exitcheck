package explain

import (
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Sentence is one sentence of an explanation, with what it rests on.
type Sentence struct {
	Text  string   `json:"text"`
	Cites []string `json:"cites"`
}

// Dropped is a sentence the model wrote that did not survive, and why.
type Dropped struct {
	Text string `json:"text"`
	Why  string `json:"why"`
}

// Why a sentence was dropped.
const (
	whyNoCitation  = "cites nothing"
	whyUnknownCite = "cites evidence that does not exist"
	whyPatternOnly = "cites only the pattern library, not this token"
	whyAssurance   = "reassures or advises"
	whyLink        = "carries a link or address the evidence does not"
	whyNumber      = "has a number the evidence does not"
	whyOffTopic    = "says more than what it cites"
	whyFragment    = "is not a whole sentence"
	whyMixes       = "joins two findings"
	whyNoFinding   = "does not state the finding it cites"
	whyTooLong     = "too long"
	whyRepeat      = "repeats an earlier sentence"
)

const (
	maxSentences = 3
	maxWords     = 45
	// minOverlap is the share of a sentence's content words that must come from what it cites. A citation alone proves
	// nothing: a model will happily append [C1] to a claim C1 never made ("this could lead to financial manipulation").
	// This keeps explanations close to the checks' own words, which are already written for a reader.
	minOverlap = 2.0 / 3
	// minSourceWords is how many words a sentence must share with a source passage for citing it to count. Code shares
	// a word or two with anything; a citation added for show is removed, and a sentence resting only on those goes.
	minSourceWords = 2
	// minFindingWords is how many words a sentence must share with the finding it cites. Overlap with the whole citation is
	// not enough: "after a pull there is almost nothing left to sell into" can take all its words from a pattern and
	// say nothing about this token, while its citation of a finding only looks like grounding.
	minFindingWords = 2
)

var (
	citeRE = regexp.MustCompile(`\[\s*([A-Za-z]\d+(?:\s*[,;/ ]\s*[A-Za-z]\d+)*)\s*\]`)
	// A sentence ends at punctuation followed by a space or the end, taking along any citations written after it, or
	// at a line break. "0.05" and "file.sol" do not end one.
	endRE              = regexp.MustCompile(`[.!?]+((?:\s*\[[^\]\n]*\])*)(?:\s+|$)|\n+`)
	spaceBeforePunctRE = regexp.MustCompile(`\s+([.,;:!?])`)
	listRE             = regexp.MustCompile(`^\s*(?:[-*•]|\d+[.)])\s+`)
	addrRE             = regexp.MustCompile(`(?i)\b0x[0-9a-f]{6,}\b`)
	linkRE             = regexp.MustCompile(`(?i)https?://|www\.|\bt\.me/|(?:^|\s)@\w{3,}`)
	domainRE           = regexp.MustCompile(`(?i)\b[a-z0-9-]+\.[a-z]{2,}\b`)
	numberRE           = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?`)
	// wordNumberRE keeps identifiers out of the number check: the 2 in v2 or ERC-20's 20 is a name, not a claim.
	claimNumberRE = regexp.MustCompile(`(?:^|[^\w.-])(\d[\d,]*(?:\.\d+)?)`)
	// orphanRE is a sentence that leans on the one before it. That one may have been dropped, and "That can collapse the
	// price" says nothing on its own.
	orphanRE = regexp.MustCompile(`^(?:(?:This|That|These|Those)\s+|It\b|They\b|Such\b|Which\b|Its\b|Their\b|Both\b)`)
	// ownSubjectRE is the exception: "This token" and "That wallet" name what they talk about.
	ownSubjectRE = regexp.MustCompile(`^(?:This|That|These|Those)\s+(?:token|contract|pool|wallet|owner|deployer|function|sale|buy|tax|proxy)s?\b`)
	// assuranceRE is what an explanation of a verdict must never say, whatever it cites: the verdict is not the
	// model's to soften, and exitcheck does not give advice. A contract's text can ask for exactly this.
	assuranceRE = regexp.MustCompile(`(?i)\b(?:safe|safely|legit|legitimate|trustworthy|trusted|guarantee[ds]?|no risk|risk[- ]free|` +
		`not a scam|isn'?t a scam|good investment|should (?:buy|sell|invest|avoid|hold)|financial advice|recommend\w*|` +
		`no (?:obvious|immediate|apparent|major) \w+|no (?:security )?(?:issues?|problems?|concerns?)|as expected|` +
		`nothing (?:wrong|suspicious)|worth considering|best practices?|further (?:review|investigation))\b`)
	// speculationRE is a model reaching past its evidence, in words overlap cannot catch: in "the owner can create unlimited
	// new tokens" every other word comes from the finding. It is allowed only if the evidence says it too.
	speculationRE = regexp.MustCompile(`(?i)\b(?:potential(?:ly)?|manipulat\w*|hidden risks?|vulnerabilit\w*|exploit\w*|unlimited|` +
		`massive(?:ly)?|highly|definitely|certain(?:ly)?|always|never|immediately|instantly|wipes?|crash\w*)\b`)
)

var abbreviations = strings.NewReplacer("e.g.", "for example", "i.e.", "that is", "E.g.", "For example", "I.e.", "That is")

// split cuts a model's answer into sentences, each keeping its citations. Formatting a model may add goes first.
func split(answer string) []string {
	answer = strings.NewReplacer("**", "", "__", "", "`", "", "\r", "").Replace(answer)
	answer = abbreviations.Replace(answer)
	var out []string
	add := func(s string) {
		s = strings.Join(strings.Fields(listRE.ReplaceAllString(s, "")), " ")
		switch {
		case s == "":
		case len(out) > 0 && citeRE.ReplaceAllString(s, "") == "":
			out[len(out)-1] += " " + s // citations alone, on their own line, belong to the sentence before
		default:
			out = append(out, s)
		}
	}
	pos := 0
	for _, loc := range endRE.FindAllStringIndex(answer, -1) {
		add(answer[pos:loc[1]])
		pos = loc[1]
	}
	add(answer[pos:])
	return out
}

// cited returns the IDs a sentence cites, upper-cased and in order, and the sentence without them.
func cited(s string) (ids []string, rest string) {
	for _, m := range citeRE.FindAllStringSubmatch(s, -1) {
		for id := range strings.FieldsFuncSeq(m[1], func(r rune) bool { return strings.ContainsRune(",;/ ", r) }) {
			if id = strings.ToUpper(id); !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	rest = strings.Join(strings.Fields(citeRE.ReplaceAllString(s, "")), " ")
	return ids, spaceBeforePunctRE.ReplaceAllString(rest, "$1") // "the taxes [C1]." loses its citation, not its period
}

// stem reduces a word to its first five letters after plural and tense endings: enough to match withdraw and
// withdrawn, tax and taxes, or pause and pausing, without pulling in a stemmer.
func stem(w string) string {
	for _, suffix := range []string{"ing", "ed", "es", "s"} {
		if len(w) >= len(suffix)+3 && strings.HasSuffix(w, suffix) {
			w = strings.TrimSuffix(w, suffix)
			break
		}
	}
	if len(w) > 3 {
		w = strings.TrimSuffix(w, "e")
	}
	if len(w) > 5 {
		return w[:5]
	}
	return w
}

func contentWords(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range tokenize(s) {
		if len(w) >= 3 {
			out[stem(w)] = true
		}
	}
	return out
}

func parseNumber(s string) (float64, bool) {
	x, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	return x, err == nil
}

// numbersSupported says whether every number claimed in s appears in the evidence. A whole number may round the
// evidence (98 for 97.6); a number written with decimals has to match them.
func numbersSupported(s, evidence string) bool {
	var known []float64
	for _, m := range numberRE.FindAllString(evidence, -1) {
		if x, ok := parseNumber(m); ok {
			known = append(known, x)
		}
	}
	for _, m := range claimNumberRE.FindAllStringSubmatch(addrRE.ReplaceAllString(s, ""), -1) {
		x, ok := parseNumber(m[1])
		if !ok {
			continue
		}
		tolerance := 0.5
		if strings.Contains(m[1], ".") {
			tolerance = 0.05
		}
		if !slices.ContainsFunc(known, func(k float64) bool { return math.Abs(k-x) <= tolerance }) {
			return false
		}
	}
	return true
}

// Ground keeps the sentences of a model's answer that rest on the evidence, in order and at most maxSentences, and
// says why it dropped the rest. Nothing a model writes reaches a reader without passing here. names are what the token
// calls itself: a sentence may repeat them, and "Freedom 250" has a number no evidence needs to back.
func Ground(answer string, names []string, evidence []Evidence) (kept []Sentence, dropped []Dropped) {
	byID := map[string]Evidence{}
	for _, e := range evidence {
		byID[e.ID] = e
	}
	seen := map[string]bool{}
	for _, raw := range split(answer) {
		ids, text := cited(raw)
		ids, why := rejection(ids, text, names, byID)
		if why == "" && seen[strings.ToLower(text)] {
			why = whyRepeat
		}
		if why != "" {
			dropped = append(dropped, Dropped{Text: raw, Why: why})
			continue
		}
		seen[strings.ToLower(text)] = true
		if len(kept) == maxSentences {
			dropped = append(dropped, Dropped{Text: raw, Why: whyTooLong})
			continue
		}
		kept = append(kept, Sentence{Text: text, Cites: ids})
	}
	return kept, dropped
}

// withoutNames removes the token's own names from a sentence, as whole phrases. They are checked as nothing: a name is the
// author's text, so its words and numbers must not count as evidence. A token called "LP locked 10 years" must not
// ground the claim that the liquidity is locked for 10 years.
func withoutNames(text string, names []string) string {
	for _, n := range names {
		if len([]rune(n)) < 3 { // a one-letter symbol would take letters out of every word
			continue
		}
		re, err := regexp.Compile(`(?i)(^|\W)` + regexp.QuoteMeta(n) + `($|\W)`)
		if err == nil {
			text = re.ReplaceAllString(text, "${1} ${2}")
		}
	}
	return text
}

func hasLink(text string) bool {
	if linkRE.MatchString(text) {
		return true
	}
	for _, d := range domainRE.FindAllString(text, -1) {
		if !strings.HasSuffix(strings.ToLower(d), ".sol") { // a citation may name its source file
			return true
		}
	}
	return false
}

// rejection returns the citations that hold up and why a sentence fails, or "".
func rejection(ids []string, sentence string, names []string, byID map[string]Evidence) ([]string, string) {
	if len(ids) == 0 {
		return nil, whyNoCitation
	}
	for _, id := range ids {
		if _, ok := byID[id]; !ok {
			return nil, whyUnknownCite
		}
	}
	if r := []rune(sentence); len(r) == 0 || !(unicode.IsUpper(r[0]) || unicode.IsDigit(r[0])) {
		return nil, whyFragment // what a citation at the start of a sentence leaves behind
	}
	if orphanRE.MatchString(sentence) && !ownSubjectRE.MatchString(sentence) {
		return nil, whyFragment
	}
	if len(strings.Fields(sentence)) > maxWords {
		return nil, whyTooLong
	}
	text := withoutNames(sentence, names)
	if assuranceRE.MatchString(text) {
		return nil, whyAssurance
	}
	// One finding per sentence. Every word of "the deployer holds 50% of the supply and could withdraw the liquidity"
	// comes from one of two findings; the claim comes from neither.
	findings := 0
	for _, id := range ids {
		if byID[id].Kind == KindCheck {
			findings++
		}
	}
	if findings > 1 {
		return nil, whyMixes
	}
	words := contentWords(text)
	var keep []string
	grounded := false
	var body, refs strings.Builder
	for _, id := range ids {
		e := byID[id]
		if e.Kind == KindSource && sharedWords(words, contentWords(e.Text)) < minSourceWords {
			continue // cited for show
		}
		keep = append(keep, id)
		grounded = grounded || e.Kind != KindPattern
		body.WriteString(e.Text + "\n")
		refs.WriteString(e.Ref + "\n")
		if e.Kind == KindPattern {
			body.WriteString(e.Ref + "\n") // our own title, unlike a source file's name
		}
	}
	if !grounded {
		if len(keep) > 0 {
			return nil, whyPatternOnly
		}
		return nil, whyOffTopic
	}
	for _, id := range keep {
		if e := byID[id]; e.Kind == KindCheck {
			finding := contentWords(e.Text)
			if sharedWords(words, finding) < min(minFindingWords, len(finding)) {
				return nil, whyNoFinding
			}
		}
	}
	evidence := body.String()
	lower := strings.ToLower(evidence)
	if hasLink(text) {
		return nil, whyLink
	}
	for _, a := range addrRE.FindAllString(text, -1) {
		if !strings.Contains(lower, strings.ToLower(a)) {
			return nil, whyLink
		}
	}
	// A source reference's line numbers may be repeated ("Token.sol:178"), so numbers also look at the references.
	if !numbersSupported(text, evidence+refs.String()) {
		return nil, whyNumber
	}
	for _, m := range speculationRE.FindAllString(text, -1) {
		if !strings.Contains(lower, strings.ToLower(m)) {
			return nil, whyOffTopic
		}
	}
	have := contentWords(evidence)
	if len(words) == 0 || float64(sharedWords(words, have))/float64(len(words)) < minOverlap {
		return nil, whyOffTopic
	}
	return keep, ""
}

func sharedWords(a, b map[string]bool) int {
	n := 0
	for w := range a {
		if b[w] {
			n++
		}
	}
	return n
}
