package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"syscall"
	"time"

	"github.com/lenaanderson5566-ops/v2box/conf"
	"github.com/lenaanderson5566-ops/v2box/core"
	"github.com/lenaanderson5566-ops/v2box/limiter"
	"github.com/lenaanderson5566-ops/v2box/node"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var (
	config string
	watch  bool
)

var serverCommand = cobra.Command{
	Use:   "server",
	Short: "Run v2box sing-box backend",
	RunE:  serverHandle,
	Args:  cobra.NoArgs,
}

func init() {
	serverCommand.PersistentFlags().
		StringVarP(&config, "config", "c",
			"/etc/v2box/config.json", "config file path")
	serverCommand.PersistentFlags().
		BoolVarP(&watch, "watch", "w",
			true, "watch file path change")
	command.AddCommand(&serverCommand)
}

func serverHandle(_ *cobra.Command, _ []string) error {
	reloadSignals := make(chan os.Signal, 1)
	signal.Notify(reloadSignals, syscall.SIGHUP)
	defer signal.Stop(reloadSignals)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	stopWatchdog := lifecycleWatchdog(ctx, 30*time.Second, func() { log.Error("Shutdown exceeded grace period; exiting for service-manager recovery"); os.Exit(1) })
	defer stopWatchdog()
	showVersion()
	c := conf.New()
	err := c.LoadFromPath(config)
	log.SetFormatter(&log.TextFormatter{
		DisableTimestamp: true,
		DisableQuote:     true,
		PadLevelText:     false,
	})
	if err != nil {
		log.WithField("err", err).Error("Load config file failed")
		return err
	}
	switch c.LogConfig.Level {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	case "warn", "warning":
		log.SetLevel(log.WarnLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	}
	if c.LogConfig.Output != "" {
		f, err := os.OpenFile(c.LogConfig.Output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.WithField("err", err).Error("Open log file failed, using stdout instead")
		}
		if err == nil {
			log.SetOutput(f)
		}
	}
	// Enable pprof if configured
	if c.PprofPort != 0 {
		go func() {
			log.Infof("Starting pprof server on :%d", c.PprofPort)
			if err := http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", c.PprofPort), nil); err != nil {
				log.WithField("err", err).Error("pprof server failed")
			}
		}()
	}
	//init limiter
	limiter.Init()
	group := newNodeGroup(ctx)
	defer group.Close()
	if err := group.Apply(c); err != nil {
		return err
	}
	reloadCh := make(chan struct{}, 1)
	log.Info("Node supervisors started; each configured node connects independently")
	if watch {
		// On file change, just signal reload; do not run reload concurrently here
		stopWatch, watchErr := c.Watch(config, func() {
			select {
			case reloadCh <- struct{}{}:
			default: // drop if a reload is already queued
			}
		})
		if watchErr != nil {
			log.WithField("err", watchErr).Error("start watch failed")
			return watchErr
		}
		defer stopWatch()
	}
	// clear memory
	runtime.GC()

	for {
		select {
		case <-reloadSignals:
			select {
			case reloadCh <- struct{}{}:
			default:
			}
		case <-ctx.Done():
			return nil
		case <-reloadCh:
			next := conf.New()
			if err := next.LoadFromPath(config); err != nil {
				log.WithError(err).Error("Local configuration rejected; current nodes retained")
				continue
			}
			if err := group.Apply(next); err != nil {
				log.WithError(err).Error("Local configuration rejected; current nodes retained")
				continue
			}
			// Logging is process-wide, while node changes are applied by their owners.
			applyRuntimeLogging(next.LogConfig)
		}
	}

}

type reloadRejected struct{ err error }

func (e *reloadRejected) Error() string { return e.err.Error() }
func (e *reloadRejected) Unwrap() error { return e.err }

