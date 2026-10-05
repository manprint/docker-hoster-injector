package watcher

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mint/docker-hoster-injector/internal/dockerclient"
)

// fakeAPI is a controllable stand-in for the Docker daemon.
type fakeAPI struct {
	mu sync.Mutex

	info    dockerclient.EngineInfo
	infoErr error
	list    []dockerclient.Container
	listErr error
	calls   int

	// events yields one stream per Events call. A nil stream entry makes the
	// call fail, which is how a daemon that is down is simulated.
	events func(call int) (<-chan dockerclient.Event, <-chan error, error)
}

func (f *fakeAPI) Info(context.Context) (dockerclient.EngineInfo, error) {
	return f.info, f.infoErr
}

func (f *fakeAPI) ListRunning(context.Context) ([]dockerclient.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.list, f.listErr
}

func (f *fakeAPI) Events(context.Context) (<-chan dockerclient.Event, <-chan error, error) {
	f.mu.Lock()
	call := f.calls
	f.mu.Unlock()
	return f.events(call)
}

func (f *fakeAPI) Close() error { return nil }

// neverEvents returns a stream that stays open and silent until ctx ends.
func neverEvents() (<-chan dockerclient.Event, <-chan error, error) {
	ev := make(chan dockerclient.Event)
	errs := make(chan error)
	// Nothing is ever sent and nothing is closed: this models a healthy,
	// quiet daemon.
	return ev, errs, nil
}

// collectTriggers runs the watcher and returns the triggers seen.
func collectTriggers(t *testing.T, api dockerclient.API, opts Options, wait time.Duration) []Trigger {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()

	triggers := make(chan Trigger, 64)
	w := New(api, opts)
	w.Run(ctx, triggers)

	var got []Trigger
	drain := time.NewTicker(5 * time.Millisecond)
	defer drain.Stop()

	for {
		select {
		case tr, ok := <-triggers:
			if !ok {
				return got
			}
			got = append(got, tr)
		case <-ctx.Done():
			return got
		case <-drain.C:
		}
	}
}

func TestWatcherAlwaysReconcilesAtStartup(t *testing.T) {
	t.Parallel()

	// The first reconcile must happen before anything else, so the file is
	// correct even if no container ever changes again.
	got := collectTriggers(t, &fakeAPI{events: func(int) (<-chan dockerclient.Event, <-chan error, error) {
		return neverEvents()
	}}, Options{Resync: time.Hour}, 300*time.Millisecond)

	if len(got) == 0 {
		t.Fatal("no trigger was produced at startup")
	}
	if got[0] != TriggerStart {
		t.Errorf("first trigger = %q, want %q", got[0], TriggerStart)
	}
}

func TestWatcherReconcilesOnReconnect(t *testing.T) {
	t.Parallel()

	// A freshly opened stream has a blind spot, so the watcher must reconcile
	// once before trusting it.
	got := collectTriggers(t, &fakeAPI{events: func(int) (<-chan dockerclient.Event, <-chan error, error) {
		return neverEvents()
	}}, Options{}, 300*time.Millisecond)

	if !containsTrigger(got, TriggerReconnect) {
		t.Errorf("triggers = %v, want one %q", got, TriggerReconnect)
	}
}

func TestWatcherResyncFiresRepeatedly(t *testing.T) {
	t.Parallel()

	got := collectTriggers(t, &fakeAPI{events: func(int) (<-chan dockerclient.Event, <-chan error, error) {
		return neverEvents()
	}}, Options{Resync: 40 * time.Millisecond}, 260*time.Millisecond)

	count := 0
	for _, tr := range got {
		if tr == TriggerResync {
			count++
		}
	}
	if count < 3 {
		t.Errorf("got %d resync triggers in 260ms with a 40ms period, want at least 3: %v",
			count, got)
	}
}

func TestWatcherResyncDisabledWhenZero(t *testing.T) {
	t.Parallel()

	got := collectTriggers(t, &fakeAPI{events: func(int) (<-chan dockerclient.Event, <-chan error, error) {
		return neverEvents()
	}}, Options{Resync: 0}, 200*time.Millisecond)

	if containsTrigger(got, TriggerResync) {
		t.Errorf("triggers = %v, want no resync when the period is zero", got)
	}
}

func TestWatcherTriggersOnContainerEvents(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{events: func(int) (<-chan dockerclient.Event, <-chan error, error) {
		ev := make(chan dockerclient.Event, 8)
		errs := make(chan error)
		for _, action := range []string{"start", "die", "destroy", "pause", "rename"} {
			ev <- dockerclient.Event{Type: "container", Action: action}
		}
		return ev, errs, nil
	}}

	got := collectTriggers(t, api, Options{}, 250*time.Millisecond)

	count := 0
	for _, tr := range got {
		if tr == TriggerEvent {
			count++
		}
	}
	if count != 5 {
		t.Errorf("got %d event triggers, want 5: %v", count, got)
	}
}

