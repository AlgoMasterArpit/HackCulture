package freshness

import (
	"regexp"
	"strings"
	"time"
)

type Kind string

const (
	KindPricing    Kind = "pricing"
	KindVersion    Kind = "version"
	KindRegulation Kind = "regulation"
	KindLeadership Kind = "leadership"
	KindRecency    Kind = "recency"
)

// How long a verified claim of each kind is trusted before it is re-checked.
// These are defaults and are never modified; use Verifier.Windows to override.
var RecheckWindow = map[Kind]time.Duration{
	KindPricing:    24 * time.Hour,
	KindVersion:    72 * time.Hour,
	KindRecency:    24 * time.Hour,
	KindLeadership: 7 * 24 * time.Hour,
	KindRegulation: 7 * 24 * time.Hour,
}

var kindPatterns = []struct {
	kind Kind
	re   *regexp.Regexp
}{
	{KindPricing, regexp.MustCompile(`(?i)[$€£₹]\s?\d|\d\s?(usd|eur|inr|dollars?|cents?)\b|\b(pric(e|es|ed|ing)|costs?|free tier|per (month|year|seat|user|token|million|1m|1k)|/(mo|month|yr|year)\b|subscription|billed)\b`)},
	{KindVersion, regexp.MustCompile(`(?i)\bv?\d+\.\d+(\.\d+)?\b|\b(latest|newest|most recent|deprecated|end[- ]of[- ]life|eol|lts|stable release|current version|version \d+|sunset)\b`)},
	{KindRegulation, regexp.MustCompile(`(?i)\b(regulations?|law|legislation|directive|gdpr|hipaa|compliance|tax rate|gst|vat|effective (from|date|as of)|mandate[sd]?|ruling|statute|enforcement|deadline|legal(ly)? require[sd]?)\b`)},
	{KindLeadership, regexp.MustCompile(`(?i)\b(ceo|cto|cfo|coo|chief \w+ officer|president|chair(man|woman|person)?|founder|head of|prime minister|led by|acquired( by)?|merged with|headquartered|valuation|market cap|employees)\b`)},
	{KindRecency, regexp.MustCompile(`(?i)\b(as of|currently|at present|right now|this year|recently|to date|20\d\d)\b`)},
}

var sentenceEnd = regexp.MustCompile(`[.!?]\s+|\n+`)

type Candidate struct {
	Sentence string
	Kinds    []Kind
}

// Detect is the zero-cost gate: an answer with no candidates is never sent to
// a model or to search.
func Detect(answer string) []Candidate {
	var out []Candidate
	for _, s := range sentenceEnd.Split(answer, -1) {
		s = strings.TrimSpace(s)
		if len(s) < 12 {
			continue
		}
		var kinds []Kind
		for _, p := range kindPatterns {
			if p.re.MatchString(s) {
				kinds = append(kinds, p.kind)
			}
		}
		if len(kinds) > 0 {
			out = append(out, Candidate{Sentence: s, Kinds: kinds})
		}
	}
	return out
}

func validKind(k Kind) bool {
	_, ok := RecheckWindow[k]
	return ok
}
