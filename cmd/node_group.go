package cmd

import (
	"context"
	"errors"
	"fmt"
	panel "github.com/lenaanderson5566-ops/v2box/api/v2board"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/core"
	"github.com/lenaanderson5566-ops/v2box/node"
	log "github.com/sirupsen/logrus"
)

// One worker owns a controller, core and all replacements for one panel/node.
// Slow preparation, retry and rollback never close another owner's listeners.
type nodeGroup struct {
	ctx        context.Context
	mu         sync.Mutex
	accounting map[string]*core.V2Core
	resources  map[string]*nodeWorker
	retiring   map[string]<-chan struct{}
	workers    map[string]*nodeWorker
	all        sync.WaitGroup
	closed     bool
	retry      time.Duration
}
type nodeWorker struct {
	group         *nodeGroup
	accounting    *core.V2Core
	claims        map[string]bool
	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
	before        <-chan struct{}
	wake          chan struct{}
	reload        chan struct{}
	mu            sync.Mutex
	desired       *conf.Conf
	cancelAttempt context.CancelFunc
	retry         time.Duration
	nodes         *node.Node // accessed only by run
	core          *core.V2Core
}

func newNodeGroup(ctx context.Context) *nodeGroup {
	return &nodeGroup{ctx: ctx, workers: make(map[string]*nodeWorker), resources: make(map[string]*nodeWorker), accounting: make(map[string]*core.V2Core), retiring: make(map[string]<-chan struct{}), retry: 30 * time.Second}
}
func (g *nodeGroup) Apply(config *conf.Conf) error {
	desired := make(map[string]*conf.Conf, len(config.NodeConfigs))
	for _, entry := range config.NodeConfigs {
		key, err := conf.NodeIdentity(entry)
		if err != nil {
			return err
		}
		if _, exists := desired[key]; exists {
			return fmt.Errorf("duplicate panel/node identity")
		}
		local := *config
		local.OutboundAllowCIDRs = append([]string(nil), config.OutboundAllowCIDRs...)
		// Stable paths also work when another panel is added later.
		if entry.CertificateDirectory == "" {
			entry.CertificateDirectory = conf.NodeCertificateDirectory(entry)
		}
		local.NodeConfigs = []conf.NodeConfig{entry}
		desired[key] = &local
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return errors.New("node supervisors closed")
	}
	for key, worker := range g.workers {
		if _, ok := desired[key]; !ok {
			worker.cancel()
			g.retiring[key] = worker.done
			delete(g.workers, key)
		}
	}
	for key, config := range desired {
		if worker, ok := g.workers[key]; ok {
			worker.update(config)
			continue
		}
		ctx, cancel := context.WithCancel(g.ctx)
		worker := &nodeWorker{group: g, accounting: g.backendAccounting(config), claims: make(map[string]bool), ctx: ctx, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1), reload: make(chan struct{}, 1), desired: config, retry: g.retry, before: g.retiring[key]}
		g.workers[key] = worker
		g.all.Add(1)
		go func() {
			defer g.all.Done()
			worker.run()
			g.mu.Lock()
			if g.retiring[key] == worker.done {
				delete(g.retiring, key)
			}
			g.mu.Unlock()
		}()
	}
	return nil
}
func (g *nodeGroup) Close() {
	g.mu.Lock()
	g.closed = true
	for _, worker := range g.workers {
		worker.cancel()
	}
	g.mu.Unlock()
	g.all.Wait()
}
func (w *nodeWorker) update(next *conf.Conf) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if reflect.DeepEqual(w.desired, next) {
		return
	}
	w.desired = next
	if w.cancelAttempt != nil {
		w.cancelAttempt()
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}
func (w *nodeWorker) run() {
	defer close(w.done)
	defer w.releaseClaims()
	defer w.closeRuntime()
	if w.before != nil {
		select {
		case <-w.before:
		case <-w.ctx.Done():
			return
		}
	}
	delay := w.retry
	var retry <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	attempt := func() {
		w.mu.Lock()
		desired := w.desired
		ctx, cancel := context.WithTimeout(w.ctx, 5*time.Minute)
		w.cancelAttempt = cancel
		w.mu.Unlock()
		err := w.reconcile(ctx, desired)
		cancel()
		w.mu.Lock()
		w.cancelAttempt = nil
		w.mu.Unlock()
		if err == nil {
			delay = w.retry
			retry = nil
			return
		}
		if w.ctx.Err() != nil {
			return
		}
		var rejected *reloadRejected
		if !errors.As(err, &rejected) {
			w.closeRuntime()
		}
		// Do not put keys or full request URLs in supervisor logs.
		log.WithFields(log.Fields{"panel": desired.NodeConfigs[0].APIHost, "nodeId": desired.NodeConfigs[0].NodeID}).WithError(err).Error("Node update failed; other nodes remain active; retry scheduled")
		if timer != nil {
			timer.Stop()
		}
		timer = time.NewTimer(delay)
		retry = timer.C
		delay = min(delay*2, 5*time.Minute)
	}
	attempt()
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-w.wake:
			if timer != nil {
				timer.Stop()
			}
			retry = nil
			delay = w.retry
			attempt()
		case <-w.reload:
			// A failed backend gets bounded retries even if polling queues more signals.
			if retry == nil {
				attempt()
			}
		case <-retry:
			retry = nil
			attempt()
		}
	}
}
func (w *nodeWorker) closeRuntime() {
	if w.nodes != nil {
		_ = w.nodes.Close()
		w.nodes = nil
	}
	if w.core != nil {
		_ = w.core.Close()
	}
}
func (w *nodeWorker) reconcile(ctx context.Context, config *conf.Conf) error {
	if w.nodes != nil {
		return reloadConfiguration(ctx, config, &w.nodes, &w.core, w.claimResources)
	}
	nodes, err := node.NewContext(ctx, config.NodeConfigs)
	if err != nil {
		return &reloadRejected{err}
	}
	finish, err := w.claimResources(nodes)
	if err != nil {
		_ = nodes.Close()
		return &reloadRejected{err}
	}
	candidate := core.New(config)
	candidate.ReloadCh = w.reload
	candidate.PreserveAccountingFrom(w.accounting)
	complete := false
	defer func() {
		if !complete {
			_ = nodes.Close()
			_ = candidate.Close()
		}
		finish(complete)
	}()
	if err = candidate.Start(nodes.NodeInfos); err != nil {
		nodes.ReportConfigurationContext(ctx, "core")
		return &reloadRejected{err}
	}
	if err = nodes.PrepareUsersContext(ctx, candidate); err != nil {
		nodes.ReportConfigurationContext(ctx, "users")
		return &reloadRejected{err}
	}
	if err = nodes.PrepareCertificatesContext(ctx); err != nil {
		nodes.ReportConfigurationContext(ctx, "certificate")
		return &reloadRejected{err}
	}
	if err = nodes.StartContext(ctx, config.NodeConfigs, candidate); err != nil {
		nodes.ReportConfigurationContext(ctx, "start")
		return &reloadRejected{err}
	}
	if err = nodes.ReportConfigurationContext(ctx, ""); err != nil {
		return &reloadRejected{err}
	}
	w.nodes, w.core = nodes, candidate
	complete = true
	return nil
}

