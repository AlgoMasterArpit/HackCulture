package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AlgoMasterArpit/HackCulture/freshness"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

// stubs plays the gateway's chat and cache-clear routes and Tavily's search
// route. The "web" says Acme Pro costs price; the judge compares the claim's
// price with it, so changing price makes a cached answer go stale.
type stubs struct {
	judgeCalls, healCalls, searchCalls, evictions atomic.Int32
	evictedID                                     atomic.Value
	price                                         atomic.Value
	brokenHeal                                    atomic.Bool
	server                                        *httptest.Server
}

var dollars = regexp.MustCompile(`\$\d+`)

func startStubs(t *testing.T, overrides map[string]any) *stubs {
	s := &stubs{}
	s.price.Store("$15")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if p := activePlugin(); p == nil || r.Header.Get(freshness.InternalHeader) != p.token {
			t.Error("judge call is missing the internal token")
		}
		var req struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		system, user := req.Messages[0].Content, req.Messages[1].Content
		price := s.price.Load().(string)
		var content string
		switch {
		case strings.HasPrefix(system, "You extract"):
			s.judgeCalls.Add(1)
			claimed := dollars.FindString(user[strings.Index(user, "Sentences"):])
			content = `{"claims":[{"text":"Acme Pro costs ` + claimed + ` per month","kind":"pricing","query":"Acme Pro price"}]}`
		case strings.HasPrefix(system, "You check"):
			s.judgeCalls.Add(1)
			claimed := dollars.FindString(user[strings.Index(user, "Claim 1:"):])
			if claimed == price {
				content = `{"verdicts":[{"claim":1,"verdict":"supported","source":1,"quote":"Acme Pro is now ` + price + ` per month"}]}`
			} else {
				content = `{"verdicts":[{"claim":1,"verdict":"contradicted","source":1,"quote":"Acme Pro is now ` + price + ` per month","current_value":"` + price + ` per month"}]}`
			}
		case strings.HasPrefix(system, "You correct"):
			s.healCalls.Add(1)
			content = `{"answer":"Acme Pro costs ` + price + ` per month [1]."}`
			if s.brokenHeal.Load() {
				content = `{"answer":"I am not sure."}`
			}
		}
		out, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": content}}}, "usage": map[string]int{"total_tokens": 10}})
		w.Write(out)
	})
	mux.HandleFunc("POST /search", func(w http.ResponseWriter, r *http.Request) {
		s.searchCalls.Add(1)
		w.Write([]byte(`{"results":[{"url":"https://acme.test/pricing","content":"Acme Pro is now ` + s.price.Load().(string) + ` per month.","score":0.9}]}`))
	})
	mux.HandleFunc("DELETE /api/cache/clear/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.evictions.Add(1)
		s.evictedID.Store(r.PathValue("id"))
	})
	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)

	cfg := map[string]any{
		"judge_model":    "test/judge",
		"gateway_url":    s.server.URL,
		"tavily_url":     s.server.URL,
		"tavily_api_key": "tvly-test",
		"sweep_interval": "0",
	}
	for k, v := range overrides {
		cfg[k] = v
	}
	if err := Init(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Cleanup() })
	return s
}

func chatRequest(body string) *schemas.HTTPRequest {
	return &schemas.HTTPRequest{Method: "POST", Path: "/v1/chat/completions", Headers: map[string]string{}, Body: []byte(body)}
}

func chatResponse(id, content, cacheDebug string) *schemas.HTTPResponse {
	extra := ""
	if cacheDebug != "" {
		extra = `,"extra_fields":{"cache_debug":` + cacheDebug + `}`
	}
	return &schemas.HTTPResponse{
		StatusCode: 200,
		Headers:    map[string]string{},
		Body:       []byte(`{"id":"` + id + `","choices":[{"message":{"role":"assistant","content":"` + content + `"}}]` + extra + `}`),
	}
}

const (
	question   = `{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"How much is Acme Pro?"}]}`
	staleText  = "Acme Pro costs $10 per month."
	answerText = "Acme Pro costs $15 per month."
)

