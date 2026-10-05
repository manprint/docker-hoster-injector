package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/manprint/docker-hoster-injector/internal/config"
	"github.com/manprint/docker-hoster-injector/internal/dockerclient"
	"github.com/manprint/docker-hoster-injector/internal/hostsfile"
)

const operatorHosts = "127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost\n\n# mine\n10.9.9.9 intranet\n"

type fakeDocker struct {
	mu        sync.Mutex
	list      []dockerclient.Container
	infoErr   error
	panicList bool
	closed    int
}

func (f *fakeDocker) Info(context.Context) (dockerclient.EngineInfo, error) {
	return dockerclient.EngineInfo{Version: "test"}, f.infoErr
}

func (f *fakeDocker) ListRunning(context.Context) ([]dockerclient.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panicList {
		panic("boom")
	}
	return f.list, nil
}

func (f *fakeDocker) Events(ctx context.Context) (<-chan dockerclient.Event, <-chan error, error) {
	ev := make(chan dockerclient.Event)
	errs := make(chan error)
	go func() {
		<-ctx.Done()
		close(ev)
		close(errs)
	}()
	return ev, errs, nil
}

func (f *fakeDocker) Close() error {
	f.mu.Lock()
	f.closed++
	f.mu.Unlock()
	return nil
}

func web(name string) dockerclient.Container {
	return dockerclient.Container{
		ID: name + "-id", Name: name, State: "running", NetworkMode: "bridge",
		Networks: []dockerclient.Network{{Name: "bridge", IPv4: "172.17.0.2"}},
	}
}

func testConfig(t *testing.T, mode config.MountMode) (config.Config, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(path, []byte(operatorHosts), 0o644); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"HOSTS_FILE":       path,
		"HOSTS_MOUNT_MODE": string(mode),
		"WEB_ENABLED":      "false",
		"EVENT_DEBOUNCE":   "0s",
		"RESYNC_INTERVAL":  "1h",
	}
	cfg, err := config.LoadFrom(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg, path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestStopRemovesTheBlockAndRestoresTheOperatorsFile(t *testing.T) {
	t.Parallel()
	for _, mode := range []config.MountMode{config.MountModeFile, config.MountModeDir} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			cfg, path := testConfig(t, mode)
			api := &fakeDocker{list: []dockerclient.Container{web("web")}}

			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- New(cfg, Options{API: api}).Run(ctx) }()

			waitFor(t, "the block to appear", func() bool {
				return strings.Contains(readFile(t, path), "web.docker.local")
			})
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("Run after a stop = %v, want nil", err)
			}
			if got := readFile(t, path); got != operatorHosts {
				t.Fatalf("the hosts file is not back to the operator's content:\n%s", got)
			}
			if api.closed != 1 {
				t.Errorf("Docker client closed %d times, want 1", api.closed)
			}
		})
	}
}

// A block left by a killed run is converged on by the next one, with no
// duplicate and nothing spurious, and removed again on stop.
func TestRestartAfterAForcedKillLeavesNoLeftovers(t *testing.T) {
	t.Parallel()
	for _, mode := range []config.MountMode{config.MountModeFile, config.MountModeDir} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			cfg, path := testConfig(t, mode)

			// What a killed run leaves: a complete block for a container that
			// is gone, and a partial one after it.
			stale := operatorHosts + hostsfile.BeginMarker + "\n172.17.0.9\tgone.docker.local\n" + hostsfile.EndMarker + "\n"
			if err := os.WriteFile(path, []byte(stale), 0o644); err != nil {
				t.Fatal(err)
			}

			api := &fakeDocker{list: []dockerclient.Container{web("web")}}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- New(cfg, Options{API: api}).Run(ctx) }()

			waitFor(t, "convergence", func() bool {
				s := readFile(t, path)
				return strings.Contains(s, "web.docker.local") && !strings.Contains(s, "gone.docker.local")
			})
			s := readFile(t, path)
			if strings.Count(s, hostsfile.BeginMarker) != 1 || strings.Count(s, hostsfile.EndMarker) != 1 {
				t.Fatalf("duplicate markers:\n%s", s)
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, path); got != operatorHosts {
				t.Fatalf("leftovers after stop:\n%s", got)
			}
		})
	}
}

func TestFatalStartupErrorStillRemovesAStaleBlock(t *testing.T) {
	t.Parallel()
	cfg, path := testConfig(t, config.MountModeDir)
	stale := operatorHosts + hostsfile.BeginMarker + "\n172.17.0.9\tgone.docker.local\n" + hostsfile.EndMarker + "\n"
	if err := os.WriteFile(path, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}

	api := &fakeDocker{infoErr: errors.New("daemon down")}
	err := New(cfg, Options{API: api}).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "daemon down") {
		t.Fatalf("Run = %v, want the Docker error", err)
	}
	if got := readFile(t, path); got != operatorHosts {
		t.Fatalf("a stale block survived a failed start:\n%s", got)
	}
}

func TestPanicInThePipelineStopsTheAgentAndCleansUp(t *testing.T) {
	t.Parallel()
	cfg, path := testConfig(t, config.MountModeFile)
	api := &fakeDocker{panicList: true}

	done := make(chan error, 1)
	go func() { done <- New(cfg, Options{API: api}).Run(context.Background()) }()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "panicked") {
			t.Fatalf("Run = %v, want a panic error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the agent did not stop after a panic")
	}
	if got := readFile(t, path); got != operatorHosts {
		t.Fatalf("the file was left modified:\n%s", got)
	}
}

func TestMissingHostsFileIsNotCreatedByAStop(t *testing.T) {
	t.Parallel()
	cfg, _ := testConfig(t, config.MountModeDir)
	missing := filepath.Join(t.TempDir(), "absent")
	cfg.HostsFile = missing

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	// No container, no block: the file must not appear as a side effect.
	if err := New(cfg, Options{API: &fakeDocker{}}).Run(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); err == nil {
		t.Fatal("the hosts file was created")
	}
}

func TestCleanIsIdempotent(t *testing.T) {
	t.Parallel()
	for _, mode := range []config.MountMode{config.MountModeFile, config.MountModeDir} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			cfg, path := testConfig(t, mode)
			dirty := operatorHosts + hostsfile.BeginMarker + "\n172.17.0.9\tgone.docker.local\n" + hostsfile.EndMarker + "\n"
			if err := os.WriteFile(path, []byte(dirty), 0o644); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				if err := Clean(cfg, nil2()); err != nil {
					t.Fatalf("Clean #%d: %v", i, err)
				}
				if got := readFile(t, path); got != operatorHosts {
					t.Fatalf("after Clean #%d:\n%s", i, got)
				}
			}
		})
	}
}

func nil2() *slog.Logger { return slog.New(slog.DiscardHandler) }
