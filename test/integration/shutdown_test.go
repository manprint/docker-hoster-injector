//go:build integration

package integration

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests run real agent processes against a sandbox hosts file, so they
// can send any signal they like without disturbing the shared agent that works
// on the real /etc/hosts. What they assert is the promise made to the operator:
// however the agent ends, the file is theirs again, byte for byte.

const exitTimeout = 12 * time.Second

// sandboxAgent is one agent process over a private hosts file.
type sandboxAgent struct {
	t    *testing.T
	cmd  *exec.Cmd
	path string
	log  string
	done chan error
}

func sandboxEnv(path, mode, dockerHost string) []string {
	return append(os.Environ(),
		"HOSTS_FILE="+path,
		"HOSTS_MOUNT_MODE="+mode,
		"DNS_SUFFIX=docker.local",
		"RESYNC_INTERVAL=2s",
		"EVENT_DEBOUNCE=50ms",
		"LOG_FORMAT=json",
		"WEB_ENABLED=false",
		"DOCKER_HOST="+dockerHost,
	)
}

func newSandboxHosts(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func startSandboxAgent(t *testing.T, path, mode, dockerHost string, args ...string) *sandboxAgent {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "agent.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(agentBin, args...)
	cmd.Env = sandboxEnv(path, mode, dockerHost)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the agent: %v", err)
	}
	a := &sandboxAgent{t: t, cmd: cmd, path: path, log: logPath, done: make(chan error, 1)}
	go func() { a.done <- cmd.Wait(); _ = logFile.Close() }()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			select {
			case <-a.done:
			case <-time.After(3 * time.Second):
			}
		}
	})
	return a
}

func (a *sandboxAgent) logText() string {
	b, _ := os.ReadFile(a.log)
	return string(b)
}

func (a *sandboxAgent) file() string {
	b, err := os.ReadFile(a.path)
	if err != nil {
		a.t.Fatalf("read the sandbox hosts file: %v", err)
	}
	return string(b)
}

// waitForBlock waits until the block holds at least one record.
func (a *sandboxAgent) waitForBlock() {
	a.t.Helper()
	waitFor(a.t, 30*time.Second, "the agent to write its block", func() bool {
		s := a.file()
		return strings.Contains(s, beginMarker) && strings.Contains(s, ".docker.local")
	})
}

// wait returns the exit code and fails if the process outlives the timeout.
func (a *sandboxAgent) wait() int {
	a.t.Helper()
	select {
	case err := <-a.done:
		var ee *exec.ExitError
		switch {
		case err == nil:
			return 0
		case errors.As(err, &ee):
			return ee.ExitCode()
		default:
			a.t.Fatalf("wait: %v", err)
		}
	case <-time.After(exitTimeout):
		a.t.Fatalf("the agent did not exit within %v of the signal\n%s", exitTimeout, a.logText())
	}
	return -1
}

var stopSignals = map[string]syscall.Signal{
	"SIGTERM": syscall.SIGTERM,
	"SIGINT":  syscall.SIGINT,
	"SIGHUP":  syscall.SIGHUP,
}

func TestGracefulStopRemovesTheBlock(t *testing.T) {
	requireDocker(t)
	for _, mode := range []string{"file", "dir"} {
		for name, sig := range stopSignals {
			t.Run(mode+"/"+name, func(t *testing.T) {
				path := newSandboxHosts(t, seedHosts)
				a := startSandboxAgent(t, path, mode, dockerHost)
				a.waitForBlock()

				start := time.Now()
				if err := a.cmd.Process.Signal(sig); err != nil {
					t.Fatal(err)
				}
				if code := a.wait(); code != 0 {
					t.Fatalf("exit code %d, want 0\n%s", code, a.logText())
				}
				if took := time.Since(start); took > 5*time.Second {
					t.Errorf("stopping took %v", took)
				}
				if got := a.file(); got != seedHosts {
					t.Fatalf("the hosts file is not back to the operator's content:\n%s", got)
				}
			})
		}
	}
}

// Signals that arrive while the agent is already cleaning up must be absorbed,
// not cut the cleanup short.
func TestRepeatedSignalsDoNotInterruptTheCleanup(t *testing.T) {
	requireDocker(t)
	path := newSandboxHosts(t, seedHosts)
	a := startSandboxAgent(t, path, "file", dockerHost)
	a.waitForBlock()

	for i := 0; i < 5; i++ {
		_ = a.cmd.Process.Signal(syscall.SIGTERM)
		_ = a.cmd.Process.Signal(syscall.SIGINT)
		time.Sleep(5 * time.Millisecond)
	}
	if code := a.wait(); code != 0 {
		t.Fatalf("exit code %d, want 0\n%s", code, a.logText())
	}
	if got := a.file(); got != seedHosts {
		t.Fatalf("leftovers after repeated signals:\n%s", got)
	}
}