func TestInitRejectsBadConfig(t *testing.T) {
	base := func(extra map[string]any) map[string]any {
		cfg := map[string]any{"judge_model": "m", "tavily_api_key": "k"}
		for k, v := range extra {
			cfg[k] = v
		}
		return cfg
	}
	bad := map[string]map[string]any{
		"missing judge_model":    {"tavily_api_key": "k"},
		"missing search key":     {"judge_model": "m"},
		"zero max_claims":        base(map[string]any{"max_claims": 0}),
		"negative timeout":       base(map[string]any{"timeout_seconds": -1}),
		"wrong type for a field": base(map[string]any{"max_claims": "four"}),
		"tiny sweep interval":    base(map[string]any{"sweep_interval": "10ms"}),
		"unknown claim kind":     base(map[string]any{"recheck_windows": map[string]string{"weather": "1h"}}),
		"bad recheck window":     base(map[string]any{"recheck_windows": map[string]string{"pricing": "soon"}}),
	}
	for name, cfg := range bad {
		if err := Init(cfg); err == nil {
			t.Errorf("%s: Init should fail", name)
		}
	}
	Cleanup()
}

// The core path: on a cache miss, PostLLMHook runs before the semantic cache
// stores the answer, so the cache keeps the corrected text.
func TestStaleAnswerIsHealedBeforeTheCacheStoresIt(t *testing.T) {
	s := startStubs(t, nil)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, map[string]string{"x-bf-cache-key": "app"})
	userText := "How much is Acme Pro?"
	PreLLMHook(ctx, &schemas.BifrostRequest{
		RequestType: schemas.ChatCompletionRequest,
		ChatRequest: &schemas.BifrostChatRequest{Input: []schemas.ChatMessage{{
			Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &userText},
		}}},
	})
	stale := staleText
	resp := &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{ID: "r1", Choices: []schemas.BifrostResponseChoice{{
		ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{Message: &schemas.ChatMessage{
			Role: schemas.ChatMessageRoleAssistant, Content: &schemas.ChatMessageContent{ContentStr: &stale},
		}},
	}}}}

	PostLLMHook(ctx, resp, nil)

	stored := contentText(firstMessage(resp.ChatResponse))
	if !strings.HasPrefix(stored, "Acme Pro costs $15 per month [1].") || !strings.Contains(stored, "https://acme.test/pricing") {
		t.Fatalf("the cache would store %q", stored)
	}
	if ctx.Value(cacheNoStoreKey) != nil {
		t.Error("a healed answer should be cached, not skipped")
	}

	calls := s.judgeCalls.Load()
	escaped, _ := json.Marshal(stored)
	annotated := chatResponse("r1", string(escaped[1:len(escaped)-1]), `{"cache_hit":false,"cache_id":"c1"}`)
	HTTPTransportPostHook(nil, chatRequest(question), annotated)
	note := gjson.GetBytes(annotated.Body, "bestbefore")
	if note.Get("status").String() != "healed" || note.Get("original_answer").String() != staleText || s.judgeCalls.Load() != calls {
		t.Fatalf("the HTTP hook should report the earlier check without a new one, got %s", note.Raw)
	}
	if annotated.Headers["x-bestbefore-healed"] != "true" || s.evictions.Load() != 0 {
		t.Errorf("headers=%v evictions=%d", annotated.Headers, s.evictions.Load())
	}
	if r, ok := activePlugin().ledger.Current("c1", time.Now()); !ok || r.Status != freshness.StatusHealed {
		t.Errorf("ledger should track the healed cache entry, got %+v %v", r, ok)
	}
}

func TestStaleAnswerThatCannotBeHealedIsNeverCached(t *testing.T) {
	s := startStubs(t, nil)
	s.brokenHeal.Store(true)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(questionKey, "How much is Acme Pro?")
	stale := staleText
	resp := &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{ID: "r2", Choices: []schemas.BifrostResponseChoice{{
		ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{Message: &schemas.ChatMessage{Content: &schemas.ChatMessageContent{ContentStr: &stale}}},
	}}}}

	PostLLMHook(ctx, resp, nil)

	if ctx.Value(cacheNoStoreKey) != true {
		t.Fatal("a stale, unhealed answer must not be written to the cache")
	}
	if contentText(firstMessage(resp.ChatResponse)) != staleText {
		t.Error("a failed rewrite must leave the answer as it was")
	}
}

