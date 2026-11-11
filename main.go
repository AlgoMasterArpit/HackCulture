// BestBefore is a Bifrost plugin that re-verifies time-sensitive claims in
// model answers against live web sources, scores their freshness, and keeps
// stale answers out of the semantic cache.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/AlgoMasterArpit/HackCulture/freshness"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	pluginName  = "bestbefore"
	verifyModel = "bestbefore/verify"

	evictRetryDelay = 2 * time.Second
)

type config struct {
	JudgeModel     string   `json:"judge_model"`
	JudgeFallbacks []string `json:"judge_fallbacks"`
	GatewayURL     string   `json:"gateway_url"`
	TavilyURL      string   `json:"tavily_url"`
	TavilyAPIKey   string   `json:"tavily_api_key"`
	VirtualKey     string   `json:"virtual_key"`
	MaxClaims      int      `json:"max_claims"`
	MaxResults     int      `json:"max_results"`
	TimeoutSeconds int      `json:"timeout_seconds"`
	EvictStale     *bool    `json:"evict_stale"`
}

type plugin struct {
	cfg      config
	verifier *freshness.Verifier
	ledger   *freshness.Ledger
	http     *http.Client
}

var active *plugin

func Init(raw any) error {
	p, err := newPlugin(raw)
	if err != nil {
		return fmt.Errorf("bestbefore: %w", err)
	}
	active = p
	return nil
}

