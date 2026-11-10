package freshness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
)

type Status string

const (
	StatusFresh     Status = "fresh"
	StatusStale     Status = "stale"
	StatusUncertain Status = "uncertain"
	StatusTimeless  Status = "timeless"
	StatusUnchecked Status = "unchecked"
)

type Claim struct {
	Text  string `json:"text"`
	Kind  Kind   `json:"kind"`
	Query string `json:"query"`
}

type ClaimReport struct {
	Claim        string  `json:"claim"`
	Kind         Kind    `json:"kind"`
	Status       Status  `json:"status"`
	Score        float64 `json:"freshness_score"`
	CurrentValue string  `json:"current_value,omitempty"`
	SourceURL    string  `json:"source_url,omitempty"`
	SourceDate   string  `json:"source_date,omitempty"`
	Quote        string  `json:"quote,omitempty"`
}

type Report struct {
	Status       Status        `json:"status"`
	Score        float64       `json:"freshness_score"`
	Claims       []ClaimReport `json:"claims"`
	CheckedAt    time.Time     `json:"checked_at"`
	RecheckAfter time.Time     `json:"recheck_after"`
	Searches     int           `json:"searches"`
	LatencyMs    int64         `json:"latency_ms"`
	Error        string        `json:"error,omitempty"`
}

func (r Report) Stale() bool { return r.Status == StatusStale }

type Verifier struct {
	LLM       LLM
	Search    Searcher
	MaxClaims int
	Now       func() time.Time
}

func (v *Verifier) Verify(ctx context.Context, question, answer string) Report {
	start := v.Now()
	report := Report{Status: StatusTimeless, Score: 1, Claims: []ClaimReport{}, CheckedAt: start}

	candidates := Detect(answer)
	if len(candidates) == 0 {
		return report
	}
	fail := func(err error) Report {
		report.Status, report.Score, report.Error = StatusUnchecked, 0.5, err.Error()
		report.LatencyMs = v.Now().Sub(start).Milliseconds()
		return report
	}

	claims, err := v.extract(ctx, question, candidates)
	if err != nil {
		return fail(fmt.Errorf("claim extraction: %w", err))
	}
	if len(claims) == 0 {
		report.LatencyMs = v.Now().Sub(start).Milliseconds()
		return report
	}

	evidence := make([][]Source, len(claims))
	var wg sync.WaitGroup
	for i, c := range claims {
		wg.Add(1)
		go func() {
			defer wg.Done()
			evidence[i], _ = v.Search.Search(ctx, c.Query)
		}()
	}
	wg.Wait()
	report.Searches = len(claims)

	verdicts, err := v.judge(ctx, claims, evidence, start)
	if err != nil {
		return fail(fmt.Errorf("judging: %w", err))
	}

	report.Score = 1
	window := time.Duration(0)
	for i, c := range claims {
		cr := scoreClaim(c, verdicts[i], evidence[i])
		report.Claims = append(report.Claims, cr)
		report.Score = min(report.Score, cr.Score)
		if w := RecheckWindow[c.Kind]; window == 0 || w < window {
			window = w
		}
	}
	report.Status = statusFor(report.Score)
	report.RecheckAfter = start.Add(window)
	report.LatencyMs = v.Now().Sub(start).Milliseconds()
	return report
}

const extractSystem = `You extract time-sensitive factual claims from an AI answer.
A claim is time-sensitive when its truth can change: prices, plans, versions, deprecations, laws and rates, leadership, ownership, company figures, "latest" or "current" statements.
Rules:
- Each claim must be atomic and self-contained: name the product, company or law explicitly, never "it" or "they".
- Skip opinions, definitions, maths and history that cannot change.
- "query" is a short web search that would reveal the current value.
- "kind" is one of: pricing, version, regulation, leadership, recency.
Return JSON: {"claims":[{"text":"...","kind":"...","query":"..."}]}. Return {"claims":[]} if nothing qualifies.`

