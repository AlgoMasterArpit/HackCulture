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
	"sync"
	"sync/atomic"
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
	// AdminModels turns on bestbefore/status and bestbefore/sweep. They list
	// cached questions and can trigger paid re-checks, so they are off unless
	// an operator asks for them.
	AdminModels bool `json:"admin_models"`
	// CheckScope is "cached" (default) to check only answers that are going
	// into the semantic cache, or "all" to check every chat answer.
	CheckScope string `json:"check_scope"`
	// Revalidate is "background" (default) to serve a cache hit that is past
	// its best-before date at once and re-check it behind the scenes, or
	// "inline" to re-check it before answering.
	Revalidate string `json:"revalidate"`
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
	// ctx ends when the plugin is cleaned up or replaced, stopping the
	// sweeper and any check it is running.
	ctx      context.Context
	stop     context.CancelFunc
	sweeping sync.Mutex
	inflight inflight
	// background holds a slot per running background revalidation.
	background chan struct{}
}

var current atomic.Pointer[plugin]

func activePlugin() *plugin { return current.Load() }

func Init(raw any) error {
	p, err := newPlugin(raw)
	if err != nil {
		return fmt.Errorf("bestbefore: %w", err)
	}
	if old := current.Swap(p); old != nil {
		old.stop()
	}
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
		CheckScope:     "cached",
		Revalidate:     "background",
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
	if cfg.CheckScope != "cached" && cfg.CheckScope != "all" {
		return nil, errors.New(`check_scope must be "cached" or "all"`)
	}
	if cfg.Revalidate != "background" && cfg.Revalidate != "inline" {
		return nil, errors.New(`revalidate must be "background" or "inline"`)
	}
	windows, err := recheckWindows(cfg.RecheckWindows)
	if err != nil {
		return nil, err
	}

	token := rand.Text()
	client := &http.Client{Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second}
	judge := &freshness.GatewayLLM{BaseURL: cfg.GatewayURL, Model: cfg.JudgeModel, Fallbacks: cfg.JudgeFallbacks, VirtualKey: cfg.VirtualKey, Token: token, HTTP: client}
	ctx, stop := context.WithCancel(context.Background())
	return &plugin{
		ctx:        ctx,
		stop:       stop,
		background: make(chan struct{}, 4),
		cfg:        cfg,
		http:       client,
		token:      token,
		ledger:     freshness.NewLedger(10000),
		pending:    newPendingChecks(),
		stats:      &stats{},
		healer:     &freshness.Healer{LLM: judge},
		verifier: &freshness.Verifier{
			LLM:       judge,
			Search:    &freshness.Tavily{BaseURL: cfg.TavilyURL, APIKey: cfg.TavilyAPIKey, VirtualKey: cfg.VirtualKey, MaxResults: cfg.MaxResults, HTTP: client},
			MaxClaims: cfg.MaxClaims,
			Now:       time.Now,
			Windows:   windows,
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

// recheckWindows lets operators shorten or lengthen how long each kind of
// claim is trusted, for example {"pricing": "6h"}. The result belongs to one
// plugin instance; the package defaults are never modified.
func recheckWindows(overrides map[string]string) (map[freshness.Kind]time.Duration, error) {
	windows := make(map[freshness.Kind]time.Duration, len(freshness.RecheckWindow))
	for k, d := range freshness.RecheckWindow {
		windows[k] = d
	}
	for kind, value := range overrides {
		k := freshness.Kind(kind)
		if _, ok := windows[k]; !ok {
			return nil, fmt.Errorf("recheck_windows: unknown claim kind %q", kind)
		}
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("recheck_windows: %s must be a positive duration", kind)
		}
		windows[k] = d
	}
	return windows, nil
}

func resolveEnv(v string) string {
	if name, ok := strings.CutPrefix(v, "env."); ok {
		return os.Getenv(name)
	}
	return v
}

func GetName() string { return pluginName }

func Cleanup() error {
	if p := current.Swap(nil); p != nil {
		p.stop()
	}
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
	Corrected      string `json:"corrected_answer,omitempty"`
	HealError      string `json:"heal_error,omitempty"`

	answer string
}

// check verifies an answer and, when it is stale and heal is set, rewrites it.
func (p *plugin) check(question, answer string, heal bool) outcome {
	return p.checkWithin(p.ctx, question, answer, heal)
}

func (p *plugin) checkWithin(parent context.Context, question, answer string, heal bool) outcome {
	ctx, cancel := context.WithTimeout(parent, time.Duration(p.cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	out := outcome{Report: p.verifier.Verify(ctx, question, answer), Origin: "verified", answer: answer}
	if out.Detail != nil {
		log.Printf("bestbefore: %s: %v", out.Error, out.Detail)
	}
	p.stats.record(out.Report)
	if !out.Stale() || !heal || !p.heals() {
		return out
	}
	// The rewrite gets its own budget, so a slow check does not starve it.
	healCtx, cancelHeal := context.WithTimeout(parent, time.Duration(p.cfg.TimeoutSeconds)*time.Second)
	defer cancelHeal()
	healed, tokens, err := p.healer.Heal(healCtx, question, answer, out.Report)
	out.JudgeTokens += tokens
	if err != nil {
		if !errors.Is(err, freshness.ErrNothingToHeal) {
			out.HealError = "could not rewrite the answer"
			log.Printf("bestbefore: heal failed: %v", err)
		}
		return out
	}
	p.stats.healed.Add(1)
	out.Healed, out.OriginalAnswer, out.Corrected, out.answer = true, answer, healed, healed
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
// write has had time to land. The second pass is skipped if a newer answer
// was recorded under the same id in the meantime, so it cannot delete a fix.
func (p *plugin) evictTwice(cacheID string) bool {
	evictedAt := time.Now()
	ok := p.evict(cacheID) == nil
	if ok {
		p.stats.evicted.Add(1)
	}
	go func() {
		time.Sleep(evictRetryDelay)
		if at, stored := p.ledger.CheckedAt(cacheID); stored && at.After(evictedAt) {
			return
		}
		p.evict(cacheID)
	}()
	return ok
}
