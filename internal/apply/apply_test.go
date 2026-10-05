package apply

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manprint/docker-hoster-injector/internal/config"
	"github.com/manprint/docker-hoster-injector/internal/dockerclient"
	"github.com/manprint/docker-hoster-injector/internal/hostsfile"
	"github.com/manprint/docker-hoster-injector/internal/reconcile"
	"github.com/manprint/docker-hoster-injector/internal/watcher"
)

// fakeAPI returns a scripted container list.
type fakeAPI struct {
	mu sync.Mutex
	// list is guarded, because the applier may read it from another
	// goroutine than the test.
	list    []dockerclient.Container
	listErr error
	calls   int
	// block, when set, makes ListRunning wait until it is closed.
	block chan struct{}
}

func (f *fakeAPI) Info(context.Context) (dockerclient.EngineInfo, error) {
	return dockerclient.EngineInfo{}, nil
}

func (f *fakeAPI) ListRunning(ctx context.Context) ([]dockerclient.Container, error) {
	f.mu.Lock()
	f.calls++
	block := f.block
	list, err := f.list, f.listErr
	f.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return list, err
}

func (f *fakeAPI) Events(context.Context) (<-chan dockerclient.Event, <-chan error, error) {
	ev := make(chan dockerclient.Event)
	errs := make(chan error)
	return ev, errs, nil
}

func (f *fakeAPI) Close() error { return nil }

func (f *fakeAPI) set(list []dockerclient.Container) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.list = list
}

func (f *fakeAPI) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listErr = err
}

func (f *fakeAPI) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// harness wires an applier over a temporary hosts file.
type harness struct {
	api     *fakeAPI
	writer  *hostsfile.Writer
	applier *Applier
	path    string
}

func newHarness(t *testing.T, debounce time.Duration) *harness {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")
	if err := os.WriteFile(path, []byte("127.0.0.1 localhost\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w, err := hostsfile.NewWriter(path, config.MountModeFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Adopt(); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadFrom(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}

	api := &fakeAPI{}
	a := New(cfg, api, Options{Writer: w, Debounce: debounce})
	return &harness{api: api, writer: w, applier: a, path: path}
}

func (h *harness) container(name, ip string, hostPort int) dockerclient.Container {
	c := dockerclient.Container{
		ID: "id-" + name, Name: name, State: "running",
		Networks:    []dockerclient.Network{{Name: "bridge", IPv4: ip}},
		NetworkMode: "bridge",
	}
	if hostPort > 0 {
		c.PublishedPorts = []dockerclient.Port{{
			HostIP: "", HostPort: hostPort, ContainerPort: 80, Protocol: "tcp",
		}}
	}
	return c
}

func (h *harness) hosts() string {
	data, err := os.ReadFile(h.path)
	if err != nil {
		h.applier.log.Error("read", "error", err)
		return ""
	}
	return string(data)
}

// run drives the applier until the triggers are exhausted.
func (h *harness) run(t *testing.T, triggers <-chan watcher.Trigger) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.applier.Run(context.Background(), triggers)
	}()
	<-done
}

func TestApplierWritesTheReconciledState(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 0)
	h.api.set([]dockerclient.Container{h.container("nginx", "172.17.0.2", 8080)})

	triggers := make(chan watcher.Trigger, 1)
	triggers <- watcher.TriggerStart
	close(triggers)
	h.run(t, triggers)

	got := h.hosts()
	if !contains(got, "nginx.docker.local") {
		t.Errorf("the record is missing:\n%s", got)
	}
	if !contains(h.hosts(), "127.0.0.1 localhost") {
		t.Errorf("the operator's entry was lost:\n%s", got)
	}
}

func TestApplierRemovesRecordsWhenContainersGo(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 0)
	h.api.set([]dockerclient.Container{h.container("nginx", "172.17.0.2", 8080)})

	triggers := make(chan watcher.Trigger, 1)
	triggers <- watcher.TriggerStart
	close(triggers)
	h.run(t, triggers)

	if !contains(h.hosts(), "nginx.docker.local") {
		t.Fatal("setup failed: the record was never written")
	}

	// The container goes away and a new trigger arrives.
	h.api.set(nil)
	triggers2 := make(chan watcher.Trigger, 1)
	triggers2 <- watcher.TriggerResync
	close(triggers2)
	h.run(t, triggers2)

	if contains(h.hosts(), "nginx.docker.local") {
		t.Errorf("the record survived the container:\n%s", h.hosts())
	}
}

// The debounce is the difference between one write for a compose up and
// dozens.
func TestApplierDebouncesABurst(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 120*time.Millisecond)
	h.api.set([]dockerclient.Container{h.container("nginx", "172.17.0.2", 8080)})

	triggers := make(chan watcher.Trigger, 64)
	for i := 0; i < 40; i++ {
		triggers <- watcher.TriggerEvent
	}
	close(triggers)

	h.run(t, triggers)

	if got := h.api.callCount(); got != 1 {
		t.Errorf("the burst caused %d reconciles, want exactly 1", got)
	}
}

