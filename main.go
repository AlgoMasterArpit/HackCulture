// BestBefore is a Bifrost plugin that keeps a semantic cache honest. It
// re-verifies the time-sensitive claims in model answers against live web
// sources, rewrites stale answers with the verified current values before
// the cache stores them, and re-checks cached answers in the background when
// their best-before date passes.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/AlgoMasterArpit/HackCulture/freshness"
)

const (
	pluginName = "bestbefore"

	evictRetryDelay = 2 * time.Second
)

type config struct {
	JudgeModel     string            `json:"judge_model"`
	JudgeFallbacks []string          `json:"judge_fallbacks"`
	GatewayURL     string            `json:"gateway_url"`
	TavilyURL      string            `json:"tavily_url"`
	TavilyAPIKey   string            `json:"tavily_api_key"`
	VirtualKey     string            `json:"virtual_key"`
	MaxClaims      int               `json:"max_claims"`
	MaxResults     int               `json:"max_results"`
	TimeoutSeconds int               `json:"timeout_seconds"`
	EvictStale     *bool             `json:"evict_stale"`
	Heal           *bool             `json:"heal"`
	SweepInterval  string            `json:"sweep_interval"`
	RecheckWindows map[string]string `json:"recheck_windows"`
}

type plugin struct {
	cfg      config
	verifier *freshness.Verifier
	healer   *freshness.Healer
	ledger   *freshness.Ledger
	pending  *pendingChecks
	stats    *stats
	http     *http.Client
	// token marks the plugin's own judge calls. It is random per process so a
	// client cannot send the header itself to skip checking.
	token string
	stop  context.CancelFunc
}

var active *plugin

func Init(raw any) error {
	p, err := newPlugin(raw)
	if err != nil {
		return fmt.Errorf("bestbefore: %w", err)
	}
	if active != nil && active.stop != nil {
		active.stop()
	}
	active = p
	p.startSweeper()
	return nil
}

func newPlugin(raw any) (*plugin, error) {
	cfg := config{
		GatewayURL:     "http://127.0.0.1:8080",
		TavilyURL:      "https://api.tavily.com",
		MaxClaims:      4,
		MaxResults:     5,
		TimeoutSeconds: 25,
		SweepInterval:  "5m",
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
		return nil, errors.New(`judge_model is required, for example "openai/gpt-4o-mini"`)
	}
	if cfg.TavilyAPIKey == "" && cfg.VirtualKey == "" {
		return nil, errors.New("set tavily_api_key, or virtual_key with tavily_url pointing at a Bifrost relay")
	}
	if cfg.MaxClaims <= 0 || cfg.MaxResults <= 0 || cfg.TimeoutSeconds <= 0 {
		return nil, errors.New("max_claims, max_results and timeout_seconds must be positive")
	}
	if _, err := cfg.sweepEvery(); err != nil {
		return nil, err
	}
	if err := applyRecheckWindows(cfg.RecheckWindows); err != nil {
		return nil, err
	}

	token := rand.Text()
	client := &http.Client{Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second}
	judge := &freshness.GatewayLLM{BaseURL: cfg.GatewayURL, Model: cfg.JudgeModel, Fallbacks: cfg.JudgeFallbacks, VirtualKey: cfg.VirtualKey, Token: token, HTTP: client}
	return &plugin{
		cfg:     cfg,
		http:    client,
		token:   token,
		ledger:  freshness.NewLedger(10000),
		pending: newPendingChecks(),
		stats:   &stats{},
		healer:  &freshness.Healer{LLM: judge},
		verifier: &freshness.Verifier{
			LLM:       judge,
			Search:    &freshness.Tavily{BaseURL: cfg.TavilyURL, APIKey: cfg.TavilyAPIKey, VirtualKey: cfg.VirtualKey, MaxResults: cfg.MaxResults, HTTP: client},
			MaxClaims: cfg.MaxClaims,
			Now:       time.Now,
		},
	}, nil
}

func (c config) sweepEvery() (time.Duration, error) {
	if c.SweepInterval == "" || c.SweepInterval == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(c.SweepInterval)
	if err != nil || d < time.Second {
		return 0, fmt.Errorf("sweep_interval must be a duration of at least 1s, or \"0\" to turn it off")
	}
	return d, nil
}

// applyRecheckWindows lets operators shorten or lengthen how long each kind of
// claim is trusted, for example {"pricing": "6h"}.
func applyRecheckWindows(windows map[string]string) error {
	for kind, value := range windows {
		k := freshness.Kind(kind)
		if _, ok := freshness.RecheckWindow[k]; !ok {
			return fmt.Errorf("recheck_windows: unknown claim kind %q", kind)
		}
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			return fmt.Errorf("recheck_windows: %s must be a positive duration", kind)
		}
		freshness.RecheckWindow[k] = d
	}
	return nil
}

func resolveEnv(v string) string {
	if name, ok := strings.CutPrefix(v, "env."); ok {
		return os.Getenv(name)
	}
	return v
}

func GetName() string { return pluginName }

func Cleanup() error {
	if p := active; p != nil && p.stop != nil {
		p.stop()
	}
	active = nil
	return nil
}

// outcome is a report plus what the plugin did about it, as sent to callers.
type outcome struct {
	freshness.Report
	Origin         string `json:"origin"`
	CacheHit       bool   `json:"cache_hit"`
	CacheEvicted   bool   `json:"cache_evicted"`
	Healed         bool   `json:"healed"`
	OriginalAnswer string `json:"original_answer,omitempty"`
	HealError      string `json:"heal_error,omitempty"`

	answer string
}

// check verifies an answer and, when it is stale and heal is set, rewrites it.
func (p *plugin) check(question, answer string, heal bool) outcome {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(p.cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	out := outcome{Report: p.verifier.Verify(ctx, question, answer), Origin: "verified", answer: answer}
	if out.Detail != nil {
		log.Printf("bestbefore: %s: %v", out.Error, out.Detail)
	}
	p.stats.record(out.Report)
	if !out.Stale() || !heal || !p.heals() {
		return out
	}
	healed, tokens, err := p.healer.Heal(ctx, question, answer, out.Report)
	out.JudgeTokens += tokens
	if err != nil {
		if !errors.Is(err, freshness.ErrNothingToHeal) {
			out.HealError = "could not rewrite the answer"
			log.Printf("bestbefore: heal failed: %v", err)
		}
		return out
	}
	p.stats.healed.Add(1)
	out.Healed, out.OriginalAnswer, out.answer = true, answer, healed
	out.Status = freshness.StatusHealed
	return out
}

// ledgerEntry describes the answer that ends up in the cache.
func (o outcome) ledgerEntry(question string) freshness.Entry {
	return freshness.Entry{Report: o.Report, Question: question, Answer: o.answer}
}

func (p *plugin) heals() bool       { return p.cfg.Heal == nil || *p.cfg.Heal }
func (p *plugin) evictsStale() bool { return p.cfg.EvictStale == nil || *p.cfg.EvictStale }

func (p *plugin) evict(cacheID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	target := strings.TrimRight(p.cfg.GatewayURL, "/") + "/api/cache/clear/" + url.PathEscape(cacheID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, target, nil)
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

// evictTwice clears an entry now and again once the cache's own asynchronous
// write has had time to land.
func (p *plugin) evictTwice(cacheID string) bool {
	ok := p.evict(cacheID) == nil
	if ok {
		p.stats.evicted.Add(1)
	}
	go func() {
		time.Sleep(evictRetryDelay)
		p.evict(cacheID)
	}()
	return ok
}
