package freshness

import (
	"sort"
	"sync"
	"time"
)

// Entry is what the ledger knows about one cached answer: its last report and
// the text needed to check it again without a user asking.
type Entry struct {
	Report   Report
	Question string
	Answer   string
	// Failures counts re-checks in a row that could not run, for backoff.
	Failures int
}

// maxStoredText bounds the memory each entry can hold.
const maxStoredText = 8000

// Ledger gives every cached answer a best-before date: the time its most
// volatile claim is due for another check.
// ponytail: in-memory and per-process, so it resets on restart and is not
// shared across gateway replicas. Move it into the cache's vector store
// metadata when that matters.
type Ledger struct {
	mu      sync.Mutex
	entries map[string]Entry
	limit   int
}

func NewLedger(limit int) *Ledger {
	return &Ledger{entries: make(map[string]Entry), limit: limit}
}

func (l *Ledger) Put(cacheID string, e Entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.put(cacheID, e)
}

func (l *Ledger) put(cacheID string, e Entry) {
	if _, exists := l.entries[cacheID]; !exists && len(l.entries) >= l.limit {
		l.evictOne()
	}
	if len(e.Question) > maxStoredText {
		e.Question = e.Question[:maxStoredText]
	}
	if len(e.Answer) > maxStoredText {
		e.Answer = e.Answer[:maxStoredText]
	}
	l.entries[cacheID] = e
}

// evictOne makes room, preferring entries that never need a re-check.
func (l *Ledger) evictOne() {
	victim := ""
	for id, e := range l.entries {
		victim = id
		if e.Report.RecheckAfter.IsZero() {
			break
		}
	}
	delete(l.entries, victim)
}

// ReplaceIf stores e only if the entry is still the one last checked at
// checkedAt, so a slow background re-check cannot overwrite a newer answer.
func (l *Ledger) ReplaceIf(cacheID string, checkedAt time.Time, e Entry) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.entries[cacheID]; !ok || !cur.Report.CheckedAt.Equal(checkedAt) {
		return false
	}
	l.put(cacheID, e)
	return true
}

// DeleteIf removes the entry only if it is still the one last checked at checkedAt.
func (l *Ledger) DeleteIf(cacheID string, checkedAt time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.entries[cacheID]; !ok || !cur.Report.CheckedAt.Equal(checkedAt) {
		return false
	}
	delete(l.entries, cacheID)
	return true
}

// CheckedAt reports when the entry was last checked, if there is one.
func (l *Ledger) CheckedAt(cacheID string) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[cacheID]
	return e.Report.CheckedAt, ok
}

// Current returns the stored report while it is still within its recheck
// window. Reports with no window (no time-sensitive wording at all) never expire.
func (l *Ledger) Current(cacheID string, now time.Time) (Report, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[cacheID]
	if !ok || e.due(now) {
		return Report{}, false
	}
	return e.Report, true
}

func (l *Ledger) Delete(cacheID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, cacheID)
}

// Due lists entries past their best-before date, or every entry when all is set.
func (l *Ledger) Due(now time.Time, all bool) map[string]Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]Entry{}
	for id, e := range l.entries {
		if all || e.due(now) {
			out[id] = e
		}
	}
	return out
}

type LedgerRow struct {
	CacheID      string    `json:"cache_id"`
	Question     string    `json:"question"`
	Status       Status    `json:"status"`
	Score        float64   `json:"freshness_score"`
	CheckedAt    time.Time `json:"checked_at"`
	RecheckAfter time.Time `json:"recheck_after,omitempty"`
}

// Snapshot lists every entry, soonest best-before date first.
func (l *Ledger) Snapshot() []LedgerRow {
	l.mu.Lock()
	rows := make([]LedgerRow, 0, len(l.entries))
	for id, e := range l.entries {
		rows = append(rows, LedgerRow{
			CacheID: id, Question: e.Question, Status: e.Report.Status, Score: e.Report.Score,
			CheckedAt: e.Report.CheckedAt, RecheckAfter: e.Report.RecheckAfter,
		})
	}
	l.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i].RecheckAfter, rows[j].RecheckAfter
		if a.IsZero() != b.IsZero() {
			return b.IsZero()
		}
		return a.Before(b)
	})
	return rows
}

func (e Entry) due(now time.Time) bool {
	return !e.Report.RecheckAfter.IsZero() && !now.Before(e.Report.RecheckAfter)
}
