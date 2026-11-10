package freshness

import (
	"sync"
	"time"
)

// Ledger remembers when each cached answer was last verified, so a cache hit
// is re-checked only once its most volatile claim is due.
// ponytail: in-memory and per-process, so it resets on restart and is not
// shared across gateway replicas. Move it into the cache's vector store
// metadata when that matters.
type Ledger struct {
	mu      sync.Mutex
	entries map[string]Report
	limit   int
}

func NewLedger(limit int) *Ledger {
	return &Ledger{entries: make(map[string]Report), limit: limit}
}

func (l *Ledger) Put(cacheID string, r Report) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.entries) >= l.limit {
		for id := range l.entries {
			delete(l.entries, id)
			break
		}
	}
	l.entries[cacheID] = r
}

// Current returns the stored report while it is still within its recheck window.
func (l *Ledger) Current(cacheID string, now time.Time) (Report, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.entries[cacheID]
	if !ok {
		return Report{}, false
	}
	if r.Status != StatusTimeless && !now.Before(r.RecheckAfter) {
		return Report{}, false
	}
	return r, true
}

func (l *Ledger) Delete(cacheID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, cacheID)
}