func (w *nodeWorker) claimResources(candidate *node.Node) (func(bool), error) {
	paths := make(map[string]bool)
	for _, info := range candidate.NodeInfos {
		if info.Security != panel.Tls || info.Common.CertInfo.CertMode == "remote" {
			continue
		}
		cert := info.Common.CertInfo
		replace := strings.NewReplacer("{domain}", cert.CertDomain, "{email}", cert.Email)
		for _, name := range []string{cert.CertFile, cert.KeyFile} {
			if name == "" {
				continue
			}
			absolute, err := filepath.Abs(replace.Replace(name))
			if err != nil {
				return nil, err
			}
			paths[absolute] = true
		}
	}
	w.group.mu.Lock()
	defer w.group.mu.Unlock()
	for name := range paths {
		if owner := w.group.resources[name]; owner != nil && owner != w {
			return nil, fmt.Errorf("certificate/key path is already owned by another node; use independent paths")
		}
	}
	previous := w.claims
	for name := range paths {
		w.group.resources[name] = w
	}
	return func(commit bool) {
		w.group.mu.Lock()
		defer w.group.mu.Unlock()
		keep, remove := previous, paths
		if commit {
			keep, remove = paths, previous
			w.claims = paths
		}
		for name := range remove {
			if !keep[name] && w.group.resources[name] == w {
				delete(w.group.resources, name)
			}
		}
	}, nil
}
func (w *nodeWorker) releaseClaims() {
	w.group.mu.Lock()
	defer w.group.mu.Unlock()
	for name := range w.claims {
		if w.group.resources[name] == w {
			delete(w.group.resources, name)
		}
	}
}

// The same panel's physical-server quota must remain shared across its nodes.
// Different panels can reuse server/user IDs without sharing counters or policies.
// Caller holds g.mu while publishing workers.
func (g *nodeGroup) backendAccounting(config *conf.Conf) *core.V2Core {
	host, _ := conf.CanonicalPanelURL(config.NodeConfigs[0].APIHost)
	previous := g.accounting[host]
	if previous == nil {
		previous = core.New(config)
		g.accounting[host] = previous
	}
	return previous
}