func TestPluginSkipsItsOwnJudgeCallsInLLMHooks(t *testing.T) {
	s := startStubs(t, nil)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, map[string]string{freshness.InternalHeader: activePlugin().token})
	text := "Acme Pro price?"
	PreLLMHook(ctx, &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{
		Input: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &text}}},
	}})
	stale := staleText
	PostLLMHook(ctx, &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{ID: "r3", Choices: []schemas.BifrostResponseChoice{{
		ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{Message: &schemas.ChatMessage{Content: &schemas.ChatMessageContent{ContentStr: &stale}}},
	}}}}, nil)
	if s.judgeCalls.Load() != 0 {
		t.Fatal("the plugin must not check its own judge calls")
	}
}

func TestStaleCacheHitIsHealedForTheUserAndEvicted(t *testing.T) {
	s := startStubs(t, map[string]any{"revalidate": "inline"})
	resp := chatResponse("hit-1", staleText, `{"cache_hit":true,"cache_id":"abc-123"}`)

	HTTPTransportPostHook(nil, chatRequest(question), resp)

	note := gjson.GetBytes(resp.Body, "bestbefore")
	if note.Get("status").String() != "healed" || !note.Get("cache_evicted").Bool() || note.Get("claims.0.current_value").String() != "$15 per month" {
		t.Fatalf("want healed and evicted, got %s", note.Raw)
	}
	if got := gjson.GetBytes(resp.Body, "choices.0.message.content").String(); !strings.HasPrefix(got, "Acme Pro costs $15 per month") {
		t.Errorf("user got %q", got)
	}
	if s.evictedID.Load() != "abc-123" {
		t.Errorf("evicted id = %v", s.evictedID.Load())
	}
}

func TestWithHealingOffAStaleHitIsFlaggedAndEvicted(t *testing.T) {
	s := startStubs(t, map[string]any{"heal": false, "revalidate": "inline"})
	resp := chatResponse("hit-2", staleText, `{"cache_hit":true,"cache_id":"abc-123"}`)

	HTTPTransportPostHook(nil, chatRequest(question), resp)

	if gjson.GetBytes(resp.Body, "bestbefore.status").String() != "stale" || resp.Headers["x-bestbefore-cache-evicted"] != "true" {
		t.Fatalf("got %s", gjson.GetBytes(resp.Body, "bestbefore").Raw)
	}
	if s.healCalls.Load() != 0 || gjson.GetBytes(resp.Body, "choices.0.message.content").String() != staleText {
		t.Error("with heal off the answer must not be rewritten")
	}
}

func TestTimelessAnswerMakesNoCalls(t *testing.T) {
	s := startStubs(t, map[string]any{"check_scope": "all"})
	resp := chatResponse("t-1", "A binary search halves the search space on every step.", "")

	HTTPTransportPostHook(nil, chatRequest(question), resp)

	if s.judgeCalls.Load() != 0 || s.searchCalls.Load() != 0 {
		t.Fatalf("timeless answer triggered %d judge and %d search calls", s.judgeCalls.Load(), s.searchCalls.Load())
	}
	if resp.Headers["x-bestbefore-status"] != "timeless" {
		t.Errorf("status header = %q", resp.Headers["x-bestbefore-status"])
	}
}

func TestCacheHitInsideItsBestBeforeDateUsesLedger(t *testing.T) {
	s := startStubs(t, nil)
	activePlugin().ledger.Put("warm", freshness.Entry{Report: freshness.Report{Status: freshness.StatusFresh, Score: 0.9, RecheckAfter: time.Now().Add(time.Hour)}})
	resp := chatResponse("hit-3", staleText, `{"cache_hit":true,"cache_id":"warm"}`)

	HTTPTransportPostHook(nil, chatRequest(question), resp)

	if s.judgeCalls.Load() != 0 || gjson.GetBytes(resp.Body, "bestbefore.origin").String() != "ledger" {
		t.Fatalf("judge calls=%d report=%s", s.judgeCalls.Load(), gjson.GetBytes(resp.Body, "bestbefore").Raw)
	}
}

