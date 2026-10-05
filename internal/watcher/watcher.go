// Package watcher decides when the hosts file needs to be recomputed.
//
// It merges two sources on purpose:
//
//   - the Docker event stream, which is reactive but best effort: events can be
//     dropped, and a disconnect loses whatever happened while it was down;
//   - a periodic full resync, which is slow but never misses anything.
//
// The resync is what provides correctness. The event stream only provides
// latency, and the agent treats every event as a hint to re-read the world
// rather than as a fact to apply.
package watcher

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/mint/docker-hoster-injector/internal/dockerclient"
)

// backoff bounds the reconnect delay after a failed stream.
const (
	minBackoff = 500 * time.Millisecond
	maxBackoff = 30 * time.Second
)

// Trigger explains why a reconcile was requested.
type Trigger string

const (
	// TriggerStart is the initial reconcile at startup.
	TriggerStart Trigger = "startup"
	// TriggerEvent is a hint from the Docker event stream.
	TriggerEvent Trigger = "event"
	// TriggerResync is the periodic full reconcile.
	TriggerResync Trigger = "resync"
	// TriggerReconnect is a reconcile right after the event stream was
	// re-established, which closes the window where events were missed.
	TriggerReconnect Trigger = "reconnect"
)

// Watcher produces reconcile triggers.
type Watcher struct {
	api           dockerclient.API
	resync        time.Duration
	log           *slog.Logger
	minResyncGap  time.Duration
	mu            sync.Mutex
	lastTriggerAt time.Time
}

// Options configures a Watcher.
type Options struct {
	// Resync is the period of the full reconcile. Zero disables it, which is
	// only appropriate in tests.
	Resync time.Duration
	// Logger receives diagnostics. Nil discards them.
	Logger *slog.Logger
}

// New returns a Watcher over the given API.
func New(api dockerclient.API, opts Options) *Watcher {
	return &Watcher{
		api:    api,
		resync: opts.Resync,
		log:    opts.Logger,
	}
}

// Run drives reconcile triggers to the returned channel until ctx is
// cancelled. The channel is closed on return, so the consumer ranges over it.
//
// Every trigger is a request to re-read the world. The watcher never reports
// what changed, because it cannot know: the Docker event stream does not carry
// the new container state, only that something happened.
func (w *Watcher) Run(ctx context.Context, out chan<- Trigger) <-chan error {
	errs := make(chan error, 1)
	go func() {
		defer close(out)
		defer close(errs)

		// The very first reconcile must happen before any event is processed,
		// so the file is correct even if nothing ever happens again.
		w.emit(ctx, out, TriggerStart)

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.runEvents(ctx, out)
		}()

		if w.resync > 0 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				w.runResync(ctx, out)
			}()
		}

		wg.Wait()
	}()
	return errs
}

// runEvents consumes the event stream, reconnecting for as long as ctx lives.
//
// A failure here is logged and retried rather than propagated: the daemon
// being briefly unreachable must not stop the agent, and the resync loop keeps
// the file correct in the meantime.
func (w *Watcher) runEvents(ctx context.Context, out chan<- Trigger) {
	backoff := minBackoff

	for ctx.Err() == nil {
		events, errs, err := w.api.Events(ctx)
		if err != nil {
			w.logEventError(ctx, "open the event stream", err)
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		// A freshly established stream has a blind spot: whatever happened
		// while it was disconnected was never delivered. Reconcile once, then
		// start trusting it again.
		w.logDebug("event stream established, reconciling to close the gap")
		w.emit(ctx, out, TriggerReconnect)

		backoff = minBackoff
		w.consume(ctx, events, errs, out)
	}
}

// consume reads one stream to exhaustion.
func (w *Watcher) consume(
	ctx context.Context,
	events <-chan dockerclient.Event,
	errs <-chan error,
	out chan<- Trigger,
) {
	for {
		select {
		case <-ctx.Done():
			return

		case err, ok := <-errs:
			if !ok || err == nil {
				// The error channel closing means the stream ended.
				w.logDebug("the event stream ended")
				return
			}
			w.logEventError(ctx, "the event stream failed", err)
			return

		case msg, ok := <-events:
			if !ok {
				w.logDebug("the event stream closed")
				return
			}
			w.onEvent(ctx, msg, out)
		}
	}
}

// onEvent decides whether a single event is worth a reconcile.
//
// Everything is coalesced by the applier, so the cheap test here is only about
// avoiding pointless work. Events for other object classes cannot change which
// containers are running, but they reach this loop when the daemon applies the
// type filter loosely, so they are ignored explicitly rather than assumed away.
func (w *Watcher) onEvent(ctx context.Context, ev dockerclient.Event, out chan<- Trigger) {
	if ev.Type != "container" {
		return
	}

	switch ev.Action {
	case "create", "start", "die", "kill", "stop", "destroy", "remove",
		"rename", "pause", "unpause", "restart", "connect", "disconnect",
		"health_status", "oom", "exec_create":
		w.logDebug("reconcile triggered by a container event",
			"action", ev.Action,
			"container", ev.ContainerName)
		w.emit(ctx, out, TriggerEvent)
	default:
		w.logDebug("ignoring an event with no effect on the hosts file",
			"action", ev.Action)
	}
}

// runResync triggers a full reconcile on a fixed period.
func (w *Watcher) runResync(ctx context.Context, out chan<- Trigger) {
	ticker := time.NewTicker(w.resync)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.emit(ctx, out, TriggerResync)
		}
	}
}

// emit delivers a trigger without blocking shutdown.
//
// A send that would block means the consumer has stopped, which happens exactly
// when the context is cancelled. Returning quietly is correct; blocking would
// leak the goroutine.
func (w *Watcher) emit(ctx context.Context, out chan<- Trigger, t Trigger) {
	select {
	case out <- t:
	case <-ctx.Done():
	}
}

func (w *Watcher) logDebug(msg string, args ...any) {
	if w.log != nil {
		w.log.Debug(msg, args...)
	}
}

func (w *Watcher) logEventError(ctx context.Context, msg string, err error) {
	if w.log == nil {
		return
	}
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return // an expected shutdown, not a fault
	}
	w.log.Warn(msg, "error", err)
}

// sleepCtx waits for d, returning false if the context ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// nextBackoff grows the delay geometrically up to the ceiling, so a daemon that
// is down for a while is not hammered, while a brief blip still reconnects fast.
func nextBackoff(d time.Duration) time.Duration {
	next := d * 2
	if next > maxBackoff {
		return maxBackoff
	}
	return next
}

// BackoffForTest exposes the backoff progression so the policy can be tested
// without waiting for it.
func BackoffForTest(d time.Duration) time.Duration { return nextBackoff(d) }