// A burst that keeps arriving must keep postponing the write, so the file
// lands once with the final state.
func TestApplierDebouncePostponesWhileTriggersKeepArriving(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 150*time.Millisecond)
	h.api.set([]dockerclient.Container{h.container("nginx", "172.17.0.2", 8080)})

	triggers := make(chan watcher.Trigger)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.applier.Run(context.Background(), triggers)
	}()

	// Five triggers spread over less than the debounce window each.
	for i := 0; i < 5; i++ {
		triggers <- watcher.TriggerEvent
		time.Sleep(40 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	close(triggers)
	<-done

	if got := h.api.callCount(); got != 1 {
		t.Errorf("got %d reconciles for a spread burst, want 1: the timer must "+
			"restart on every trigger", got)
	}
}

// Closing the trigger channel with work pending must still flush it, or the
// last state would be lost when the watcher stops.
func TestApplierFlushesPendingWorkOnShutdown(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 200*time.Millisecond)
	h.api.set([]dockerclient.Container{h.container("nginx", "172.17.0.2", 8080)})

	triggers := make(chan watcher.Trigger, 1)
	triggers <- watcher.TriggerEvent

	// Closing immediately means Run has pending work but no more triggers.
	close(triggers)
	h.run(t, triggers)

	if !contains(h.hosts(), "nginx.docker.local") {
		t.Errorf("pending work was lost on shutdown:\n%s", h.hosts())
	}
}

func TestApplierSurvivesAListError(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 0)
	h.api.setErr(errors.New("cannot connect to the Docker daemon"))

	triggers := make(chan watcher.Trigger, 1)
	triggers <- watcher.TriggerStart
	close(triggers)

	// The important property: Run returns rather than dying.
	h.run(t, triggers)

	_, writes, lastErr := h.applier.Stats()
	if lastErr == nil {
		t.Error("the error was not recorded")
	}
	if writes != 0 {
		t.Errorf("writes = %d after a failed reconcile, want 0", writes)
	}
	if contains(h.hosts(), "nginx.docker.local") {
		t.Error("a record was written despite the failure")
	}
}

// A failure must not stop the agent: the next trigger has to be able to write.
func TestApplierRecoversAfterAnError(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 0)

	h.api.setErr(errors.New("transient"))

	triggers := make(chan watcher.Trigger, 1)
	triggers <- watcher.TriggerStart
	close(triggers)
	h.run(t, triggers)

	// set takes the lock itself; taking it here as well would deadlock.
	h.api.setErr(nil)
	h.api.set([]dockerclient.Container{h.container("nginx", "172.17.0.2", 8080)})

	triggers2 := make(chan watcher.Trigger, 1)
	triggers2 <- watcher.TriggerResync
	close(triggers2)
	h.run(t, triggers2)

	if !contains(h.hosts(), "nginx.docker.local") {
		t.Errorf("the applier did not recover:\n%s", h.hosts())
	}
	_, _, lastErr := h.applier.Stats()
	if lastErr != nil {
		t.Errorf("the last error was not cleared: %v", lastErr)
	}
}

func TestApplierCountsOnlyRealWrites(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 0)
	h.api.set([]dockerclient.Container{h.container("nginx", "172.17.0.2", 8080)})

	for range 5 {
		triggers := make(chan watcher.Trigger, 1)
		triggers <- watcher.TriggerResync
		close(triggers)
		h.run(t, triggers)
	}

	_, writes, _ := h.applier.Stats()
	if writes != 1 {
		t.Errorf("writes = %d for five identical reconciles, want 1: an unchanged "+
			"state must not touch the file", writes)
	}
}

func TestApplierNotifiesObserver(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")
	if err := os.WriteFile(path, []byte("127.0.0.1 localhost\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := hostsfile.NewWriter(path, config.MountModeFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Adopt(); err != nil {
		t.Fatal(err)
	}

	api := &fakeAPI{list: []dockerclient.Container{
		{ID: "id1", Name: "nginx", State: "running", NetworkMode: "bridge",
			Networks: []dockerclient.Network{{Name: "bridge", IPv4: "172.17.0.2"}}},
	}}

	updates := make(chan int, 4)
	a := New(mustConfig(t), api, Options{
		Writer: w,
		OnChange: func(res reconcile.Result) {
			updates <- len(res.Entries)
		},
	})

	triggers := make(chan watcher.Trigger, 1)
	triggers <- watcher.TriggerStart
	close(triggers)
	a.Run(context.Background(), triggers)

	select {
	case n := <-updates:
		if n == 0 {
			t.Error("the observer was told about an empty state")
		}
	case <-time.After(time.Second):
		t.Fatal("the observer was never called")
	}
}

func TestApplierRetainsTheLastResult(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 0)
	h.api.set([]dockerclient.Container{h.container("nginx", "172.17.0.2", 8080)})

	triggers := make(chan watcher.Trigger, 1)
	triggers <- watcher.TriggerStart
	close(triggers)
	h.run(t, triggers)

	res, ok := h.applier.LastResult()
	if !ok {
		t.Fatal("no result was retained")
	}
	if len(res.Entries) == 0 {
		t.Error("the retained result has no entries")
	}
}

func TestApplierStatsAreRaceFree(t *testing.T) {
	t.Parallel()

	h := newHarness(t, 10*time.Millisecond)
	h.api.set([]dockerclient.Container{h.container("nginx", "172.17.0.2", 8080)})

	triggers := make(chan watcher.Trigger)
	go h.applier.Run(context.Background(), triggers)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					h.applier.Stats()
					h.applier.LastResult()
					h.hosts()
				}
			}
		}()
	}

	for range 30 {
		triggers <- watcher.TriggerEvent
		time.Sleep(15 * time.Millisecond)
	}
	close(triggers)
	close(stop)
	wg.Wait()
}

func mustConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.LoadFrom(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