func TestCacheHitPastItsBestBeforeDateIsCheckedAgainInline(t *testing.T) {
	s := startStubs(t, map[string]any{"revalidate": "inline"})
	activePlugin().ledger.Put("due", freshness.Entry{Report: freshness.Report{Status: freshness.StatusFresh, Score: 0.9, RecheckAfter: time.Now().Add(-time.Minute)}})
	resp := chatResponse("hit-4", staleText, `{"cache_hit":true,"cache_id":"due"}`)

	HTTPTransportPostHook(nil, chatRequest(question), resp)

	if s.judgeCalls.Load() == 0 || gjson.GetBytes(resp.Body, "bestbefore.status").String() != "healed" {
		t.Fatalf("a due cache hit must be checked again, got %s", gjson.GetBytes(resp.Body, "bestbefore").Raw)
	}
}

// The sweeper finds answers that went stale while sitting in the cache,
// before any user asks again.
func TestSweeperEvictsAnswersThatWentStaleInTheCache(t *testing.T) {
	s := startStubs(t, nil)
	past := time.Now().Add(-time.Minute)
	activePlugin().ledger.Put("acme", freshness.Entry{Question: "How much is Acme Pro?", Answer: answerText,
		Report: freshness.Report{Status: freshness.StatusFresh, RecheckAfter: past}})
	activePlugin().ledger.Put("later", freshness.Entry{Question: "q", Answer: answerText,
		Report: freshness.Report{Status: freshness.StatusFresh, RecheckAfter: time.Now().Add(time.Hour)}})

	s.price.Store("$20")
	results, _ := activePlugin().sweep(false)

	if len(results) != 1 || results[0].CacheID != "acme" || !results[0].Evicted || s.evictedID.Load() != "acme" {
		t.Fatalf("sweep results = %+v", results)
	}
	if _, ok := activePlugin().ledger.Current("acme", time.Now()); ok {
		t.Error("an evicted entry must leave the ledger")
	}
	if _, ok := activePlugin().ledger.Current("later", time.Now()); !ok {
		t.Error("an entry inside its best-before date must not be touched")
	}
	if activePlugin().stats.sweptStale.Load() != 1 || s.healCalls.Load() != 0 {
		t.Errorf("evicted_by_sweeper=%d heal calls=%d", activePlugin().stats.sweptStale.Load(), s.healCalls.Load())
	}
}

func TestSweeperRenewsAnswersThatAreStillTrue(t *testing.T) {
	s := startStubs(t, nil)
	activePlugin().ledger.Put("acme", freshness.Entry{Question: "How much is Acme Pro?", Answer: answerText,
		Report: freshness.Report{Status: freshness.StatusFresh, RecheckAfter: time.Now().Add(-time.Minute)}})

	activePlugin().sweep(false)

	r, ok := activePlugin().ledger.Current("acme", time.Now())
	if !ok || r.Status != freshness.StatusFresh || s.evictions.Load() != 0 {
		t.Fatalf("a still-true answer should get a new best-before date, got %+v %v", r, ok)
	}
}

func TestStatusAndSweepModels(t *testing.T) {
	startStubs(t, map[string]any{"admin_models": true})
	activePlugin().ledger.Put("acme", freshness.Entry{Question: "How much is Acme Pro?", Answer: answerText,
		Report: freshness.Report{Status: freshness.StatusFresh, RecheckAfter: time.Now().Add(time.Hour)}})

	status, _ := HTTPTransportPreHook(nil, chatRequest(`{"model":"bestbefore/status","messages":[]}`))
	if got := gjson.GetBytes(status.Body, "bestbefore_status.entries.0.question").String(); got != "How much is Acme Pro?" {
		t.Fatalf("status = %s", status.Body)
	}

	sweep, _ := HTTPTransportPreHook(nil, chatRequest(`{"model":"bestbefore/sweep","messages":[]}`))
	if gjson.GetBytes(sweep.Body, "bestbefore_status.swept.#").Int() != 1 || gjson.GetBytes(sweep.Body, "bestbefore_status.counts.sweeps").Int() != 1 {
		t.Fatalf("a forced sweep re-checks every entry, got %s", sweep.Body)
	}
}

func TestCacheIDIsEscapedInEvictionURL(t *testing.T) {
	s := startStubs(t, map[string]any{"heal": false, "revalidate": "inline"})
	resp := chatResponse("e-1", staleText, `{"cache_hit":true,"cache_id":"../../v1/x?y=1"}`)

	HTTPTransportPostHook(nil, chatRequest(question), resp)

	if s.evictedID.Load() != "../../v1/x?y=1" {
		t.Errorf("evicted id = %v, want the raw id delivered to the clear route", s.evictedID.Load())
	}
}

