package node

import (
	"context"

	"fmt"
	"time"

	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"github.com/lenaanderson5566-ops/v2box/common/task"
	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/core"
	"github.com/lenaanderson5566-ops/v2box/limiter"
	log "github.com/sirupsen/logrus"
)

type Controller struct {
	server                  *core.V2Core
	apiClient               *panel.Client
	tag                     string
	limiter                 *limiter.Limiter
	userList                []panel.UserInfo
	usersPrepared           bool
	preparedBudget          *panel.TrafficBudget
	reloadPending           bool
	aliveMap                map[int]int
	conf                    *conf.NodeConfig
	info                    *panel.NodeInfo
	nodeInfoMonitorPeriodic *task.Task
	userReportPeriodic      *task.Task
	targetReportPeriodic    *task.Task
	// Owned solely by the target statistics task goroutine.
	targetLastReport  time.Time
	targetInterval    time.Duration
	targetRetry       bool
	renewCertPeriodic *task.Task
}

// NewController return a Node controller with default parameters.
func NewController(api *panel.Client, conf *conf.NodeConfig, info *panel.NodeInfo) *Controller {
	controller := &Controller{
		apiClient: api,
		info:      info,
		conf:      conf,
	}
	return controller
}

// Start implement the Start() function of the service interface
func (c *Controller) Start(x *core.V2Core) error {
	// Init Core
	c.server = x
	var err error
	// First fetch Node Info
	node := c.info
	if node == nil {
		c.info, err = c.apiClient.GetNodeInfo(context.Background())
		if err != nil {
			return fmt.Errorf("get node info error: %s", err)
		}
		node = c.info
	}
	if !c.usersPrepared {
		if err := c.prepareUsers(); err != nil {
			return err
		}
	}
	c.tag = node.Tag
	c.server.SetTrafficBudget(c.tag, c.preparedBudget)

	// add limiter
	l := limiter.AddLimiter(c.info.Type, c.tag, c.userList, c.aliveMap)
	c.limiter = l
	if node.Security == panel.Tls {
		err = c.requestCert()
		if err != nil {
			return fmt.Errorf("request cert error: %s", err)
		}
	}
	// Add new tag
	err = c.server.AddNode(c.tag, node)
	if err != nil {
		return fmt.Errorf("add new node error: %s", err)
	}
	added, err := c.server.AddUsers(&core.AddUsersParams{
		Tag:      c.tag,
		Users:    c.userList,
		NodeInfo: node,
	})
	if err != nil {
		return fmt.Errorf("add users error: %s", err)
	}
	log.WithField("tag", c.tag).Infof("Added %d new users", added)
	c.info = node
	c.startTasks(node)
	return nil
}

// Fetch all authentication inputs before replacing live listeners.
func (c *Controller) prepareUsers() error {
	users, err := c.apiClient.GetUserList(context.Background())
	if err != nil {
		return fmt.Errorf("get user list error: %w", err)
	}
	alive, err := c.apiClient.GetUserAlive(context.Background())
	if err != nil {
		return fmt.Errorf("get user alive list error: %w", err)
	}
	budget, err := c.apiClient.GetTrafficBudget(context.Background())
	if err != nil {
		return fmt.Errorf("get traffic budget error: %w", err)
	}
	c.preparedBudget = budget
	c.userList, c.aliveMap, c.usersPrepared = users, alive, true
	return nil
}

// Close implement the Close() function of the service interface
func (c *Controller) Close() error {
	if c.server == nil || c.limiter == nil {
		return nil
	}

	if c.nodeInfoMonitorPeriodic != nil {
		c.nodeInfoMonitorPeriodic.Close()
	}
	if c.userReportPeriodic != nil {
		c.userReportPeriodic.Close()
	}
	if c.targetReportPeriodic != nil {
		c.targetReportPeriodic.Close()
	}
	if c.renewCertPeriodic != nil {
		c.renewCertPeriodic.Close()
	}
	err := c.server.DelNode(c.tag)
	reportCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	traffic, _ := c.server.GetUserTrafficSlice(c.tag, 0)
	if len(traffic) > 0 {
		if reportErr := c.apiClient.ReportUserTraffic(reportCtx, traffic); reportErr != nil {
			c.server.RestoreUserTraffic(c.tag, traffic)
			log.WithError(reportErr).Warn("Final traffic report failed")
		}
	}
	limiter.DeleteLimiter(c.tag)
	if err != nil {
		return fmt.Errorf("del node error: %s", err)
	}
	return nil
}
