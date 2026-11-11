package main

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/AlgoMasterArpit/HackCulture/freshness"
)

type stats struct {
	checked, stale, healed, evicted, ledgerHits atomic.Int64
	sweeps, swept, sweptStale, revalidated      atomic.Int64
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
		"checks_run":                s.checked.Load(),
		"stale_found":               s.stale.Load(),
		"healed":                    s.healed.Load(),
		"evicted":                   s.evicted.Load(),
		"served_within_best_before": s.ledgerHits.Load(),
		"sweeps":                    s.sweeps.Load(),
		"rechecked_by_sweeper":      s.swept.Load(),
		"evicted_by_sweeper":        s.sweptStale.Load(),
		"revalidated_in_background": s.revalidated.Load(),
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

func (pc *pendingChecks) put(key, question string, o outcome) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	now := time.Now()
	// Entries are taken within milliseconds; only orphans from failed
	// requests linger, so prune rarely instead of on every put.
	if len(pc.items) >= 256 {
		for k, v := range pc.items {
			if now.Sub(v.at) > time.Minute {
				delete(pc.items, k)
			}
		}
	}
	pc.items[key] = pendingCheck{outcome: o, question: question, at: now}
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
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-p.ctx.Done():
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
// Only one sweep runs at a time; ran is false when another was in progress.
func (p *plugin) sweep(all bool) (results []sweepResult, ran bool) {
	if !p.sweeping.TryLock() {
		return nil, false
	}
	defer p.sweeping.Unlock()
	p.stats.sweeps.Add(1)
	results = []sweepResult{}
	for id, e := range p.ledger.Due(time.Now(), all) {
		if p.ctx.Err() != nil {
			break
		}
		if r, ran := p.recheck(id, e, true); ran {
			p.stats.swept.Add(1)
			results = append(results, r)
		}
	}
	return results, true
}

// recheck verifies one cached answer again and updates the ledger and cache.
// known says whether e came from the ledger; updates are conditional on the
// entry not having changed meanwhile. Only one recheck per cache id runs at a
// time, so a burst of hits on an expired entry pays for a single check.
func (p *plugin) recheck(id string, e freshness.Entry, known bool) (sweepResult, bool) {
	if !p.inflight.claim(id) {
		return sweepResult{}, false
	}
	defer p.inflight.release(id)

	checkedAt := e.Report.CheckedAt
	out := p.checkWithin(p.ctx, e.Question, freshness.StripSources(e.Answer), false)
	r := sweepResult{CacheID: id, Question: e.Question, Status: out.Status}
	switch {
	case out.Status == freshness.StatusUnchecked:
		// Do not keep vouching for an answer that could not be re-checked,
		// and back off so an outage does not repeat paid calls every tick.
		if known {
			e.Failures++
			e.Report.Status, e.Report.Score, e.Report.Error = freshness.StatusUnchecked, out.Score, out.Error
			e.Report.RecheckAfter = time.Now().Add(min(time.Minute<<min(e.Failures, 6), time.Hour))
			p.ledger.ReplaceIf(id, checkedAt, e)
		}
	case out.Stale():
		if (!known || p.ledger.DeleteIf(id, checkedAt)) && p.evictsStale() {
			r.Evicted = p.evictTwice(id)
			if r.Evicted {
				p.stats.sweptStale.Add(1)
			}
		}
	case known:
		p.ledger.ReplaceIf(id, checkedAt, out.ledgerEntry(e.Question))
	default:
		p.ledger.Put(id, out.ledgerEntry(e.Question))
	}
	return r, true
}

// revalidateLater serves the user now and re-checks a cache hit in the
// background, like HTTP's stale-while-revalidate.
func (p *plugin) revalidateLater(id, question, answer string) {
	e, known := p.ledger.Get(id)
	if !known {
		e = freshness.Entry{Question: question, Answer: answer}
	}
	go func() {
		if _, ran := p.recheck(id, e, known); ran {
			p.stats.revalidated.Add(1)
		}
	}()
}

// inflight tracks cache ids being re-checked right now.
type inflight struct {
	mu  sync.Mutex
	ids map[string]bool
}

func (f *inflight) claim(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ids == nil {
		f.ids = map[string]bool{}
	}
	if f.ids[id] {
		return false
	}
	f.ids[id] = true
	return true
}

func (f *inflight) release(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.ids, id)
}