func TestWatcherIgnoresNonContainerEvents(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{events: func(int) (<-chan dockerclient.Event, <-chan error, error) {
		ev := make(chan dockerclient.Event, 8)
		errs := make(chan error)
		for _, typ := range []string{"image", "volume", "network", "builder"} {
			ev <- dockerclient.Event{Type: typ, Action: "pull"}
		}
		return ev, errs, nil
	}}

	got := collectTriggers(t, api, Options{}, 200*time.Millisecond)

	if containsTrigger(got, TriggerEvent) {
		t.Errorf("triggers = %v, want no event trigger for non-container objects", got)
	}
}

func TestWatcherIgnoresIrrelevantContainerActions(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{events: func(int) (<-chan dockerclient.Event, <-chan error, error) {
		ev := make(chan dockerclient.Event, 8)
		errs := make(chan error)
		for _, action := range []string{"top", "attach", "checkpoint", "prune", "commit"} {
			ev <- dockerclient.Event{Type: "container", Action: action}
		}
		return ev, errs, nil
	}}

	got := collectTriggers(t, api, Options{}, 200*time.Millisecond)

	if containsTrigger(got, TriggerEvent) {
		t.Errorf("triggers = %v, want no trigger for actions that cannot change the hosts file", got)
	}
}

// A stream that dies must not stop the watcher: the daemon restarting is a
// normal event, and the resync keeps the file correct meanwhile.
func TestWatcherSurvivesStreamFailure(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{events: func(call int) (<-chan dockerclient.Event, <-chan error, error) {
		if call == 0 {
			// The stream ends immediately with an error.
			ev := make(chan dockerclient.Event)
			errs := make(chan error, 1)
			close(ev)
			errs <- errors.New("connection reset")
			close(errs)
			return ev, errs, nil
		}
		return neverEvents()
	}}

	got := collectTriggers(t, api, Options{Resync: 40 * time.Millisecond}, 300*time.Millisecond)

	// At least two reconnects, each followed by a reconcile.
	reconnects := 0
	for _, tr := range got {
		if tr == TriggerReconnect {
			reconnects++
		}
	}
	if reconnects < 2 {
		t.Errorf("got %d reconnect triggers, want at least 2: %v", reconnects, got)
	}
}

// A daemon that refuses to open the stream at all must be retried.
func TestWatcherSurvivesEventsOpenFailure(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{events: func(call int) (<-chan dockerclient.Event, <-chan error, error) {
		if call < 3 {
			return nil, nil, errors.New("cannot connect to the Docker daemon socket")
		}
		return neverEvents()
	}}

	got := collectTriggers(t, api, Options{Resync: 40 * time.Millisecond}, 400*time.Millisecond)

	if containsTrigger(got, TriggerResync) == false {
		t.Errorf("triggers = %v, want the resync to keep running while the daemon is unreachable", got)
	}
}

func TestWatcherBackoffGrowsAndIsCapped(t *testing.T) {
	t.Parallel()

	d := minBackoff
	if d != 500*time.Millisecond {
		t.Errorf("minBackoff = %v, want 500ms", d)
	}

	for i := 0; i < 20; i++ {
		next := nextBackoff(d)
		if next < d {
			t.Errorf("backoff shrank from %v to %v", d, next)
		}
		if next > maxBackoff {
			t.Fatalf("backoff %v exceeded the cap %v", next, maxBackoff)
		}
		d = next
	}
	if d != maxBackoff {
		t.Errorf("backoff settled at %v, want the cap %v", d, maxBackoff)
	}
	if maxBackoff != 30*time.Second {
		t.Errorf("maxBackoff = %v, want 30s", maxBackoff)
	}
}

// Run must close its channels so the consumer's range loop terminates.
func TestWatcherClosesChannels(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	api := &fakeAPI{events: func(int) (<-chan dockerclient.Event, <-chan error, error) {
		return neverEvents()
	}}

	triggers := make(chan Trigger, 8)
	errs := New(api, Options{Resync: 20 * time.Millisecond}).Run(ctx, triggers)

	// Drain one trigger so the applier side is not the blocker.
	select {
	case <-triggers:
	case <-time.After(time.Second):
		t.Fatal("no trigger arrived")
	}

	cancel()

	select {
	case _, ok := <-triggers:
		if ok {
			// Another trigger may still be in flight; the channel must close
			// eventually.
			for range triggers {
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the trigger channel was never closed after cancellation")
	}

	select {
	case _, ok := <-errs:
		if ok {
			for range errs {
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the error channel was never closed after cancellation")
	}
}

// A slow consumer must not block the watcher: when the context ends, emitting
// has to give up rather than leak the goroutine.
func TestWatcherEmitDoesNotBlockForever(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	w := New(&fakeAPI{}, Options{})
	out := make(chan Trigger) // unbuffered, and nobody is reading

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.emit(ctx, out, TriggerEvent)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("emit blocked after cancellation, which would leak the goroutine")
	}
}

func containsTrigger(all []Trigger, want Trigger) bool {
	for _, t := range all {
		if t == want {
			return true
		}
	}
	return false
}