// A forced kill cannot clean up. What must hold is that the next start
// converges to exactly one well-formed block, that stopping it restores the
// operator's file, and that "clean" can do the same without any agent.
func TestForcedKillIsRecoveredByTheNextStart(t *testing.T) {
	requireDocker(t)
	for _, mode := range []string{"file", "dir"} {
		t.Run(mode, func(t *testing.T) {
			path := newSandboxHosts(t, seedHosts)

			a := startSandboxAgent(t, path, mode, dockerHost)
			a.waitForBlock()
			if err := a.cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			<-a.done
			if !strings.Contains(a.file(), beginMarker) {
				t.Fatal("expected the block to remain after SIGKILL: nothing could have removed it")
			}

			// The records of a container that is gone, as a killed agent would
			// have left them if it had died after the container stopped.
			stale := strings.Replace(a.file(), endMarker, "172.30.0.9\tghost.docker.local\n"+endMarker, 1)
			if err := os.WriteFile(path, []byte(stale), 0o644); err != nil {
				t.Fatal(err)
			}

			b := startSandboxAgent(t, path, mode, dockerHost)
			waitFor(t, 30*time.Second, "the restarted agent to drop the stale record", func() bool {
				s := b.file()
				return strings.Contains(s, ".docker.local") && !strings.Contains(s, "ghost.docker.local")
			})
			// The signal handler is installed before the first log line, so a
			// signal sent after it is a stop and not a default-action kill.
			waitFor(t, 10*time.Second, "the agent to log its start", func() bool {
				return strings.Contains(b.logText(), `"starting"`)
			})
			s := b.file()
			if strings.Count(s, beginMarker) != 1 || strings.Count(s, endMarker) != 1 {
				t.Fatalf("the restarted agent left duplicate markers:\n%s", s)
			}
			if !strings.HasPrefix(s, seedHosts) {
				t.Fatalf("the operator's part changed:\n%s", s)
			}

			_ = b.cmd.Process.Signal(syscall.SIGTERM)
			if code := b.wait(); code != 0 {
				t.Fatalf("exit code %d\n%s", code, b.logText())
			}
			if got := b.file(); got != seedHosts {
				t.Fatalf("leftovers after the restart and stop:\n%s", got)
			}
		})
	}
}

func TestCleanCommandRemovesAStaleBlockAndIsIdempotent(t *testing.T) {
	requireDocker(t)
	for _, mode := range []string{"file", "dir"} {
		t.Run(mode, func(t *testing.T) {
			path := newSandboxHosts(t, seedHosts)
			a := startSandboxAgent(t, path, mode, dockerHost)
			a.waitForBlock()
			_ = a.cmd.Process.Kill()
			<-a.done

			for i := 0; i < 3; i++ {
				c := startSandboxAgent(t, path, mode, dockerHost, "clean")
				if code := c.wait(); code != 0 {
					t.Fatalf("clean #%d exited %d\n%s", i, code, c.logText())
				}
				if got := c.file(); got != seedHosts {
					t.Fatalf("after clean #%d:\n%s", i, got)
				}
			}
		})
	}
}

// A kill in the middle of the block (a torn write) followed by a clean must
// still leave the operator's file and nothing else.
func TestCleanAfterATornBlock(t *testing.T) {
	requireDocker(t)
	torn := seedHosts + beginMarker + "\n172.30.0.9\tghost.docker.local\n"
	path := newSandboxHosts(t, torn)

	c := startSandboxAgent(t, path, "file", dockerHost, "clean")
	if code := c.wait(); code != 0 {
		t.Fatalf("clean exited %d\n%s", code, c.logText())
	}
	if got := c.file(); got != seedHosts {
		t.Fatalf("a torn block left residue:\n%s", got)
	}
}

// With the daemon unreachable the agent cannot do its job. It must fail, and
// it must not leave records from a previous run pointing at nothing.
func TestUnreachableDockerFailsAndLeavesNoStaleBlock(t *testing.T) {
	stale := seedHosts + beginMarker + "\n172.30.0.9\tghost.docker.local\n" + endMarker + "\n"
	path := newSandboxHosts(t, stale)

	a := startSandboxAgent(t, path, "file", "unix:///nonexistent/docker.sock")
	if code := a.wait(); code == 0 {
		t.Fatalf("exit code 0 with no Docker daemon\n%s", a.logText())
	}
	if got := a.file(); got != seedHosts {
		t.Fatalf("a stale block survived the failed start:\n%s", got)
	}
}

// Every agent run must leave the file's mode alone.
func TestStopKeepsTheFileMode(t *testing.T) {
	requireDocker(t)
	for _, mode := range []string{"file", "dir"} {
		t.Run(mode, func(t *testing.T) {
			path := newSandboxHosts(t, seedHosts)
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatal(err)
			}
			a := startSandboxAgent(t, path, mode, dockerHost)
			a.waitForBlock()
			_ = a.cmd.Process.Signal(syscall.SIGTERM)
			a.wait()
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o640 {
				t.Errorf("mode = %v, want 0640", info.Mode().Perm())
			}
			entries, _ := os.ReadDir(filepath.Dir(path))
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".hosts-docker-hoster-injector-") {
					t.Errorf("temporary file left behind: %s", e.Name())
				}
			}
		})
	}
}
