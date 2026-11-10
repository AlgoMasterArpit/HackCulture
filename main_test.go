package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/AlgoMasterArpit/HackCulture/freshness"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
)

type stubs struct {
	judgeCalls, searchCalls, evictions atomic.Int32
	evictedID                          atomic.Value
	server                             *httptest.Server
}

// One server plays the gateway's chat and cache-clear routes and Tavily's search route.
func startStubs(t *testing.T) *stubs {
	s := &stubs{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(freshness.InternalHeader) == "" {
			t.Error("judge call is missing the internal header")
		}
		content := `{\"claims\":[{\"text\":\"Acme Pro costs $10 per month\",\"kind\":\"pricing\",\"query\":\"Acme Pro price\"}]}`
		if s.judgeCalls.Add(1)%2 == 0 {
			content = `{\"verdicts\":[{\"claim\":1,\"verdict\":\"contradicted\",\"source\":1,\"quote\":\"Acme Pro is now $15 per month\",\"current_value\":\"$15 per month\"}]}`
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"` + content + `"}}]}`))
	})
	mux.HandleFunc("POST /search", func(w http.ResponseWriter, r *http.Request) {
		s.searchCalls.Add(1)
		w.Write([]byte(`{"results":[{"url":"https://acme.test/pricing","content":"Acme Pro is now $15 per month.","score":0.9}]}`))
	})
	mux.HandleFunc("DELETE /api/cache/clear/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.evictions.Add(1)
		s.evictedID.Store(r.PathValue("id"))
	})
	s.server = httptest.NewServer(mux)
	t.Cleanup(s.server.Close)

	if err := Init(map[string]any{
		"judge_model":    "test/judge",
		"gateway_url":    s.server.URL,
		"tavily_url":     s.server.URL,
		"tavily_api_key": "tvly-test",
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { Cleanup() })
	return s
}

func chatRequest(body string) *schemas.HTTPRequest {
	return &schemas.HTTPRequest{Method: "POST", Path: "/v1/chat/completions", Headers: map[string]string{}, Body: []byte(body)}
}

func chatResponse(content, cacheDebug string) *schemas.HTTPResponse {
	extra := ""
	if cacheDebug != "" {
		extra = `,"extra_fields":{"cache_debug":` + cacheDebug + `}`
	}
	return &schemas.HTTPResponse{
		StatusCode: 200,
		Headers:    map[string]string{},
		Body:       []byte(`{"choices":[{"message":{"role":"assistant","content":"` + content + `"}}]` + extra + `}`),
	}
}

const question = `{"model":"openai/gpt-4o-mini","messages":[{"role":"user","content":"How much is Acme Pro?"}]}`

func TestInitRejectsMissingConfig(t *testing.T) {
	if err := Init(map[string]any{"tavily_api_key": "k"}); err == nil {
		t.Error("missing judge_model should fail")
	}
	if err := Init(map[string]any{"judge_model": "m"}); err == nil {
		t.Error("missing search credentials should fail")
	}
}

func TestStaleCachedAnswerIsFlaggedAndEvicted(t *testing.T) {
	s := startStubs(t)
	resp := chatResponse("Acme Pro costs $10 per month.", `{"cache_hit":true,"cache_id":"abc-123"}`)

	if err := HTTPTransportPostHook(nil, chatRequest(question), resp); err != nil {
		t.Fatal(err)
	}
	report := gjson.GetBytes(resp.Body, "bestbefore")
	if report.Get("status").String() != "stale" || !report.Get("cache_evicted").Bool() {
		t.Fatalf("want stale and evicted, got %s", report.Raw)
	}
	if got := report.Get("claims.0.current_value").String(); got != "$15 per month" {
		t.Errorf("current_value = %q", got)
	}
	if resp.Headers["x-bestbefore-status"] != "stale" || resp.Headers["x-bestbefore-cache-evicted"] != "true" {
		t.Errorf("headers = %v", resp.Headers)
	}
	if s.evictions.Load() != 1 || s.evictedID.Load() != "abc-123" {
		t.Errorf("evictions = %d id = %v", s.evictions.Load(), s.evictedID.Load())
	}
	if gjson.GetBytes(resp.Body, "choices.0.message.content").String() == "" {
		t.Error("original answer was lost")
	}
}

func TestTimelessAnswerMakesNoCalls(t *testing.T) {
	s := startStubs(t)
	resp := chatResponse("A binary search halves the search space on every step.", "")

	HTTPTransportPostHook(nil, chatRequest(question), resp)

	if s.judgeCalls.Load() != 0 || s.searchCalls.Load() != 0 {
		t.Fatalf("timeless answer triggered %d judge and %d search calls", s.judgeCalls.Load(), s.searchCalls.Load())
	}
	if resp.Headers["x-bestbefore-status"] != "timeless" {
		t.Errorf("status header = %q", resp.Headers["x-bestbefore-status"])
	}
}

func TestCacheHitInsideWindowUsesLedger(t *testing.T) {
	s := startStubs(t)
	active.ledger.Put("warm", freshness.Report{Status: freshness.StatusTimeless, Score: 1})
	resp := chatResponse("Acme Pro costs $10 per month.", `{"cache_hit":true,"cache_id":"warm"}`)

	HTTPTransportPostHook(nil, chatRequest(question), resp)

	if s.judgeCalls.Load() != 0 {
		t.Fatal("a cache hit inside its recheck window must not be re-verified")
	}
	if gjson.GetBytes(resp.Body, "bestbefore.origin").String() != "ledger" {
		t.Errorf("origin = %s", gjson.GetBytes(resp.Body, "bestbefore.origin"))
	}
}

func TestSkipsStreamsErrorsAndOwnCalls(t *testing.T) {
	s := startStubs(t)
	answer := "Acme Pro costs $10 per month."

	internal := chatRequest(question)
	internal.Headers[freshness.InternalHeader] = "1"
	failed := chatResponse(answer, "")
	failed.StatusCode = 500

	cases := map[string]struct {
		req  *schemas.HTTPRequest
		resp *schemas.HTTPResponse
	}{
		"own judge call": {internal, chatResponse(answer, "")},
		"stream":         {chatRequest(`{"stream":true,"messages":[]}`), chatResponse(answer, "")},
		"error status":   {chatRequest(question), failed},
		"other route":    {&schemas.HTTPRequest{Method: "POST", Path: "/v1/embeddings", Headers: map[string]string{}}, chatResponse(answer, "")},
	}
	for name, c := range cases {
		HTTPTransportPostHook(nil, c.req, c.resp)
		if strings.Contains(string(c.resp.Body), "bestbefore") {
			t.Errorf("%s: response should be untouched", name)
		}
	}
	if s.judgeCalls.Load() != 0 {
		t.Errorf("skipped requests made %d judge calls", s.judgeCalls.Load())
	}
}

func TestVerifyModelChecksPastedAnswer(t *testing.T) {
	startStubs(t)
	req := chatRequest(`{"model":"bestbefore/verify","messages":[{"role":"user","content":"How much is Acme Pro?"},{"role":"assistant","content":"Acme Pro costs $10 per month."}]}`)

	resp, err := HTTPTransportPreHook(nil, req)
	if err != nil || resp == nil || resp.StatusCode != 200 {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	if gjson.GetBytes(resp.Body, "bestbefore.status").String() != "stale" {
		t.Errorf("report = %s", gjson.GetBytes(resp.Body, "bestbefore").Raw)
	}
	if text := gjson.GetBytes(resp.Body, "choices.0.message.content").String(); !strings.Contains(text, "$15 per month") {
		t.Errorf("summary = %q", text)
	}
}

func TestOrdinaryRequestsPassThroughPreHook(t *testing.T) {
	startStubs(t)
	if resp, err := HTTPTransportPreHook(nil, chatRequest(question)); resp != nil || err != nil {
		t.Fatalf("ordinary request was intercepted: %v %v", resp, err)
	}
}
