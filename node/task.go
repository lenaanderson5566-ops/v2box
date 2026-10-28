package node

import (
	"context"
	"errors"
	"reflect"
	"time"

	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/common/task"
	log "github.com/sirupsen/logrus"
)

func (c *Controller) startTasks(node *panel.NodeInfo) {
	// fetch node info task
	c.nodeInfoMonitorPeriodic = &task.Task{
		Name:     "nodeInfoMonitor",
		Interval: node.PullInterval,
		Jitter:   true,
		Execute:  c.nodeInfoMonitor,
		ReloadCh: c.server.ReloadCh,
	}
	// fetch user list task
	c.userReportPeriodic = &task.Task{
		Name:     "reportUserTrafficTask",
		Interval: node.PushInterval,
		Jitter:   true,
		Execute:  c.reportUserTrafficTask,
		ReloadCh: c.server.ReloadCh,
	}
	log.WithField("tag", c.tag).Info("Start monitor node status")
	// delay to start nodeInfoMonitor
	_ = c.nodeInfoMonitorPeriodic.Start(false)
	log.WithField("tag", c.tag).Info("Start report node status")
	_ = c.userReportPeriodic.Start(false)
	c.targetReportPeriodic = &task.Task{
		Name:         "reportTargetStatistics",
		NextInterval: func() time.Duration { return c.targetReportDelay(time.Now()) },
		Interval:     60 * time.Second,
		Jitter:       true,
		Execute:      func(ctx context.Context) error { c.reportTargetStatistics(ctx); return nil },
	}
	_ = c.targetReportPeriodic.Start(true)
	if node.Security == panel.Tls {
		switch c.info.Common.CertInfo.CertMode {
		case "none", "", "file", "self", "remote":
		default:
			c.renewCertPeriodic = &task.Task{
				Name:     "renewCertTask",
				Interval: time.Hour * 24,
				Execute:  c.renewCertTask,
				ReloadCh: c.server.ReloadCh,
			}
			log.WithField("tag", c.tag).Info("Start renew cert")
			// delay to start renewCert
			_ = c.renewCertPeriodic.Start(true)
		}
	}
}

func (c *Controller) nodeInfoMonitor(ctx context.Context) (err error) {
	// A failed budget refresh preserves the last local restriction.
	if budget, budgetErr := c.apiClient.GetTrafficBudget(ctx); budgetErr == nil {
		snapshot := c.server.SetTrafficBudget(c.tag, budget)
		if saveErr := c.apiClient.SaveTrafficBudget(snapshot); saveErr != nil {
			log.WithField("tag", c.tag).WithError(saveErr).Error("Save traffic budget failed")
		}
	} else {
		log.WithField("tag", c.tag).WithError(budgetErr).Error("Refresh traffic budget failed")
	}
	// get node info
	newN, err := c.apiClient.GetNodeInfo(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Error("Get node info failed")
		return nil
	}
	if completedTransitionOnly(c.info, newN) {
		// Publication removes candidate metadata but does not change listeners.
		// Acknowledge the final hash without disconnecting clients a second time.
		_ = c.apiClient.ReportConfiguration(ctx, newN.ConfigurationHash, "", c.info.CertificateFingerprint)
		c.reloadPending = false
		newN = nil
	}
	if newN != nil {
		c.reloadPending = true
	}
	// Keep requesting replacement after a rejected reload, even if the panel
	// returns 304 for the configuration already fetched by this controller.
	if c.reloadPending {
		log.WithFields(log.Fields{
			"tag": c.tag,
		}).Error("Got new node info, reload")
		if c.server.ReloadCh != nil {
			select {
			case c.server.ReloadCh <- struct{}{}:
			default:
			}
		} else {
			log.Panic("Reload failed")
		}
		return nil
	}
	log.WithField("tag", c.tag).Debug("Node info no change")

	// get user info
	newU, err := c.apiClient.GetUserList(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Error("Get user list failed")
		return nil
	}
	// Commit authorization changes before optional online-count refreshes. The
	// user response has already advanced its ETag; dropping it here can leave
	// revoked accounts active indefinitely after the panel starts returning 304.
	if newU != nil {
		deleted, added, modified := compareUserList(c.userList, newU)
		if len(added) > 0 || len(deleted) > 0 {
			if err = c.server.SetUsersWithCommit(c.tag, newU, func() { c.limiter.UpdateUser(c.tag, added, deleted, modified) }); err != nil {
				c.apiClient.InvalidateUserCache()
				log.WithField("tag", c.tag).WithError(err).Error("Update node users failed")
				return nil
			}
		} else if len(modified) > 0 {
			c.limiter.UpdateUser(c.tag, nil, nil, modified)
		}
		c.userList = newU
		log.WithField("tag", c.tag).Infof("%d user deleted, %d user added, %d user modified", len(deleted), len(added), len(modified))
	}
	// Refresh counts even on a 304 user response; failures retain prior limits.
	newA, err := c.apiClient.GetUserAlive(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		log.WithFields(log.Fields{
			"tag": c.tag,
			"err": err,
		}).Error("Get alive list failed")
		return nil
	}

	// update alive list
	if newA != nil {
		c.limiter.SetAlive(newA)
	}
	return nil
}

func completedTransitionOnly(old, next *panel.NodeInfo) bool {
	if old == nil || next == nil || old.Common == nil || next.Common == nil || old.Common.DomainTransition == nil || next.Common.DomainTransition != nil {
		return false
	}
	if old.Id != next.Id || old.Tag != next.Tag || old.Type != next.Type || old.Security != next.Security || old.PushInterval != next.PushInterval || old.PullInterval != next.PullInterval {
		return false
	}
	before, after := *old.Common, *next.Common
	before.DomainTransition = nil
	return reflect.DeepEqual(before, after)
}
