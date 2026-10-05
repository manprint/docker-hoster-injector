// Package agent runs the whole lifecycle of the injector: connect to Docker,
// bring the hosts file to a known state, keep it in sync, and, whatever way the
// run ends, take its records out of the file again.
//
// The last part is the reason the lifecycle lives here rather than in main. A
// record that nothing keeps up to date will sooner or later name an address
// that now belongs to another container, so the managed block must not outlive
// the process that maintains it. That has to hold for every exit the process
// can still influence: a signal, a fatal error at start-up, a panic.
// What no process can influence, SIGKILL or a power cut, is covered by the
// next start, which repairs and rebuilds the block (see hostsfile.Writer), and
// by the "clean" command.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/mint/docker-hoster-injector/internal/apply"
	"github.com/mint/docker-hoster-injector/internal/config"
	"github.com/mint/docker-hoster-injector/internal/dockerclient"
	"github.com/mint/docker-hoster-injector/internal/hostsfile"
	"github.com/mint/docker-hoster-injector/internal/reconcile"
	"github.com/mint/docker-hoster-injector/internal/watcher"
	"github.com/mint/docker-hoster-injector/internal/webui"
)

// Options are the parts of an Agent that are not configuration.
type Options struct {
	// API is the Docker client. When nil, one is created from the config.
	// Tests pass a fake here.
	API dockerclient.API
	// Version is shown in the log and in the web UI.
	Version string
	// Logger receives everything the agent reports.
	Logger *slog.Logger
}

// Agent is one run of the injector.
type Agent struct {
	cfg  config.Config
	opts Options
	log  *slog.Logger
}

// New returns an Agent. Nothing happens until Run.
func New(cfg config.Config, opts Options) *Agent {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Agent{cfg: cfg, opts: opts, log: log}
}

// cleanupAttempts and cleanupPause bound the effort to remove the block: a
// momentary failure (a busy file, a full disk that frees up) is worth a retry,
// an endless one is not, because the process has to exit.
const (
	cleanupAttempts = 3
	cleanupPause    = 200 * time.Millisecond
)

// Run keeps the hosts file in sync until ctx is done or something fatal
// happens, and removes the managed block before it returns.
//
// It returns nil after a requested stop, and an error for everything else.
func (a *Agent) Run(ctx context.Context) (err error) {
	// The hosts file is opened first, before Docker is even contacted. If
	// Docker turns out to be unreachable, a block left behind by an earlier
	// killed run is then still removed instead of staying there, stale, for as
	// long as the daemon is down.
	writer, err := a.openHostsFile()
	if err != nil {
		return err
	}

	// Registered before anything that can fail or panic, so that it runs for
	// every way out of this function. It is the last thing to run: the
	// pipeline below has stopped by then and nothing can write after it.
	defer func() {
		if r := recover(); r != nil {
			a.log.Error("the agent panicked", "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			err = fmt.Errorf("panic: %v", r)
		}
		a.removeBlock(writer)
	}()

	return a.serve(ctx, writer)
}

// openHostsFile creates the writer and brings the file to a known state.
func (a *Agent) openHostsFile() (*hostsfile.Writer, error) {
	cfg := a.cfg
	writer, err := hostsfile.NewWriter(cfg.HostsFile, cfg.MountMode)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", cfg.HostsFile, err)
	}
	writer.SetOwnedSuffix(cfg.DNSSuffix)

	// Recovery first. A file left damaged by a crash must be repaired before
	// anything is written over it, so the first write lands on a known good
	// baseline.
	if err := writer.Adopt(); err != nil {
		return nil, fmt.Errorf("read %s: %w", cfg.HostsFile, err)
	}
	if repaired, err := writer.Repair(); err != nil {
		// Not fatal: the file may still be usable, and the first reconcile
		// will rebuild the block.
		a.log.Warn("could not repair the hosts file", "error", err, "path", cfg.HostsFile)
	} else if repaired {
		a.log.Warn("repaired a hosts file left inconsistent by a previous run",
			"path", cfg.HostsFile)
	}

	if n, err := writer.CleanStale(); err != nil {
		a.log.Warn("could not remove leftover temporary files", "error", err)
	} else if n > 0 {
		a.log.Warn("removed temporary files left by a previous run", "count", n)
	}
	return writer, nil
}

