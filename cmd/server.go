package cmd

import (
	"errors"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"syscall"

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
	//get node info
	nodes, err := node.New(c.NodeConfigs)
	if err != nil {
		log.WithField("err", err).Error("Get node info failed")
		return err
	}
	log.Info("Got nodes info from server")
	//core
	var reloadCh = make(chan struct{}, 1)
	v2core := core.New(c)
	v2core.ReloadCh = reloadCh
	err = v2core.Start(nodes.NodeInfos)
	if err != nil {
		nodes.ReportConfiguration("core")
		log.WithField("err", err).Error("Start core failed")
		return err
	}
	defer func() { _ = nodes.Close(); _ = v2core.Close() }()
	//node
	err = nodes.Start(c.NodeConfigs, v2core)
	if err != nil {
		nodes.ReportConfiguration("start")
		log.WithField("err", err).Error("Run nodes failed")
		return err
	}
	if err := nodes.ReportConfiguration(""); err != nil {
		return err
	}
	log.Info("Nodes started")
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

	osSignals := make(chan os.Signal, 1)
	signal.Notify(osSignals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(osSignals)

	for {
		select {
		case <-osSignals:
			log.Info("收到退出信号，正在关闭程序...")
			return nil
		case <-reloadCh:
			log.Info("收到重启信号，正在重新加载配置...")
			if err := reload(config, &nodes, &v2core); err != nil {
				var rejected *reloadRejected
				if errors.As(err, &rejected) {
					log.WithError(err).Error("Configuration rejected; existing nodes retained")
					continue
				}
				return fmt.Errorf("reload failed: %w", err)
			}
			log.Info("重启成功")
		}
	}
}

type reloadRejected struct{ err error }

func (e *reloadRejected) Error() string { return e.err.Error() }
func (e *reloadRejected) Unwrap() error { return e.err }

func reload(config string, nodes **node.Node, v2core **core.V2Core) error {
	// Preserve old reload channel so new core continues to receive signals
	var oldReloadCh chan struct{}
	if *v2core != nil {
		oldReloadCh = (*v2core).ReloadCh
	}

	newConf := conf.New()
	if err := newConf.LoadFromPath(config); err != nil {
		return &reloadRejected{err}
	}
	newNodes, err := node.New(newConf.NodeConfigs)
	if err != nil {
		return &reloadRejected{err}
	}
	if reflect.DeepEqual(newConf, (*v2core).Config) && (*nodes).ConfirmPublication(newNodes) {
		return nil
	}
	newCore := core.New(newConf)
	if err := newCore.Start(newNodes.NodeInfos); err != nil {
		newNodes.ReportConfiguration("core")
		return &reloadRejected{err}
	}
	if err := newNodes.PrepareUsers(newCore); err != nil {
		newNodes.ReportConfiguration("users")
		return &reloadRejected{err}
	}
	if err := newNodes.PrepareCertificates(); err != nil {
		newNodes.ReportConfiguration("certificate")
		return &reloadRejected{err}
	}
	// Validate before stopping live listeners. Parsing failures and panel outages
	// must not interrupt the currently applied configuration.
	oldNodes := (*nodes).Snapshot()
	oldCore := *v2core
	stopErr := errors.Join((*nodes).Close(), (*v2core).Close())
	newCore.PreserveAccountingFrom(oldCore)

	rollback := func(cause error) error {
		_ = newNodes.Close()
		_ = newCore.Close()
		restored := core.New(oldCore.Config)
		restored.ReloadCh = oldReloadCh
		restored.PreserveAccountingFrom(newCore)
		if err := restored.Start(oldNodes.NodeInfos); err != nil {
			return fmt.Errorf("reload failed: %v; rollback preparation: %w", cause, err)
		}
		if err := oldNodes.Restore(restored); err != nil {
			_ = oldNodes.Close()
			_ = restored.Close()
			return fmt.Errorf("reload failed: %v; rollback start: %w", cause, err)
		}
		*nodes = oldNodes
		*v2core = restored
		return &reloadRejected{cause}
	}

	if stopErr != nil {
		newNodes.ReportConfiguration("start")
		return rollback(stopErr)
	}

	switch newConf.LogConfig.Level {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	case "warn", "warning":
		log.SetLevel(log.WarnLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	}
	if newConf.LogConfig.Output != "" {
		f, err := os.OpenFile(newConf.LogConfig.Output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.WithField("err", err).Error("Open log file failed, using stdout instead")
		} else {
			// 关闭旧的日志文件（如果是文件）
			if oldWriter, ok := log.StandardLogger().Out.(*os.File); ok && oldWriter != os.Stdout && oldWriter != os.Stderr {
				oldWriter.Close()
			}
			if err == nil {
				log.SetOutput(f)
			}
		}
	}

	newCore.PreserveAccountingFrom(*v2core)
	// Reattach reload channel
	newCore.ReloadCh = oldReloadCh
	if err := newCore.Start(newNodes.NodeInfos); err != nil {
		newNodes.ReportConfiguration("core")
		return rollback(err)
	}

	if err := newNodes.Start(newConf.NodeConfigs, newCore); err != nil {
		newNodes.ReportConfiguration("start")
		return rollback(err)
	}

	if err := newNodes.ReportConfiguration(""); err != nil {
		return rollback(err)
	}
	*nodes = newNodes
	*v2core = newCore

	runtime.GC()
	return nil
}