func TestForgedInternalHeaderIsStillChecked(t *testing.T) {
	s := startStubs(t, map[string]any{"check_scope": "all"})
	req := chatRequest(question)
	req.Headers[freshness.InternalHeader] = "1"

	HTTPTransportPostHook(nil, req, chatResponse("f-1", staleText, ""))

	if s.judgeCalls.Load() == 0 {
		t.Fatal("a client-supplied internal header must not skip checking")
	}
}

func TestSkipsStreamsErrorsAndOwnCalls(t *testing.T) {
	s := startStubs(t, nil)
	internal := chatRequest(question)
	internal.Headers[freshness.InternalHeader] = activePlugin().token
	failed := chatResponse("x", staleText, "")
	failed.StatusCode = 500

	cases := map[string]struct {
		req  *schemas.HTTPRequest
		resp *schemas.HTTPResponse
	}{
		"own judge call": {internal, chatResponse("x", staleText, "")},
		"stream":         {chatRequest(`{"stream":true,"messages":[]}`), chatResponse("x", staleText, "")},
		"error status":   {chatRequest(question), failed},
		"other route":    {&schemas.HTTPRequest{Method: "POST", Path: "/v1/embeddings", Headers: map[string]string{}}, chatResponse("x", staleText, "")},
		"verify model":   {chatRequest(`{"model":"bestbefore/verify","messages":[]}`), chatResponse("x", staleText, "")},
		"status model":   {chatRequest(`{"model":"bestbefore/status","messages":[]}`), chatResponse("x", staleText, "")},
		"nil body":       {chatRequest(question), &schemas.HTTPResponse{StatusCode: 200, Headers: map[string]string{}}},
	}
	for name, c := range cases {
		HTTPTransportPostHook(nil, c.req, c.resp)
		if strings.Contains(string(c.resp.Body), "bestbefore\":") {
			t.Errorf("%s: response should be untouched", name)
		}
	}
	if s.judgeCalls.Load() != 0 {
		t.Errorf("skipped requests made %d judge calls", s.judgeCalls.Load())
	}
}