func (v *Verifier) extract(ctx context.Context, question string, candidates []Candidate) ([]Claim, error) {
	var b strings.Builder
	if question != "" {
		fmt.Fprintf(&b, "User question (context only):\n%s\n\n", question)
	}
	b.WriteString("Sentences flagged as possibly time-sensitive:\n")
	for i, c := range candidates {
		fmt.Fprintf(&b, "%d. %s\n", i+1, c.Sentence)
	}
	raw, err := v.LLM.CompleteJSON(ctx, extractSystem, b.String())
	if err != nil {
		return nil, err
	}
	var out struct {
		Claims []Claim `json:"claims"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("bad extraction JSON: %w", err)
	}
	claims := out.Claims[:0]
	for _, c := range out.Claims {
		if strings.TrimSpace(c.Text) == "" || strings.TrimSpace(c.Query) == "" {
			continue
		}
		if !validKind(c.Kind) {
			c.Kind = KindRecency
		}
		claims = append(claims, c)
		if len(claims) == v.MaxClaims {
			break
		}
	}
	return claims, nil
}

type verdict struct {
	Claim        int    `json:"claim"`
	Verdict      string `json:"verdict"`
	CurrentValue string `json:"current_value"`
	Source       int    `json:"source"`
	Quote        string `json:"quote"`
}

const judgeSystem = `You check whether claims are still true, using only the numbered web sources given for each claim.
For each claim return:
- "verdict": "supported" if a source confirms it is true today, "contradicted" if a source shows a different current value, "unverifiable" if the sources do not settle it.
- "source": the number of the single source you relied on.
- "quote": an exact, verbatim span copied from that source's text that proves the verdict. Never paraphrase.
- "current_value": for contradicted claims, the correct current value in a few words.
Prefer the most recent source. Do not use your own knowledge; if the sources are silent, answer "unverifiable".
Return JSON: {"verdicts":[{"claim":1,"verdict":"...","source":1,"quote":"...","current_value":"..."}]}`

func (v *Verifier) judge(ctx context.Context, claims []Claim, evidence [][]Source, now time.Time) ([]verdict, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Today's date: %s\n", now.Format("2 January 2006"))
	for i, c := range claims {
		fmt.Fprintf(&b, "\nClaim %d: %s\n", i+1, c.Text)
		if len(evidence[i]) == 0 {
			b.WriteString("  (no sources found)\n")
		}
		for j, s := range evidence[i] {
			fmt.Fprintf(&b, "  Source %d [%s] %s %s\n  %s\n", j+1, s.URL, s.Title, s.PublishedDate, s.Content)
		}
	}
	raw, err := v.LLM.CompleteJSON(ctx, judgeSystem, b.String())
	if err != nil {
		return nil, err
	}
	var out struct {
		Verdicts []verdict `json:"verdicts"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("bad verdict JSON: %w", err)
	}
	byClaim := make([]verdict, len(claims))
	for i := range byClaim {
		byClaim[i].Verdict = "unverifiable"
	}
	for _, vd := range out.Verdicts {
		if vd.Claim >= 1 && vd.Claim <= len(claims) {
			byClaim[vd.Claim-1] = vd
		}
	}
	return byClaim, nil
}

const (
	scoreSupported    = 0.9
	scoreUnverifiable = 0.5
	scoreContradicted = 0.1
	freshAt           = 0.7
	staleAt           = 0.3
)

// scoreClaim is the only place a claim's status is decided, and it is
// deterministic: the model's verdict counts only if its quote is found
// verbatim in the source it cites. Anything else falls back to uncertain, so
// a hallucinated verdict can never mark an answer stale.
func scoreClaim(c Claim, vd verdict, sources []Source) ClaimReport {
	cr := ClaimReport{Claim: c.Text, Kind: c.Kind, Score: scoreUnverifiable}

	base := scoreUnverifiable
	switch vd.Verdict {
	case "supported":
		base = scoreSupported
	case "contradicted":
		base = scoreContradicted
	}
	if base != scoreUnverifiable && vd.Source >= 1 && vd.Source <= len(sources) {
		src := sources[vd.Source-1]
		if grounded(vd.Quote, src.Content) {
			weight := min(1, 0.25+src.Relevance)
			cr.Score = scoreUnverifiable + (base-scoreUnverifiable)*weight
			cr.SourceURL, cr.SourceDate, cr.Quote = src.URL, src.PublishedDate, vd.Quote
			if vd.Verdict == "contradicted" {
				cr.CurrentValue = vd.CurrentValue
			}
		}
	}
	cr.Status = statusFor(cr.Score)
	return cr
}

func statusFor(score float64) Status {
	switch {
	case score >= freshAt:
		return StatusFresh
	case score <= staleAt:
		return StatusStale
	}
	return StatusUncertain
}

func grounded(quote, content string) bool {
	q := normalize(quote)
	return len(q) >= 8 && strings.Contains(normalize(content), q)
}

func normalize(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}
