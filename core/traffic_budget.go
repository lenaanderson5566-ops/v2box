package core

import (
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"io"
	"sync/atomic"
)

// UsedBytes in the embedded panel snapshot is never mutated by packet I/O.
// All live usage is read from used; metadata is guarded by trafficTracker.mu.
type localTrafficBudget struct {
	panel.TrafficBudget
	used atomic.Int64
}

func (t *trafficTracker) budgetBlockedLocked(tag string) bool {
	b := t.budgets[tag]
	return b != nil && b.LimitBytes > 0 && b.used.Load() >= b.LimitBytes
}
func (v *V2Core) SetTrafficBudget(tag string, b *panel.TrafficBudget) *panel.TrafficBudget {
	t := v.tracker
	if t == nil {
		return nil
	}
	t.mu.Lock()
	if b == nil {
		delete(t.budgets, tag)
		t.hasBudgets.Store(len(t.budgets) > 0)
		t.mu.Unlock()
		return nil
	}
	copyBudget := *b
	// Nodes on the same server share one admission counter in this process.
	var shared *localTrafficBudget
	for _, old := range t.budgets {
		if old.ID == b.ID {
			shared = old
			break
		}
	}
	if shared == nil {
		shared = &localTrafficBudget{TrafficBudget: copyBudget}
		shared.used.Store(copyBudget.UsedBytes)
	} else {
		if shared.PeriodStart != b.PeriodStart || shared.CountingMode != b.CountingMode {
			shared.TrafficBudget = copyBudget
			shared.used.Store(copyBudget.UsedBytes)
		} else if b.UsedBytes > shared.used.Load() {
			shared.used.Store(b.UsedBytes)
		}
		shared.LimitBytes = b.LimitBytes
		shared.PeriodEndUnix = b.PeriodEndUnix
	}
	t.budgets[tag] = shared
	t.hasBudgets.Store(true)
	var closeList []io.Closer
	if t.budgetBlockedLocked(tag) {
		for other, budget := range t.budgets {
			if budget == shared {
				for c, m := range t.connections[other] {
					m.cancel()
					closeList = append(closeList, c)
				}
			}
		}
	}
	snapshot := shared.TrafficBudget
	snapshot.UsedBytes = shared.used.Load()
	t.mu.Unlock()
	for _, c := range closeList {
		c.Close()
	}
	return &snapshot
}
func (v *V2Core) TrafficBudgetSnapshot(tag string) *panel.TrafficBudget {
	t := v.tracker
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if b := t.budgets[tag]; b != nil {
		copyBudget := b.TrafficBudget
		copyBudget.UsedBytes = b.used.Load()
		return &copyBudget
	}
	return nil
}

// An expired period is kept until the panel supplies the next period, avoiding
// clock drift or an offline node silently granting a second server-wide quota.
