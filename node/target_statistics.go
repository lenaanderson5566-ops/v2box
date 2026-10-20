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
		return
	}
	c.server.SetTargetPolicy(c.tag, policy)
	if !policy.Enabled {
		return
	}
	if batch := c.server.TargetBatch(c.tag); batch != nil {
		accepted, err := c.apiClient.ReportTargets(ctx, batch)
		if err != nil {
			log.WithField("tag", c.tag).Warn("Target statistics report failed")
			return
		}
		c.server.AckTargets(c.tag, batch.BatchID, accepted)
	}
}
