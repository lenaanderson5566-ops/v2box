package node

import (
	"context"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	log "github.com/sirupsen/logrus"
	"time"
)

func (c *Controller) reportTargetStatistics(ctx context.Context) {
	// Optional telemetry runs independently of billing, with a bounded timeout and no disk spool.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	policy, err := c.apiClient.GetTargetPolicy(ctx)
	if err != nil {
		c.server.SetTargetPolicy(c.tag, panel.TargetPolicy{})
		c.targetLastReport = time.Time{}
		c.clearTargetRetry()
		return
	}
	c.server.SetTargetPolicy(c.tag, policy)
	if !policy.Enabled {
		c.targetLastReport = time.Time{}
		c.clearTargetRetry()
		return
	}
	c.targetInterval = policy.ReportInterval()
	if c.targetRetry {
		// Keep renewing the lease while backing off failed uploads.
		if time.Now().Before(c.targetRetryAt) {
			return
		}
	} else if !c.targetLastReport.IsZero() && time.Since(c.targetLastReport) < c.targetInterval {
		return
	}
	if batch := c.server.TargetBatch(c.tag); batch != nil {
		accepted, err := c.apiClient.ReportTargets(ctx, batch)
		if err != nil {
			c.scheduleTargetRetry(time.Now())
			log.WithField("tag", c.tag).Warn("Target statistics report failed")
			return
		}
		c.server.AckTargets(c.tag, batch.BatchID, accepted)
	}
	c.clearTargetRetry()
	c.targetLastReport = time.Now()
}

// Wake independently of the upload cadence to renew the 120-second collection lease.
func (c *Controller) targetReportDelay(now time.Time) time.Duration {
	if c.targetRetry {
		if c.targetRetryAt.IsZero() {
			return 30 * time.Second
		}
		return max(time.Second, min(time.Minute, c.targetRetryAt.Sub(now)))
	}
	if c.targetLastReport.IsZero() {
		return time.Minute
	}
	remaining := c.targetInterval - now.Sub(c.targetLastReport)
	return max(time.Second, min(time.Minute, remaining))
}

func (c *Controller) clearTargetRetry() {
	c.targetRetry = false
	c.targetRetryAt = time.Time{}
	c.targetRetryDelay = 0
}

func (c *Controller) scheduleTargetRetry(now time.Time) {
	c.targetRetry = true
	c.targetRetryDelay = min(2*time.Minute, max(30*time.Second, 2*c.targetRetryDelay))
	c.targetRetryAt = now.Add(c.targetRetryDelay)
}
