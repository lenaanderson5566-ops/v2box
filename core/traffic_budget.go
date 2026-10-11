package core

import (
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"io"
)

func (t *trafficTracker) budgetBlockedLocked(tag string) bool {
	b := t.budgets[tag]
	return b != nil && b.LimitBytes > 0 && b.UsedBytes >= b.LimitBytes
}
func (v *V2Core) SetTrafficBudget(tag string, b *panel.TrafficBudget) *panel.TrafficBudget {
	t := v.tracker
	if t == nil {
		return nil
	}
	t.mu.Lock()
	if b == nil {
		delete(t.budgets, tag)
		t.mu.Unlock()
		return nil
	}
	copyBudget := *b
	// Nodes on the same server share one admission counter in this process.
	var shared *panel.TrafficBudget
	for _, old := range t.budgets {
		if old.ID == b.ID {
			shared = old
			break
		}
	}
	if shared == nil {
		shared = &copyBudget
	} else {
		if shared.PeriodStart != b.PeriodStart || shared.CountingMode != b.CountingMode {
			*shared = copyBudget
		} else if b.UsedBytes > shared.UsedBytes {
			shared.UsedBytes = b.UsedBytes
		}
		shared.LimitBytes = b.LimitBytes
		shared.PeriodEndUnix = b.PeriodEndUnix
	}
	t.budgets[tag] = shared
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
	snapshot := *shared
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
		copyBudget := *b
		return &copyBudget
	}
	return nil
}

// An expired period is kept until the panel supplies the next period, avoiding
// clock drift or an offline node silently granting a second server-wide quota.
