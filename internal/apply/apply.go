// Package apply turns reconcile triggers into writes of the hosts file.
//
// It owns the two properties that make concurrent reconciles safe:
//
//   - single flight: exactly one write is in progress at any time, so two
//     triggers can never interleave a read-modify-write;
//   - debouncing: a burst of events produces one write, which keeps
//     "docker compose up" from rewriting the file dozens of times.
package apply

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mint/docker-hoster-injector/internal/config"
	"github.com/mint/docker-hoster-injector/internal/dockerclient"
	"github.com/mint/docker-hoster-injector/internal/hostsfile"
	"github.com/mint/docker-hoster-injector/internal/reconcile"
	"github.com/mint/docker-hoster-injector/internal/watcher"
)

// containerLister is the only part of the Docker client the applier needs. It
// is declared here, in the consuming package, so the applier can be tested with
// a one method fake.
type containerLister interface {
	ListRunning(ctx context.Context) ([]dockerclient.Container, error)
}

// Applier watches triggers and keeps the hosts file up to date.
type Applier struct {
	api        containerLister
	writer     *hostsfile.Writer
	reconciler *reconcile.Reconciler
	debounce   time.Duration
	log        *slog.Logger

	// onChange, when set, receives the latest result. It is how the web UI
	// learns about a new state without ever writing back into this package.
	onChange func(reconcile.Result)

	// mu guards the fields below, which the metrics endpoint reads while the
	// apply loop writes them.
	mu            sync.RWMutex
	lastApply     time.Time
	lastErr       error
	writes        uint64
	lastResult    reconcile.Result
	lastResultSet bool
}

// Options configures an Applier.
type Options struct {
	// Writer manages the hosts file. It is required.
	Writer *hostsfile.Writer
	// Debounce coalesces bursts of triggers. Zero applies immediately.
	Debounce time.Duration
	// Logger receives diagnostics. Nil discards them.
	Logger *slog.Logger
	// OnChange is called after every apply with the resulting state. It is
	// called synchronously on the apply goroutine, so it must not block.
	OnChange func(reconcile.Result)
}

// New returns an Applier.
func New(cfg config.Config, api containerLister, opts Options) *Applier {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	return &Applier{
		api:        api,
		writer:     opts.Writer,
		reconciler: reconcile.New(cfg),
		debounce:   opts.Debounce,
		log:        log,
		onChange:   opts.OnChange,
	}
}

// Run consumes triggers until the channel closes or ctx is cancelled.
//
// Every trigger causes a fresh read of the world, because a trigger says only
// that something happened, never what the new state is.
func (a *Applier) Run(ctx context.Context, triggers <-chan watcher.Trigger) {
	if a.debounce <= 0 {
		a.runImmediate(ctx, triggers)
		return
	}
	a.runDebounced(ctx, triggers)
}

// runImmediate applies each trigger as it arrives.
func (a *Applier) runImmediate(ctx context.Context, triggers <-chan watcher.Trigger) {
	for {
		select {
		case <-ctx.Done():
			return
		case trigger, ok := <-triggers:
			if !ok {
				return
			}
			a.applyOnce(ctx, trigger)
		}
	}
}

// runDebounced collapses a burst of triggers into a single write.
//
// The timer restarts on every trigger, so the write happens once the burst has
// actually finished rather than a fixed window after the first event. That
// matters for "docker compose up", which emits a dozen events over a second or
// two: a fixed window would write repeatedly, a resetting timer writes once.
func (a *Applier) runDebounced(ctx context.Context, triggers <-chan watcher.Trigger) {
	timer := time.NewTimer(a.debounce)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	var (
		pending bool
		last    watcher.Trigger
	)

	for {
		select {
		case <-ctx.Done():
			return

		case trigger, ok := <-triggers:
			if !ok {
				// The producer is gone. Apply what is pending so the file
				// reflects the last observed state.
				if pending {
					a.applyOnce(ctx, last)
				}
				return
			}
			last = trigger
			pending = true
			resetTimer(timer, a.debounce)

		case <-timer.C:
			if pending {
				pending = false
				a.applyOnce(ctx, last)
			}
		}
	}
}

// resetTimer restarts a timer that may or may not have already fired.
func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// applyOnce performs a single reconcile-and-write.
func (a *Applier) applyOnce(ctx context.Context, trigger watcher.Trigger) {
	containers, err := a.api.ListRunning(ctx)
	if err != nil {
		a.fail("list the running containers", err)
		return
	}

	result := a.reconciler.Reconcile(containers)

	for _, c := range result.Conflicts {
		a.log.Warn("two containers want the same name",
			"name", c.Name,
			"winner", c.Winner.Owner,
			"losers", len(c.Losers))
	}
	for _, s := range result.Skipped {
		a.log.Debug("container not published",
			"container", s.ContainerName,
			"state", s.State,
			"reason", string(s.Reason))
	}

	changed, err := a.writer.Apply(result.HostsEntries())
	if err != nil {
		// The write failed. The intended state is still reported so the web
		// UI shows what should be there, and the next trigger retries.
		a.fail("write "+a.writer.Path(), err)
		a.notify(result)
		return
	}

	a.mu.Lock()
	a.lastApply = time.Now()
	a.lastErr = nil
	if changed {
		a.writes++
	}
	a.mu.Unlock()

	if changed {
		a.log.Info("hosts file updated",
			"trigger", string(trigger),
			"records", len(result.Entries),
			"skipped", len(result.Skipped),
			"path", a.writer.Path())
	} else {
		a.log.Debug("hosts file already up to date",
			"trigger", string(trigger),
			"records", len(result.Entries))
	}

	a.notify(result)
}

// notify hands the result to the observer, if any.
//
// The result is also retained, so a web client that connects later has
// something to display immediately instead of waiting for the next change.
func (a *Applier) notify(res reconcile.Result) {
	a.mu.Lock()
	a.lastResult = res
	a.lastResultSet = true
	a.mu.Unlock()

	if a.onChange != nil {
		a.onChange(res)
	}
}

// fail records an error without stopping the agent.
//
// A hosts file that cannot be written, or a daemon that is briefly
// unreachable, must not take the process down: the next trigger or resync tries
// again, and the file keeps whatever content it already had.
func (a *Applier) fail(msg string, err error) {
	a.mu.Lock()
	a.lastErr = err
	a.mu.Unlock()
	a.log.Error(msg, "error", err)
}

// Stats reports what the applier has done, for the metrics endpoint.
func (a *Applier) Stats() (lastApply time.Time, writes uint64, lastErr error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.lastApply, a.writes, a.lastErr
}

// LastResult returns the most recent result published to the observer, so a
// newly connected web client has something to show immediately.
func (a *Applier) LastResult() (reconcile.Result, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.lastResult, a.lastResultSet
}

// discard is an io.Writer that throws everything away.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