// Each owner passes exactly one node; legacy callers can still replace a group.
func reloadConfiguration(ctx context.Context, newConf *conf.Conf, nodes **node.Node, v2core **core.V2Core, prepare ...func(*node.Node) (func(bool), error)) error {
	oldReloadCh := (*v2core).ReloadCh
	newNodes, err := node.NewContext(ctx, newConf.NodeConfigs)
	if err != nil {
		return &reloadRejected{err}
	}
	defer func() {
		if *nodes != newNodes {
			_ = newNodes.Close()
		}
	}()
	for _, reserve := range prepare {
		finish, err := reserve(newNodes)
		if err != nil {
			return &reloadRejected{err}
		}
		defer func() {
			if *nodes != newNodes {
				_ = newNodes.Close()
			}
			finish(*nodes == newNodes)
		}()
	}
	if reflect.DeepEqual(newConf, (*v2core).Config) && ((*nodes).Unchanged(newNodes) || (*nodes).ConfirmPublication(newNodes)) {
		return nil
	}
	newCore := core.New(newConf)
	newCore.InheritSharedListeners(*v2core)
	defer func() {
		if *v2core != newCore {
			_ = newCore.Close()
		}
	}()
	if err := newCore.Start(newNodes.NodeInfos); err != nil {
		newNodes.ReportConfigurationContext(ctx, "core")
		return &reloadRejected{err}
	}
	if err := newNodes.PrepareUsersContext(ctx, newCore); err != nil {
		newNodes.ReportConfigurationContext(ctx, "users")
		return &reloadRejected{err}
	}
	if err := newNodes.PrepareCertificatesContext(ctx); err != nil {
		newNodes.ReportConfigurationContext(ctx, "certificate")
		return &reloadRejected{err}
	}
	if err := ctx.Err(); err != nil {
		return &reloadRejected{err}
	}
	// Validate before stopping live listeners. Parsing failures and panel outages
	// must not interrupt the currently applied configuration.
	oldNodes := (*nodes).Snapshot()
	oldCore := *v2core
	stopErr := errors.Join((*nodes).Close(), (*v2core).Close())
	newCore.PreserveAccountingFrom(oldCore)

	rollback := func(cause error) error {
		// Recovery has its own deadline; a spent candidate deadline must not prevent restoration.
		recovery, cancelRecovery := context.WithTimeout(context.Background(), time.Minute)
		defer cancelRecovery()
		_ = newNodes.Close()
		_ = newCore.Close()
		restored := core.New(oldCore.Config)
		restored.ReloadCh = oldReloadCh
		restored.PreserveAccountingFrom(newCore)
		if err := restored.Start(oldNodes.NodeInfos); err != nil {
			return fmt.Errorf("reload failed: %v; rollback preparation: %w", cause, err)
		}
		if err := oldNodes.RestoreContext(recovery, restored); err != nil {
			*nodes, *v2core = oldNodes, restored
			_ = oldNodes.Close()
			_ = restored.Close()
			return fmt.Errorf("reload failed: %v; rollback start: %w", cause, err)
		}
		*nodes = oldNodes
		*v2core = restored
		return &reloadRejected{cause}
	}

	if stopErr != nil {
		newNodes.ReportConfigurationContext(ctx, "start")
		return rollback(stopErr)
	}

	newCore.PreserveAccountingFrom(*v2core)
	// Reattach reload channel
	newCore.ReloadCh = oldReloadCh
	if err := newCore.Start(newNodes.NodeInfos); err != nil {
		newNodes.ReportConfigurationContext(ctx, "core")
		return rollback(err)
	}

	if err := newNodes.StartContext(ctx, newConf.NodeConfigs, newCore); err != nil {
		newNodes.ReportConfigurationContext(ctx, "start")
		return rollback(err)
	}

	if err := newNodes.ReportConfigurationContext(ctx, ""); err != nil {
		return rollback(err)
	}
	*nodes = newNodes
	*v2core = newCore

	runtime.GC()
	return nil
}

func applyRuntimeLogging(config conf.LogConfig) {
	if level, err := log.ParseLevel(config.Level); err == nil {
		log.SetLevel(level)
	}
	var next *os.File
	if config.Output == "" {
		next = os.Stdout
	} else {
		file, err := os.OpenFile(config.Output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.WithError(err).Error("Open new log output failed; current output retained")
			return
		}
		next = file
	}
	previous := log.StandardLogger().Out
	log.SetOutput(next)
	if old, ok := previous.(*os.File); ok && old != os.Stdout && old != os.Stderr && old != next {
		_ = old.Close()
	}
}
