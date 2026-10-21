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
		c.targetRetry = false
		return
	}
	c.server.SetTargetPolicy(c.tag, policy)
	if !policy.Enabled {
		c.targetLastReport = time.Time{}
		c.targetRetry = false
		return
	}
	c.targetInterval = policy.ReportInterval()
	if !c.targetLastReport.IsZero() && time.Since(c.targetLastReport) < c.targetInterval && !c.targetRetry {
		return
	}
	c.targetRetry = false
	if batch := c.server.TargetBatch(c.tag); batch != nil {
		accepted, err := c.apiClient.ReportTargets(ctx, batch)
		if err != nil {
			c.targetRetry = true
			log.WithField("tag", c.tag).Warn("Target statistics report failed")
			return
		}
		c.server.AckTargets(c.tag, batch.BatchID, accepted)
	}
	c.targetLastReport = time.Now()
}

// Wake independently of the upload cadence to renew the 120-second collection lease.
func (c *Controller) targetReportDelay(now time.Time) time.Duration {
	if c.targetRetry {
		return 30 * time.Second
	}
	if c.targetLastReport.IsZero() {
		return time.Minute
	}
	remaining := c.targetInterval - now.Sub(c.targetLastReport)
	return max(time.Second, min(time.Minute, remaining))
}