func TestVerifyModelHealsAPastedAnswer(t *testing.T) {
	startStubs(t, nil)
	req := chatRequest(`{"model":"bestbefore/verify","messages":[{"role":"user","content":"How much is Acme Pro?"},{"role":"assistant","content":"` + staleText + `"}]}`)

	resp, err := HTTPTransportPreHook(nil, req)
	if err != nil || resp == nil || resp.StatusCode != 200 {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	if gjson.GetBytes(resp.Body, "bestbefore.status").String() != "healed" {
		t.Errorf("report = %s", gjson.GetBytes(resp.Body, "bestbefore").Raw)
	}
	if text := gjson.GetBytes(resp.Body, "choices.0.message.content").String(); !strings.Contains(text, "Corrected answer:\nAcme Pro costs $15 per month") {
		t.Errorf("summary = %q", text)
	}
}

func TestOrdinaryRequestsPassThroughPreHook(t *testing.T) {
	startStubs(t, nil)
	if resp, err := HTTPTransportPreHook(nil, chatRequest(question)); resp != nil || err != nil {
		t.Fatalf("ordinary request was intercepted: %v %v", resp, err)
	}
}

func TestRecheckWindowsBelongToOnePluginInstance(t *testing.T) {
	startStubs(t, map[string]any{"recheck_windows": map[string]string{"pricing": "6h"}})
	if activePlugin().verifier.Windows[freshness.KindPricing] != 6*time.Hour {
		t.Fatal("the override should apply to this plugin")
	}
	if freshness.RecheckWindow[freshness.KindPricing] != 24*time.Hour {
		t.Fatal("the package defaults must never be modified")
	}
	if err := Init(map[string]any{"judge_model": "m", "tavily_api_key": "k", "sweep_interval": "0",
		"recheck_windows": map[string]string{"pricing": "1h", "weather": "1h"}}); err == nil {
		t.Fatal("an unknown kind should fail Init")
	}
	if activePlugin().verifier.Windows[freshness.KindPricing] != 6*time.Hour {
		t.Fatal("a failed Init must leave the running plugin untouched")
	}
}

func TestAdminModelsAreOffByDefault(t *testing.T) {
	startStubs(t, nil)
	for _, model := range []string{"bestbefore/status", "bestbefore/sweep"} {
		resp, _ := HTTPTransportPreHook(nil, chatRequest(`{"model":"`+model+`","messages":[]}`))
		if resp == nil || resp.StatusCode != 403 {
			t.Errorf("%s should be refused unless admin_models is set", model)
		}
	}
}

func TestOnlyOneSweepRunsAtATime(t *testing.T) {
	startStubs(t, map[string]any{"admin_models": true})
	p := activePlugin()
	p.sweeping.Lock()
	defer p.sweeping.Unlock()
	if _, ran := p.sweep(true); ran {
		t.Fatal("a second sweep must not start while one is running")
	}
	resp, _ := HTTPTransportPreHook(nil, chatRequest(`{"model":"bestbefore/sweep","messages":[]}`))
	if !gjson.GetBytes(resp.Body, "bestbefore_status.busy").Bool() {
		t.Fatalf("got %s", resp.Body)
	}
}

func TestSweeperBacksOffAndStopsVouchingWhenACheckFails(t *testing.T) {
	s := startStubs(t, nil)
	s.server.Close()
	p := activePlugin()
	p.ledger.Put("acme", freshness.Entry{Question: "How much is Acme Pro?", Answer: answerText,
		Report: freshness.Report{Status: freshness.StatusFresh, CheckedAt: time.Now().Add(-time.Hour), RecheckAfter: time.Now().Add(-time.Minute)}})

	p.sweep(false)

	if _, ok := p.ledger.Current("acme", time.Now()); !ok {
		t.Fatal("a failed re-check should keep the entry, with a later date")
	}
	r, _ := p.ledger.Current("acme", time.Now())
	if r.Status != freshness.StatusUnchecked || time.Until(r.RecheckAfter) < time.Minute {
		t.Fatalf("want unchecked with backoff, got %+v", r)
	}
}

func TestResultsWithTheSameResponseIDDoNotCross(t *testing.T) {
	startStubs(t, nil)
	p := activePlugin()
	p.pending.put(pendingKey("dup", "answer A"), "question A", outcome{Origin: "A"})
	p.pending.put(pendingKey("dup", "answer B"), "question B", outcome{Origin: "B"})

	if got, ok := p.takePending(true, "dup", "answer A"); ok {
		t.Fatalf("a cache hit must not consume a pending result, got %+v", got)
	}
	a, okA := p.takePending(false, "dup", "answer A")
	b, okB := p.takePending(false, "dup", "answer B")
	if !okA || !okB || a.Origin != "A" || b.Origin != "B" {
		t.Fatalf("a=%+v b=%+v", a, b)
	}
}

func TestAnswersNotHeadedForTheCachePassUntouched(t *testing.T) {
	s := startStubs(t, nil)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, map[string]string{})
	text := "How much is Acme Pro?"
	PreLLMHook(ctx, &schemas.BifrostRequest{RequestType: schemas.ChatCompletionRequest, ChatRequest: &schemas.BifrostChatRequest{
		Input: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &text}}},
	}})
	if ctx.Value(questionKey) != nil {
		t.Fatal("an uncached request must not be queued for a check")
	}
	resp := chatResponse("u-1", staleText, "")
	HTTPTransportPostHook(nil, chatRequest(question), resp)
	if strings.Contains(string(resp.Body), "bestbefore") || s.judgeCalls.Load() != 0 {
		t.Fatal("an uncached answer should pass with no check and no delay")
	}
}

