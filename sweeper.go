package main

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlgoMasterArpit/HackCulture/freshness"
)

type stats struct {
	checked, stale, healed, evicted, ledgerHits atomic.Int64
	sweeps, swept, sweptStale                   atomic.Int64
}

func (s *stats) record(r freshness.Report) {
	if r.Status == freshness.StatusTimeless && r.Searches == 0 {
		return
	}
	s.checked.Add(1)
	if r.Stale() {
		s.stale.Add(1)
	}
}

func (s *stats) snapshot() map[string]int64 {
	return map[string]int64{
		"answers_checked":           s.checked.Load(),
		"stale_found":               s.stale.Load(),
		"healed":                    s.healed.Load(),
		"evicted":                   s.evicted.Load(),
		"served_within_best_before": s.ledgerHits.Load(),
		"sweeps":                    s.sweeps.Load(),
		"rechecked_by_sweeper":      s.swept.Load(),
		"evicted_by_sweeper":        s.sweptStale.Load(),
	}
}

// pendingChecks hands a result from PostLLMHook, which can still change what
// the cache stores, to HTTPTransportPostHook, which annotates the response
// and knows the cache id. They are matched by the chat response id.
type pendingChecks struct {
	mu    sync.Mutex
	items map[string]pendingCheck
}

type pendingCheck struct {
	outcome
	question string
	at       time.Time
}

func newPendingChecks() *pendingChecks {
	return &pendingChecks{items: map[string]pendingCheck{}}
}

func (pc *pendingChecks) put(id, question string, o outcome) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	now := time.Now()
	for k, v := range pc.items {
		if now.Sub(v.at) > time.Minute {
			delete(pc.items, k)
		}
	}
	pc.items[id] = pendingCheck{outcome: o, question: question, at: now}
}

func (pc *pendingChecks) take(id string) (pendingCheck, bool) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	v, ok := pc.items[id]
	delete(pc.items, id)
	return v, ok
}

func (p *plugin) startSweeper() {
	every, _ := p.cfg.sweepEvery()
	if every == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.stop = cancel
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.sweep(false)
			}
		}
	}()
}

type sweepResult struct {
	CacheID  string           `json:"cache_id"`
	Question string           `json:"question"`
	Status   freshness.Status `json:"status"`
	Evicted  bool             `json:"evicted"`
}

// sweep re-checks cached answers whose best-before date has passed, so a fact
// that changed is caught before the next user asks. Stale entries are
// evicted; the next request then gets a fresh, healed answer into the cache.
func (p *plugin) sweep(all bool) []sweepResult {
	p.stats.sweeps.Add(1)
	var results []sweepResult
	for id, e := range p.ledger.Due(time.Now(), all) {
		out := p.check(e.Question, e.Answer, false)
		p.stats.swept.Add(1)
		r := sweepResult{CacheID: id, Question: e.Question, Status: out.Status}
		switch {
		case out.Status == freshness.StatusUnchecked:
			e.Report.RecheckAfter = time.Now().Add(time.Minute)
			p.ledger.Put(id, e)
		case out.Stale():
			if p.evictsStale() {
				r.Evicted = p.evictTwice(id)
				p.stats.sweptStale.Add(1)
			}
			p.ledger.Delete(id)
		default:
			p.ledger.Put(id, out.ledgerEntry(e.Question))
		}
		results = append(results, r)
	}
	return results
}
