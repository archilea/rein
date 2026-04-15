package meter

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrBudgetExceeded is returned by Meter.Check when a key's daily or monthly
// cap would be exceeded by further spend. Callers should translate this into a
// 402 Payment Required at the edge.
var ErrBudgetExceeded = errors.New("budget exceeded")

// Meter tracks accumulated USD spend per virtual key and enforces caps.
// Implementations must be safe for concurrent use.
//
// Budgets are advisory in the sense that Rein can only Record cost AFTER an
// upstream response is received, so a burst of concurrent requests can slip
// past a soft cap. The kill-switch is the independent hard stop.
type Meter interface {
	// Check returns ErrBudgetExceeded if the accumulated spend for keyID
	// has already reached either cap. A cap of 0 means "no limit on this
	// window". Check is called on every request before the upstream fetch.
	Check(ctx context.Context, keyID string, dailyCap, monthCap float64) error

	// Record adds cost (in USD) to the key's daily and monthly totals.
	// Called from ModifyResponse hooks after a successful upstream call.
	Record(ctx context.Context, keyID string, cost float64) error
}

// Memory is an in-process Meter. Spend totals are lost on restart.
//
// This is suitable for single-replica 0.1 deployments. For multi-replica
// setups or for strict "survive an OOM" guarantees, a durable Meter backed by
// SQLite or Redis is needed (tracked for 0.2).
type Memory struct {
	mu    sync.Mutex
	spend map[string]float64 // key: "<keyID>|<period>"
}

// NewMemory returns an empty in-process Meter.
func NewMemory() *Memory {
	return &Memory{spend: make(map[string]float64)}
}

// Check implements Meter.
func (m *Memory) Check(_ context.Context, keyID string, dailyCap, monthCap float64) error {
	if dailyCap <= 0 && monthCap <= 0 {
		return nil
	}
	dayKey, monthKey := periodKeys(keyID, time.Now().UTC())
	m.mu.Lock()
	defer m.mu.Unlock()
	if dailyCap > 0 && m.spend[dayKey] >= dailyCap {
		return ErrBudgetExceeded
	}
	if monthCap > 0 && m.spend[monthKey] >= monthCap {
		return ErrBudgetExceeded
	}
	return nil
}

// Record implements Meter.
func (m *Memory) Record(_ context.Context, keyID string, cost float64) error {
	if cost <= 0 {
		return nil
	}
	dayKey, monthKey := periodKeys(keyID, time.Now().UTC())
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spend[dayKey] += cost
	m.spend[monthKey] += cost
	return nil
}

// dayPeriodKey returns the UTC-anchored daily bucket id for t,
// formatted "d:YYYY-MM-DD".
func dayPeriodKey(t time.Time) string {
	return "d:" + t.UTC().Format("2006-01-02")
}

// monthPeriodKey returns the UTC-anchored monthly bucket id for t,
// formatted "m:YYYY-MM".
func monthPeriodKey(t time.Time) string {
	return "m:" + t.UTC().Format("2006-01")
}

// periodKeys produces the per-day and per-month map keys for keyID.
// Boundaries are always UTC to keep multi-region deployments consistent.
func periodKeys(keyID string, now time.Time) (string, string) {
	return keyID + "|" + dayPeriodKey(now),
		keyID + "|" + monthPeriodKey(now)
}