func newPlugin(raw any) (*plugin, error) {
	cfg := config{
		GatewayURL:     "http://127.0.0.1:8080",
		TavilyURL:      "https://api.tavily.com",
		MaxClaims:      4,
		MaxResults:     5,
		TimeoutSeconds: 25,
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(encoded, &cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	cfg.TavilyAPIKey = resolveEnv(cfg.TavilyAPIKey)
	cfg.VirtualKey = resolveEnv(cfg.VirtualKey)

	if cfg.JudgeModel == "" {
		return nil, fmt.Errorf("judge_model is required, for example \"openai/gpt-4o-mini\"")
	}
	if cfg.TavilyAPIKey == "" && cfg.VirtualKey == "" {
		return nil, fmt.Errorf("set tavily_api_key, or virtual_key with tavily_url pointing at a Bifrost relay")
	}

	client := &http.Client{Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second}
	return &plugin{
		cfg:    cfg,
		http:   client,
		ledger: freshness.NewLedger(10000),
		verifier: &freshness.Verifier{
			LLM:       &freshness.GatewayLLM{BaseURL: cfg.GatewayURL, Model: cfg.JudgeModel, Fallbacks: cfg.JudgeFallbacks, VirtualKey: cfg.VirtualKey, HTTP: client},
			Search:    &freshness.Tavily{BaseURL: cfg.TavilyURL, APIKey: cfg.TavilyAPIKey, VirtualKey: cfg.VirtualKey, MaxResults: cfg.MaxResults, HTTP: client},
			MaxClaims: cfg.MaxClaims,
			Now:       time.Now,
		},
	}, nil
}

func resolveEnv(v string) string {
	if name, ok := strings.CutPrefix(v, "env."); ok {
		return os.Getenv(name)
	}
	return v
}

func GetName() string { return pluginName }

func Cleanup() error {
	active = nil
	return nil
}

// HTTPTransportPreHook serves the "bestbefore/verify" virtual model: any
// OpenAI-compatible client can hand over an existing answer and get its
// freshness report back, without a second service or a new route.
func HTTPTransportPreHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest) (*schemas.HTTPResponse, error) {
	p := active
	if p == nil || !isChatCompletion(req) || isInternal(req) {
		return nil, nil
	}
	if gjson.GetBytes(req.Body, "model").String() != verifyModel {
		return nil, nil
	}
	messages := gjson.GetBytes(req.Body, "messages").Array()
	if len(messages) == 0 {
		return jsonResponse(400, map[string]any{"error": map[string]string{
			"message": "bestbefore/verify needs the answer to check as the last message",
		}}), nil
	}
	answer := messageText(messages[len(messages)-1])
	question := ""
	if len(messages) > 1 {
		question = messageText(messages[len(messages)-2])
	}
	report := p.verify(question, answer)
	return jsonResponse(200, map[string]any{
		"id":      "bestbefore-" + strconv.FormatInt(report.CheckedAt.UnixNano(), 36),
		"object":  "chat.completion",
		"created": report.CheckedAt.Unix(),
		"model":   verifyModel,
		"choices": []map[string]any{{
			"index":         0,
			"finish_reason": "stop",
			"message":       map[string]string{"role": "assistant", "content": summarize(report)},
		}},
		"bestbefore": report,
	}), nil
}

type annotation struct {
	freshness.Report
	Origin       string `json:"origin"`
	CacheHit     bool   `json:"cache_hit"`
	CacheEvicted bool   `json:"cache_evicted"`
}

// HTTPTransportPostHook scores every non-streaming chat answer. It runs on
// cache hits too, which is where a TTL-only cache would keep serving an
// answer that has since gone stale.
func HTTPTransportPostHook(ctx *schemas.BifrostContext, req *schemas.HTTPRequest, resp *schemas.HTTPResponse) error {
	p := active
	if p == nil || !isChatCompletion(req) || isInternal(req) || resp.StatusCode != 200 || len(resp.Body) == 0 {
		return nil
	}
	if gjson.GetBytes(req.Body, "stream").Bool() || !gjson.ValidBytes(resp.Body) {
		return nil
	}
	answer := messageText(gjson.GetBytes(resp.Body, "choices.0.message"))
	if answer == "" {
		return nil
	}
	cacheID := gjson.GetBytes(resp.Body, "extra_fields.cache_debug.cache_id").String()
	note := annotation{CacheHit: gjson.GetBytes(resp.Body, "extra_fields.cache_debug.cache_hit").Bool()}

	if stored, ok := p.ledger.Current(cacheID, time.Now()); ok && note.CacheHit {
		note.Report, note.Origin = stored, "ledger"
		return annotate(resp, note)
	}

	note.Report, note.Origin = p.verify(lastUserMessage(req.Body), answer), "verified"
	if cacheID != "" {
		switch {
		case note.Stale() && p.evictsStale():
			note.CacheEvicted = p.evict(cacheID) == nil
			p.ledger.Delete(cacheID)
			// The cache writes its entry in a goroutine that can finish after a
			// fast verification, so clear it once more after that write lands.
			go func() {
				time.Sleep(evictRetryDelay)
				p.evict(cacheID)
			}()
		case note.Status != freshness.StatusUnchecked:
			p.ledger.Put(cacheID, note.Report)
		}
	}
	return annotate(resp, note)
}

func (p *plugin) evictsStale() bool {
	return p.cfg.EvictStale == nil || *p.cfg.EvictStale
}

func (p *plugin) verify(question, answer string) freshness.Report {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(p.cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	return p.verifier.Verify(ctx, question, answer)
}

func (p *plugin) evict(cacheID string) error {
	url := strings.TrimRight(p.cfg.GatewayURL, "/") + "/api/cache/clear/" + cacheID
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("cache clear returned %d", resp.StatusCode)
	}
	return nil
}

func annotate(resp *schemas.HTTPResponse, note annotation) error {
	body, err := sjson.SetBytes(resp.Body, "bestbefore", note)
	if err != nil {
		return nil
	}
	resp.Body = body
	if resp.Headers == nil {
		resp.Headers = map[string]string{}
	}
	resp.Headers["x-bestbefore-status"] = string(note.Status)
	resp.Headers["x-bestbefore-score"] = strconv.FormatFloat(note.Score, 'f', 2, 64)
	if note.CacheEvicted {
		resp.Headers["x-bestbefore-cache-evicted"] = "true"
	}
	return nil
}

func isChatCompletion(req *schemas.HTTPRequest) bool {
	return req.Method == http.MethodPost && strings.HasSuffix(req.Path, "/chat/completions")
}

func isInternal(req *schemas.HTTPRequest) bool {
	return req.CaseInsensitiveHeaderLookup(freshness.InternalHeader) != ""
}

func lastUserMessage(body []byte) string {
	users := gjson.GetBytes(body, `messages.#(role=="user")#`).Array()
	if len(users) == 0 {
		return ""
	}
	return messageText(users[len(users)-1])
}

// messageText reads both plain string content and multi-part content arrays.
func messageText(message gjson.Result) string {
	content := message.Get("content")
	if !content.IsArray() {
		return content.String()
	}
	var parts []string
	for _, part := range content.Array() {
		if text := part.Get("text").String(); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func summarize(r freshness.Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Freshness: %s (score %.2f)\n", r.Status, r.Score)
	if r.Error != "" {
		fmt.Fprintf(&b, "Could not verify: %s\n", r.Error)
	}
	for _, c := range r.Claims {
		fmt.Fprintf(&b, "\n[%s %.2f] %s", strings.ToUpper(string(c.Status)), c.Score, c.Claim)
		if c.CurrentValue != "" {
			fmt.Fprintf(&b, "\n  now: %s", c.CurrentValue)
		}
		if c.SourceURL != "" {
			fmt.Fprintf(&b, "\n  source: %s", c.SourceURL)
		}
	}
	if len(r.Claims) == 0 && r.Error == "" {
		b.WriteString("No time-sensitive claims found.")
	}
	return b.String()
}

func jsonResponse(status int, payload any) *schemas.HTTPResponse {
	body, _ := json.Marshal(payload)
	return &schemas.HTTPResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       body,
	}
}
