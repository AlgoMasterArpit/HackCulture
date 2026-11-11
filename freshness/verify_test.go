package freshness

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeLLM struct {
	replies []string
	calls   int
	err     error
}

func (f *fakeLLM) CompleteJSON(context.Context, string, string) (string, int, error) {
	if f.err != nil {
		return "", 0, f.err
	}
	r := f.replies[f.calls]
	f.calls++
	return r, 100, nil
}

type fakeSearch struct {
	sources []Source
	calls   int
}

func (f *fakeSearch) Search(context.Context, string) ([]Source, error) {
	f.calls++
	return f.sources, nil
}

var fixedNow = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

func newVerifier(llm LLM, search Searcher) *Verifier {
	return &Verifier{LLM: llm, Search: search, MaxClaims: 5, Now: func() time.Time { return fixedNow }}
}

const pricingClaim = `{"claims":[{"text":"Acme Pro costs $10 per month","kind":"pricing","query":"Acme Pro price"}]}`

var acmeSource = []Source{{
	URL:       "https://acme.test/pricing",
	Content:   "Acme Pro is now $15 per month, billed annually.",
	Relevance: 0.9,
}}

func TestDetect(t *testing.T) {
	cases := []struct {
		text string
		want []Kind
	}{
		{"The Pro plan costs $20 per month for each seat.", []Kind{KindPricing}},
		{"The latest stable release is 3.12.4 at the moment.", []Kind{KindVersion}},
		{"Satya Nadella is the CEO of Microsoft.", []Kind{KindLeadership}},
		{"GDPR fines can reach four percent of turnover.", []Kind{KindRegulation}},
		{"A binary search halves the search space on every step.", nil},
		{"Photosynthesis converts light into chemical energy in plants.", nil},
	}
	for _, c := range cases {
		got := Detect(c.text)
		if c.want == nil {
			if len(got) != 0 {
				t.Errorf("%q: expected no candidates, got %v", c.text, got)
			}
			continue
		}
		if len(got) != 1 || !containsKind(got[0].Kinds, c.want[0]) {
			t.Errorf("%q: want kind %v, got %v", c.text, c.want, got)
		}
	}
}

func containsKind(kinds []Kind, k Kind) bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}

func TestTimelessAnswerCostsNothing(t *testing.T) {
	llm, search := &fakeLLM{}, &fakeSearch{}
	r := newVerifier(llm, search).Verify(context.Background(), "", "A binary search halves the search space on every step.")
	if r.Status != StatusTimeless || r.Score != 1 || llm.calls != 0 || search.calls != 0 {
		t.Fatalf("timeless answer should skip model and search: %+v llm=%d search=%d", r, llm.calls, search.calls)
	}
}

func TestContradictedClaimIsStale(t *testing.T) {
	llm := &fakeLLM{replies: []string{
		pricingClaim,
		`{"verdicts":[{"claim":1,"verdict":"contradicted","source":1,"quote":"Acme Pro is now $15 per month","current_value":"$15 per month"}]}`,
	}}
	r := newVerifier(llm, &fakeSearch{sources: acmeSource}).Verify(context.Background(), "How much is Acme Pro?", "Acme Pro costs $10 per month.")
	if !r.Stale() || len(r.Claims) != 1 {
		t.Fatalf("want stale with one claim, got %+v", r)
	}
	if r.JudgeTokens != 200 || !r.Claims[0].Grounded || r.Claims[0].ModelVerdict != "contradicted" {
		t.Errorf("tokens=%d grounded=%v verdict=%q", r.JudgeTokens, r.Claims[0].Grounded, r.Claims[0].ModelVerdict)
	}
	c := r.Claims[0]
	if c.CurrentValue != "$15 per month" || c.SourceURL != "https://acme.test/pricing" || c.Score > staleAt {
		t.Errorf("unexpected claim report: %+v", c)
	}
	if want := fixedNow.Add(RecheckWindow[KindPricing]); !r.RecheckAfter.Equal(want) {
		t.Errorf("recheck after = %v, want %v", r.RecheckAfter, want)
	}
}

func TestSupportedClaimIsFresh(t *testing.T) {
	llm := &fakeLLM{replies: []string{
		`{"claims":[{"text":"Acme Pro costs $15 per month","kind":"pricing","query":"Acme Pro price"}]}`,
		`{"verdicts":[{"claim":1,"verdict":"supported","source":1,"quote":"acme pro is now $15   per month"}]}`,
	}}
	r := newVerifier(llm, &fakeSearch{sources: acmeSource}).Verify(context.Background(), "", "Acme Pro costs $15 per month.")
	if r.Status != StatusFresh || r.Score < freshAt {
		t.Fatalf("want fresh, got %+v", r)
	}
}