// removeBlock takes the managed block out of the file. It is safe to call when
// there is no block, and any number of times.
func (a *Agent) removeBlock(w *hostsfile.Writer) {
	var err error
	for i := 0; i < cleanupAttempts; i++ {
		var changed bool
		if changed, err = w.Clear(); err == nil {
			if changed {
				a.log.Info("removed the managed block from the hosts file", "path", a.cfg.HostsFile)
			}
			return
		}
		time.Sleep(cleanupPause)
	}
	a.log.Error("could not remove the managed block, the next start will rebuild it",
		"path", a.cfg.HostsFile, "error", err)
}

// serve connects to Docker and runs the pipeline until ctx is done.
func (a *Agent) serve(ctx context.Context, writer *hostsfile.Writer) error {
	cfg, log := a.cfg, a.log

	api := a.opts.API
	if api == nil {
		c, err := dockerclient.New(ctx, dockerclient.Options{
			Host:       cfg.DockerHost,
			APIVersion: cfg.DockerAPIVersion,
			Logger:     log,
		})
		if err != nil {
			return fmt.Errorf("connect to Docker: %w", err)
		}
		api = c
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

	// A fatal error in any goroutine, a panic included, stops the others
	// through this context, so that Run returns and the block is removed.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		failMu sync.Mutex
		failed error
	)
	fail := func(err error) {
		failMu.Lock()
		if failed == nil {
			failed = err
		}
		failMu.Unlock()
		cancel()
	}
	var wg sync.WaitGroup
	spawn := func(name string, fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					log.Error("a goroutine panicked", "name", name,
						"panic", fmt.Sprint(r), "stack", string(debug.Stack()))
					fail(fmt.Errorf("%s panicked: %v", name, r))
				}
			}()
			fn()
		}()
	}

	// --- web UI ----------------------------------------------------------
	// Started before the first reconcile so the page is reachable immediately.
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
				Version:       a.opts.Version,
				Source:        hostname(),
				DisableEvents: !cfg.Web.Events,
			}, log)
			spawn("web UI", func() {
				if err := web.Serve(runCtx, ln); err != nil {
					log.Warn("the web UI stopped", "error", err)
				}
			})
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
	var eventErrs <-chan error
	spawn("watcher", func() {
		eventErrs = w.Run(runCtx, triggers)
		// Run returns at once with a channel that is closed when the watcher
		// stops; a non-nil error would be a bug and is surfaced.
		if err := <-eventErrs; err != nil && !errors.Is(err, context.Canceled) {
			log.Error("the watcher stopped", "error", err)
			fail(err)
		}
	})
	spawn("applier", func() { applier.Run(runCtx, triggers) })

	<-runCtx.Done()
	if ctx.Err() != nil {
		log.Info("stop requested, waiting for the current write to finish")
	}
	// Every goroutine returns once the context is done, and the applier only
	// after its in-flight write has finished. Waiting here guarantees nothing
	// writes while, or after, the block is removed.
	wg.Wait()

	_, writes, _ := applier.Stats()
	log.Info("pipeline stopped", "writes", writes, "hosts_file", cfg.HostsFile)

	failMu.Lock()
	defer failMu.Unlock()
	return failed
}

// Clean removes the managed block from the configured hosts file and returns.
// It needs neither Docker nor a running agent, which makes it the way out after
// a forced kill: "docker-hoster-injector clean".
func Clean(cfg config.Config, log *slog.Logger) error {
	writer, err := hostsfile.NewWriter(cfg.HostsFile, cfg.MountMode)
	if err != nil {
		return fmt.Errorf("open %s: %w", cfg.HostsFile, err)
	}
	writer.SetOwnedSuffix(cfg.DNSSuffix)
	if err := writer.Adopt(); err != nil {
		return fmt.Errorf("read %s: %w", cfg.HostsFile, err)
	}
	if _, err := writer.CleanStale(); err != nil {
		log.Warn("could not remove leftover temporary files", "error", err)
	}
	changed, err := writer.Clear()
	if err != nil {
		return fmt.Errorf("clean %s: %w", cfg.HostsFile, err)
	}
	if changed {
		log.Info("removed the managed block", "path", cfg.HostsFile)
	} else {
		log.Info("nothing to remove", "path", cfg.HostsFile)
	}
	return nil
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