func TestExpiredHitIsServedNowAndRevalidatedInTheBackground(t *testing.T) {
	s := startStubs(t, nil)
	p := activePlugin()
	p.ledger.Put("old", freshness.Entry{Question: "How much is Acme Pro?", Answer: staleText,
		Report: freshness.Report{Status: freshness.StatusFresh, Score: 0.9, CheckedAt: time.Now().Add(-48 * time.Hour), RecheckAfter: time.Now().Add(-time.Hour)}})
	resp := chatResponse("hit-5", staleText, `{"cache_hit":true,"cache_id":"old"}`)

	HTTPTransportPostHook(nil, chatRequest(question), resp)

	if got := gjson.GetBytes(resp.Body, "bestbefore.origin").String(); got != "revalidating" {
		t.Fatalf("origin = %q", got)
	}
	if gjson.GetBytes(resp.Body, "choices.0.message.content").String() != staleText {
		t.Error("the hit should be served as it is, without waiting for the check")
	}
	deadline := time.Now().Add(5 * time.Second)
	for s.evictedID.Load() != "old" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if s.evictedID.Load() != "old" {
		t.Fatal("the background re-check should evict the entry that went stale")
	}
	if _, ok := p.ledger.Get("old"); ok {
		t.Error("the evicted entry should leave the ledger")
	}
}

func TestABurstOfExpiredHitsPaysForOneCheck(t *testing.T) {
	startStubs(t, nil)
	p := activePlugin()
	if !p.inflight.claim("busy") {
		t.Fatal("first claim should succeed")
	}
	defer p.inflight.release("busy")
	if _, ran := p.recheck("busy", freshness.Entry{Answer: staleText}, false); ran {
		t.Fatal("a second re-check of the same entry must not start while one is running")
	}
}

func TestStreamedStaleAnswerGetsACorrectionAndIsNotCached(t *testing.T) {
	startStubs(t, nil)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, map[string]string{"x-bf-cache-key": "app"})
	text := "How much is Acme Pro?"
	PreLLMHook(ctx, &schemas.BifrostRequest{RequestType: schemas.ChatCompletionStreamRequest, ChatRequest: &schemas.BifrostChatRequest{
		Input: []schemas.ChatMessage{{Role: schemas.ChatMessageRoleUser, Content: &schemas.ChatMessageContent{ContentStr: &text}}},
	}})
	chunk := func(content string) *schemas.BifrostResponse {
		return &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{ID: "s1", Choices: []schemas.BifrostResponseChoice{{
			ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{Delta: &schemas.ChatStreamResponseChoiceDelta{Content: &content}},
		}}}}
	}

	PostLLMHook(ctx, chunk("Acme Pro costs $10"), nil)
	PostLLMHook(ctx, chunk(" per month."), nil)
	if ctx.Value(cacheNoStoreKey) != nil {
		t.Fatal("nothing should be decided before the final chunk")
	}
	ctx.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
	final := chunk("")
	PostLLMHook(ctx, final, nil)

	delta := *final.ChatResponse.Choices[0].Delta.Content
	if !strings.Contains(delta, "Corrected by BestBefore: Acme Pro costs $15 per month") {
		t.Fatalf("final delta = %q", delta)
	}
	if ctx.Value(cacheNoStoreKey) != true {
		t.Error("a stale stream must be kept out of the cache")
	}
	report, ok := final.ChatResponse.ExtraFields.RawResponse.(map[string]any)
	if !ok || report["bestbefore"].(outcome).Status != freshness.StatusHealed {
		t.Errorf("the final chunk should carry the report, got %#v", final.ChatResponse.ExtraFields.RawResponse)
	}
}

func TestInitRejectsUnknownModes(t *testing.T) {
	for _, cfg := range []map[string]any{
		{"judge_model": "m", "tavily_api_key": "k", "check_scope": "some"},
		{"judge_model": "m", "tavily_api_key": "k", "revalidate": "later"},
	} {
		if err := Init(cfg); err == nil {
			t.Errorf("Init(%v) should fail", cfg)
		}
	}
	Cleanup()
}

func TestCachedAnswersExpireAtTheirBestBeforeDate(t *testing.T) {
	startStubs(t, nil)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(schemas.BifrostContextKeyRequestHeaders, map[string]string{"x-bf-cache-key": "app"})
	ctx.SetValue(questionKey, "How much is Acme Pro?")
	fresh := answerText
	PostLLMHook(ctx, &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{ID: "ttl", Choices: []schemas.BifrostResponseChoice{{
		ChatNonStreamResponseChoice: &schemas.ChatNonStreamResponseChoice{Message: &schemas.ChatMessage{Content: &schemas.ChatMessageContent{ContentStr: &fresh}}},
	}}}}, nil)

	ttl, ok := ctx.Value(cacheTTLKey).(time.Duration)
	want := freshness.RecheckWindow[freshness.KindPricing]
	if !ok || ttl <= want-time.Minute || ttl > want {
		t.Fatalf("the cache entry should expire at the pricing best-before date, got %v", ctx.Value(cacheTTLKey))
	}
}