func TestUngroundedVerdictNeverMarksStale(t *testing.T) {
	llm := &fakeLLM{replies: []string{
		pricingClaim,
		`{"verdicts":[{"claim":1,"verdict":"contradicted","source":1,"quote":"Acme Pro was discontinued last week","current_value":"discontinued"}]}`,
	}}
	r := newVerifier(llm, &fakeSearch{sources: acmeSource}).Verify(context.Background(), "", "Acme Pro costs $10 per month.")
	if r.Status != StatusUncertain || r.Claims[0].CurrentValue != "" || r.Claims[0].SourceURL != "" {
		t.Fatalf("a quote missing from the source must fall back to uncertain, got %+v", r)
	}
	if r.Claims[0].Grounded || r.Claims[0].ModelVerdict != "contradicted" {
		t.Errorf("the rejected model verdict should still be recorded: %+v", r.Claims[0])
	}
}

func TestVerdictWithBadSourceIndexIsUncertain(t *testing.T) {
	llm := &fakeLLM{replies: []string{
		pricingClaim,
		`{"verdicts":[{"claim":1,"verdict":"contradicted","source":7,"quote":"Acme Pro is now $15 per month"}]}`,
	}}
	r := newVerifier(llm, &fakeSearch{sources: acmeSource}).Verify(context.Background(), "", "Acme Pro costs $10 per month.")
	if r.Status != StatusUncertain {
		t.Fatalf("want uncertain, got %+v", r)
	}
}

func TestWeakEvidencePullsScoreTowardUncertain(t *testing.T) {
	weak := []Source{{URL: "https://blog.test", Content: acmeSource[0].Content, Relevance: 0.05}}
	vd := verdict{Verdict: "contradicted", Source: 1, Quote: "Acme Pro is now $15 per month"}
	strong := scoreClaim(Claim{Kind: KindPricing}, vd, acmeSource)
	soft := scoreClaim(Claim{Kind: KindPricing}, vd, weak)
	if !(strong.Score < soft.Score && soft.Status == StatusUncertain && strong.Status == StatusStale) {
		t.Fatalf("strong=%+v soft=%+v", strong, soft)
	}
}

func TestModelFailureIsUncheckedNotStale(t *testing.T) {
	r := newVerifier(&fakeLLM{err: errors.New("provider down")}, &fakeSearch{}).Verify(context.Background(), "", "Acme Pro costs $10 per month.")
	if r.Status != StatusUnchecked || r.Stale() || !strings.Contains(r.Error, "provider down") {
		t.Fatalf("want unchecked with error, got %+v", r)
	}
}

func TestMaxClaimsCapsSearches(t *testing.T) {
	llm := &fakeLLM{replies: []string{
		`{"claims":[{"text":"a costs $1 now","kind":"pricing","query":"a"},{"text":"b costs $2 now","kind":"pricing","query":"b"},{"text":"c costs $3 now","kind":"pricing","query":"c"}]}`,
		`{"verdicts":[]}`,
	}}
	search := &fakeSearch{}
	v := newVerifier(llm, search)
	v.MaxClaims = 2
	r := v.Verify(context.Background(), "", "Plan a costs $1, plan b costs $2 and plan c costs $3 per month.")
	if search.calls != 2 || len(r.Claims) != 2 {
		t.Fatalf("want 2 searches and 2 claims, got %d and %d", search.calls, len(r.Claims))
	}
}

func TestStripFence(t *testing.T) {
	fence := "```"
	inputs := []string{`{"a":1}`, fence + "json\n{\"a\":1}\n" + fence, fence + "\n{\"a\":1}" + fence}
	for _, in := range inputs {
		if got := stripFence(in); got != `{"a":1}` {
			t.Errorf("stripFence(%q) = %q", in, got)
		}
	}
}

func TestLedgerExpiresOnRecheckWindow(t *testing.T) {
	l := NewLedger(2)
	l.Put("a", Report{Status: StatusFresh, RecheckAfter: fixedNow.Add(time.Hour)})
	if _, ok := l.Current("a", fixedNow); !ok {
		t.Fatal("report should be current inside its window")
	}
	if _, ok := l.Current("a", fixedNow.Add(2*time.Hour)); ok {
		t.Fatal("report should be due after its window")
	}
	l.Put("t", Report{Status: StatusTimeless})
	if _, ok := l.Current("t", fixedNow.Add(1000*time.Hour)); !ok {
		t.Fatal("timeless reports never need a recheck")
	}
	l.Put("c", Report{Status: StatusFresh, RecheckAfter: fixedNow.Add(time.Hour)})
	if len(l.entries) > 2 {
		t.Fatalf("ledger exceeded its limit: %d", len(l.entries))
	}
}
