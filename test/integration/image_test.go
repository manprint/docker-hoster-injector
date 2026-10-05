//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests run the agent the way it is deployed: the image, the Docker
// socket, and a hosts file bind-mounted from the host. They are what shows that
// "docker stop" leaves the operator's file as it found it.

const testImage = "dhi-it-image:test"

var imageBuilt bool

func buildImage(t *testing.T) {
	t.Helper()
	if imageBuilt {
		return
	}
	root, err := repoRoot()
	if err != nil {
		t.Skipf("cannot locate the repository: %v", err)
	}
	if out, err := run(10*time.Minute, "docker", "build", "-q", "-t", testImage, root); err != nil {
		t.Skipf("the image cannot be built here: %v\n%s", err, out)
	}
	imageBuilt = true
}

type imageRun struct {
	name string
	path string // hosts file (file mode) or its directory (dir mode)
	file string
}

// startImage runs the agent container over a private hosts file.
func startImage(t *testing.T, mode string, extra ...string) *imageRun {
	t.Helper()
	buildImage(t)

	dir := t.TempDir()
	// A bind-mounted directory needs to be reachable by the container's user.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "hosts")
	if err := os.WriteFile(file, []byte(seedHosts), 0o644); err != nil {
		t.Fatal(err)
	}

	r := &imageRun{name: "dhi-it-" + itoa(os.Getpid()) + "-" + mode, file: file}
	_ = runQuiet(30*time.Second, "docker", "rm", "-f", r.name)
	t.Cleanup(func() { _ = runQuiet(30*time.Second, "docker", "rm", "-f", r.name) })

	args := []string{"run", "-d", "--name", r.name,
		"-v", "/var/run/docker.sock:/var/run/docker.sock:ro",
		"-e", "WEB_ENABLED=true", "-e", "RESYNC_INTERVAL=2s", "-e", "EVENT_DEBOUNCE=50ms",
	}
	switch mode {
	case "file":
		args = append(args, "-v", file+":/hosts", "-e", "HOSTS_FILE=/hosts", "-e", "HOSTS_MOUNT_MODE=file")
	default:
		args = append(args, "-v", dir+":/hostdir", "-e", "HOSTS_FILE=/hostdir/hosts", "-e", "HOSTS_MOUNT_MODE=dir")
	}
	args = append(args, extra...)
	args = append(args, testImage)
	if out, err := run(60*time.Second, "docker", args...); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	return r
}

func (r *imageRun) hosts(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(r.file)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (r *imageRun) waitForBlock(t *testing.T) {
	t.Helper()
	waitFor(t, 60*time.Second, "the container to write its block", func() bool {
		s := r.hosts(t)
		return strings.Contains(s, beginMarker) && strings.Contains(s, ".docker.local")
	})
}

func TestDockerStopHandsTheHostsFileBack(t *testing.T) {
	requireDocker(t)
	for _, mode := range []string{"file", "dir"} {
		t.Run(mode, func(t *testing.T) {
			r := startImage(t, mode)
			r.waitForBlock(t)

			start := time.Now()
			if out, err := run(30*time.Second, "docker", "stop", r.name); err != nil {
				t.Fatalf("docker stop: %v\n%s", err, out)
			}
			// Docker kills after 10 seconds. A stop that took that long was
			// not graceful, whatever the exit code says.
			if took := time.Since(start); took > 6*time.Second {
				t.Errorf("docker stop took %v", took)
			}
			code, _ := run(10*time.Second, "docker", "inspect", "-f", "{{.State.ExitCode}}", r.name)
			if strings.TrimSpace(code) != "0" {
				logs, _ := run(10*time.Second, "docker", "logs", r.name)
				t.Errorf("exit code %s, want 0\n%s", strings.TrimSpace(code), logs)
			}
			if got := r.hosts(t); got != seedHosts {
				t.Fatalf("the hosts file is not back to the operator's content:\n%s", got)
			}
		})
	}
}

func TestDockerKillThenCleanAndRestart(t *testing.T) {
	requireDocker(t)
	r := startImage(t, "dir")
	r.waitForBlock(t)

	if out, err := run(30*time.Second, "docker", "kill", r.name); err != nil {
		t.Fatalf("docker kill: %v\n%s", err, out)
	}
	if !strings.Contains(r.hosts(t), beginMarker) {
		t.Fatal("expected the block to remain after SIGKILL: nothing could have removed it")
	}

	// "clean" in a one-off container, with no Docker socket at all.
	out, err := run(60*time.Second, "docker", "run", "--rm",
		"-v", filepath.Dir(r.file)+":/hostdir",
		"-e", "HOSTS_FILE=/hostdir/hosts", "-e", "HOSTS_MOUNT_MODE=dir",
		testImage, "clean")
	if err != nil {
		t.Fatalf("clean: %v\n%s", err, out)
	}
	if got := r.hosts(t); got != seedHosts {
		t.Fatalf("clean left residue:\n%s", got)
	}

	// Run again: nothing to remove, and no error.
	if out, err := run(60*time.Second, "docker", "run", "--rm",
		"-v", filepath.Dir(r.file)+":/hostdir",
		"-e", "HOSTS_FILE=/hostdir/hosts", "-e", "HOSTS_MOUNT_MODE=dir",
		testImage, "clean"); err != nil {
		t.Fatalf("second clean: %v\n%s", err, out)
	}
	if got := r.hosts(t); got != seedHosts {
		t.Fatalf("a second clean changed the file:\n%s", got)
	}
}

func TestDockerRestartAfterKillConvergesWithoutDuplicates(t *testing.T) {
	requireDocker(t)
	r := startImage(t, "file")
	r.waitForBlock(t)
	if out, err := run(30*time.Second, "docker", "kill", r.name); err != nil {
		t.Fatalf("docker kill: %v\n%s", err, out)
	}
	if out, err := run(30*time.Second, "docker", "start", r.name); err != nil {
		t.Fatalf("docker start: %v\n%s", err, out)
	}
	r.waitForBlock(t)
	s := r.hosts(t)
	if strings.Count(s, beginMarker) != 1 || strings.Count(s, endMarker) != 1 {
		t.Fatalf("duplicate markers after a restart:\n%s", s)
	}
	if !strings.HasPrefix(s, seedHosts) {
		t.Fatalf("the operator's part changed:\n%s", s)
	}
	_, _ = run(30*time.Second, "docker", "stop", r.name)
	if got := r.hosts(t); got != seedHosts {
		t.Fatalf("leftovers after the stop:\n%s", got)
	}
}

func TestDockerHealthcheckReportsHealthy(t *testing.T) {
	requireDocker(t)
	r := startImage(t, "dir")
	r.waitForBlock(t)
	waitFor(t, 90*time.Second, "the container to be healthy", func() bool {
		out, _ := run(10*time.Second, "docker", "inspect", "-f", "{{.State.Health.Status}}", r.name)
		return strings.TrimSpace(out) == "healthy"
	})
}