func TestFreshStreamIsCachedOnlyUntilItsBestBeforeDate(t *testing.T) {
	startStubs(t, nil)
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(questionKey, "How much is Acme Pro?")
	ctx.SetValue(streamKey, &streamText{})
	ctx.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
	content := answerText
	PostLLMHook(ctx, &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{ID: "fs", Choices: []schemas.BifrostResponseChoice{{
		ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{Delta: &schemas.ChatStreamResponseChoiceDelta{Content: &content}},
	}}}}, nil)

	if ctx.Value(cacheNoStoreKey) != nil {
		t.Fatal("a fresh stream may be cached")
	}
	if _, ok := ctx.Value(cacheTTLKey).(time.Duration); !ok {
		t.Fatal("a cached stream must expire at its best-before date, since the ledger never sees it")
	}
}

func TestStreamThatCannotBeCheckedIsNotCached(t *testing.T) {
	s := startStubs(t, nil)
	s.server.Close()
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	ctx.SetValue(questionKey, "How much is Acme Pro?")
	ctx.SetValue(streamKey, &streamText{})
	ctx.SetValue(schemas.BifrostContextKeyStreamEndIndicator, true)
	content := answerText
	PostLLMHook(ctx, &schemas.BifrostResponse{ChatResponse: &schemas.BifrostChatResponse{ID: "us", Choices: []schemas.BifrostResponseChoice{{
		ChatStreamResponseChoice: &schemas.ChatStreamResponseChoice{Delta: &schemas.ChatStreamResponseChoiceDelta{Content: &content}},
	}}}}, nil)
	if ctx.Value(cacheNoStoreKey) != true {
		t.Fatal("an unchecked stream must not be cached")
	}
}

func TestHealedAnswerIsReportedEvenWhenTheCacheSkippedIt(t *testing.T) {
	startStubs(t, nil)
	p := activePlugin()
	healed := "Acme Pro costs $15 per month [1]."
	p.pending.put(pendingKey("nc", healed), "How much is Acme Pro?", outcome{Report: freshness.Report{Status: freshness.StatusHealed}, Healed: true})
	resp := chatResponse("nc", healed, "")

	HTTPTransportPostHook(nil, chatRequest(question), resp)

	if gjson.GetBytes(resp.Body, "bestbefore.status").String() != "healed" || resp.Headers["x-bestbefore-healed"] != "true" {
		t.Fatalf("a rewritten answer must always carry its report, got %s %v", resp.Body, resp.Headers)
	}
}

func TestRecheckSkipsWhenTheLedgerHasANewerAnswer(t *testing.T) {
	s := startStubs(t, nil)
	p := activePlugin()
	p.ledger.Put("x", freshness.Entry{Answer: answerText, Report: freshness.Report{Status: freshness.StatusHealed, CheckedAt: time.Now()}})

	if _, ran := p.recheck("x", freshness.Entry{Question: "q", Answer: staleText}, false); ran {
		t.Fatal("a re-check from an older snapshot must not run")
	}
	if s.judgeCalls.Load() != 0 || s.evictions.Load() != 0 {
		t.Fatal("nothing should be checked or evicted")
	}
}

func TestFailedRecheckOfAnUnknownEntryIsRemembered(t *testing.T) {
	s := startStubs(t, nil)
	s.server.Close()
	p := activePlugin()

	p.recheck("fresh-start", freshness.Entry{Question: "How much is Acme Pro?", Answer: staleText}, false)

	e, ok := p.ledger.Get("fresh-start")
	if !ok || e.Report.Status != freshness.StatusUnchecked || time.Until(e.Report.RecheckAfter) < time.Minute {
		t.Fatalf("a failed check should leave a backoff entry, got %+v %v", e, ok)
	}
	if _, ran := p.recheck("fresh-start", freshness.Entry{Question: "q", Answer: staleText}, false); ran {
		t.Fatal("a later hit must not pay for another check while backing off")
	}
}
