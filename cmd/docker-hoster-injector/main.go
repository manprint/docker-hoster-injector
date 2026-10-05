// Command docker-hoster-injector keeps the host's /etc/hosts in sync with the
// running Docker containers, so that every container is reachable under
// <name>.<DNS_SUFFIX> from the host.
//
// It observes the Docker event stream for reactivity and reconciles fully on a
// timer for correctness, because events can be missed and a stream can drop.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mint/docker-hoster-injector/internal/apply"
	"github.com/mint/docker-hoster-injector/internal/config"
	"github.com/mint/docker-hoster-injector/internal/dockerclient"
	"github.com/mint/docker-hoster-injector/internal/hostsfile"
	"github.com/mint/docker-hoster-injector/internal/logging"
	"github.com/mint/docker-hoster-injector/internal/reconcile"
	"github.com/mint/docker-hoster-injector/internal/watcher"
	"github.com/mint/docker-hoster-injector/internal/webui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "docker-hoster-injector: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		// The logger does not exist yet, so this goes straight to stderr.
		// Reporting every bad field at once is the whole point of the joined
		// error: an operator should not have to restart to find the next one.
		return err
	}

	log := logging.New(cfg, logging.Options{Service: "docker-hoster-injector"})

	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("starting", "version", version, "config", cfg.String(), "pid", os.Getpid())

	if err := start(ctx, cfg, log); err != nil {
		log.Error("the agent cannot start", "error", err)
		return err
	}

	log.Info("shutting down")
	return nil
}

// start wires the components and blocks until ctx is done.
func start(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	// --- Docker ----------------------------------------------------------
	api, err := dockerclient.New(ctx, dockerclient.Options{
		Host:       cfg.DockerHost,
		APIVersion: cfg.DockerAPIVersion,
		Logger:     log,
	})
	if err != nil {
		return fmt.Errorf("connect to Docker: %w", err)
	}
	defer func() {
		if err := api.Close(); err != nil {
			log.Warn("closing the Docker client", "error", err)
		}
	}()

	info, err := api.Info(ctx)
	if err != nil {
		return fmt.Errorf("check the Docker daemon: %w", err)
	}
	log.Info("connected to Docker",
		"engine", info.Version,
		"api", info.APIVersion,
		"os", info.OSType,
		"min_api", dockerclient.MinAPIVersion)

	// --- hosts file ------------------------------------------------------
	writer, err := hostsfile.NewWriter(cfg.HostsFile, cfg.MountMode)
	if err != nil {
		return fmt.Errorf("open %s: %w", cfg.HostsFile, err)
	}

	writer.SetOwnedSuffix(cfg.DNSSuffix)

	// Recovery first. A file left damaged by a crash must be repaired before
	// anything is written over it, so the first write lands on a known good
	// baseline.
	if err := writer.Adopt(); err != nil {
		return fmt.Errorf("read %s: %w", cfg.HostsFile, err)
	}
	if repaired, err := writer.Repair(); err != nil {
		// Not fatal: the file may still be usable, and the first reconcile
		// will rebuild the block. Losing the ability to repair is worth
		// reporting but not worth refusing to start over.
		log.Warn("could not repair the hosts file", "error", err, "path", cfg.HostsFile)
	} else if repaired {
		log.Warn("repaired a hosts file left inconsistent by a previous run",
			"path", cfg.HostsFile)
	}

	// --- web UI ----------------------------------------------------------
	var webDone chan struct{}
	// Started before the first reconcile so the page is reachable immediately,
	// even while the hosts file is still being brought up to date.
	var web *webui.Server
	if cfg.Web.Enabled {
		// The monitoring service is a convenience, never a dependency: a
		// failure to bind must not stop the agent from managing the hosts file.
		// The address is bound here, synchronously, so a taken port is reported
		// at once rather than discovered later.
		ln, err := webui.Listen(cfg.Web.Addr)
		if err != nil {
			log.Warn("the web UI could not start, continuing without it",
				"addr", cfg.Web.Addr, "error", err)
		} else {
			web = webui.New(webui.Config{
				DNSSuffix:     cfg.DNSSuffix,
				HostsFile:     cfg.HostsFile,
				TargetMode:    string(cfg.TargetMode),
				MountMode:     string(cfg.MountMode),
				Version:       version,
				Source:        hostname(),
				DisableEvents: !cfg.Web.Events,
			}, log)

			webDone = make(chan struct{})
			go func() {
				defer close(webDone)
				if err := web.Serve(ctx, ln); err != nil {
					log.Warn("the web UI stopped", "error", err)
				}
			}()
		}
	}

	// --- pipeline --------------------------------------------------------
	applier := apply.New(cfg, api, apply.Options{
		Writer:   writer,
		Debounce: cfg.EventDebounce,
		Logger:   log,
		OnChange: func(res reconcile.Result) {
			if web != nil {
				web.Publish(res)
			}
		},
	})

	if web != nil {
		web.SetStatsProvider(func() (uint64, time.Time, error) {
			lastApply, writes, lastErr := applier.Stats()
			return writes, lastApply, lastErr
		})
	}

	w := watcher.New(api, watcher.Options{
		Resync: cfg.ResyncInterval,
		Logger: log,
	})

	triggers := make(chan watcher.Trigger, 16)
	eventErrs := w.Run(ctx, triggers)

	done := make(chan struct{})
	go func() {
		defer close(done)
		applier.Run(ctx, triggers)
	}()

	select {
	case <-ctx.Done():
		log.Info("stop requested, waiting for the current write to finish")
	case err := <-eventErrs:
		// Run only returns when the watcher stops, which happens on
		// cancellation. A non-nil error here would be a bug, so it is
		// surfaced rather than swallowed.
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Error("the watcher stopped", "error", err)
		}
	}

	// The applier returns once ctx is done and its in-flight write has
	// finished, so waiting here guarantees the file is left consistent.
	<-done
	if webDone != nil {
		<-webDone
	}
	log.Info("stopped cleanly",
		"writes", writesOf(applier),
		"hosts_file", cfg.HostsFile)
	return nil
}

// writesOf is a tiny helper so the shutdown log reads the applier's counter
// under its own lock rather than reaching into it.
func writesOf(a *apply.Applier) uint64 {
	_, w, _ := a.Stats()
	return w
}

// hostname identifies which agent is reporting, which matters when several
// machines publish a dashboard for the same suffix.
func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

// version is overridden at build time with -X main.version=<semver>.
var version = "dev"
