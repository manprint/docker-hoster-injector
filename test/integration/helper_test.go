//go:build integration

package integration

import (
	"bufio"
	"context"
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"

	"github.com/manprint/docker-hoster-injector/internal/config"
	"github.com/manprint/docker-hoster-injector/internal/hostsfile"
)

func net4(i int) net.IP { return net.IPv4(172, 17, 0, byte(i)) }

// execCommandContext is a thin seam so tests can read without an import alias
// on every line.
func execCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// childRounds is how many times the child rewrites the file before it would
// exit on its own. The parent always kills it first, but the loop must be long
// enough that the kill lands during the write phase.
const childRounds = 1000000

// readyLine is printed by the child once it is inside the write loop. Waiting
// for it is what makes the kill land on a real write instead of on startup.
const readyLine = "WRITER-READY"

// newWriterChild builds the command that plays the part of a running agent
// process: it opens the hosts file and rewrites it in a tight loop, so the
// parent can SIGKILL it in the middle of a write.
//
// A real child process is used rather than a goroutine because a goroutine
// cannot be SIGKILLed: only a genuine kill leaves the kind of on-disk state
// this suite is about.
//
// The child is this same test binary re-executed with a different entry point,
// so it runs the exact code under test with no second binary to build and no
// risk of the two drifting apart.
func newWriterChild(t *testing.T, path string, mode config.MountMode) (*exec.Cmd, *bufio.Scanner, func()) {
	t.Helper()

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate the test binary: %v", err)
	}

	cmd := exec.Command(self,
		"-test.run=^TestWriterChildProcess$",
		"-test.timeout=10m",
	)
	cmd.Env = append(os.Environ(),
		"GO_WANT_HELPER_PROCESS=1",
		"HOSTS_PATH="+path,
		"HOSTS_MOUNT_MODE="+string(mode),
		"HOSTS_ROUNDS="+strconv.Itoa(childRounds),
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr

	return cmd, bufio.NewScanner(stdout), func() { _ = cmd.Process.Kill() }
}

// waitForReady blocks until the child reports it is writing, so the parent
// never kills a process that is still starting up.
func waitForReady(t *testing.T, sc *bufio.Scanner) {
	t.Helper()
	for sc.Scan() {
		if line := sc.Text(); line == readyLine {
			return
		}
	}
	t.Fatalf("the child exited before signalling readiness")
}

// TestWriterChildProcess is not a real test. It is the payload the parent
// process kills, and it exits immediately unless the helper environment
// variable is set.
func TestWriterChildProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		t.Skip("helper process, not a standalone test")
	}

	path := os.Getenv("HOSTS_PATH")
	mode := config.MountMode(os.Getenv("HOSTS_MOUNT_MODE"))
	rounds, err := strconv.Atoi(os.Getenv("HOSTS_ROUNDS"))
	if err != nil {
		t.Fatalf("bad HOSTS_ROUNDS: %v", err)
	}

	w, err := hostsfile.NewWriter(path, mode)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Adopt(); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	// Signal readiness only once the first write has landed, so the parent's
	// kill is guaranteed to interrupt the write loop.
	if _, err := w.Apply([]hostsfile.Entry{{IP: net4(2), Names: []string{"warmup.docker.local"}}}); err != nil {
		t.Fatalf("warmup Apply: %v", err)
	}
	os.Stdout.WriteString(readyLine + "\n")

	for i := 0; i < rounds; i++ {
		entries := []hostsfile.Entry{
			{IP: net4(2), Names: []string{"alpha.docker.local"}},
			{IP: net4(3), Names: []string{"beta.docker.local", "b.docker.local"}},
			{IP: net4(4), Names: []string{"gamma.docker.local"}},
		}
		// Vary the set so most rounds are genuine writes rather than no-ops.
		// The block is longer than the previous one in some rounds and
		// shorter in others, which exercises both the trailing-trim and the
		// plain overwrite paths.
		switch i % 4 {
		case 0:
			entries = entries[:1]
		case 1:
			entries = append(entries, hostsfile.Entry{
				IP:    net4(5),
				Names: []string{"delta.docker.local"},
			})
		case 2:
			entries = entries[1:]
		}

		if _, err := w.Apply(entries); err != nil {
			// A write failure is not what this test is about; the parent
			// inspects the resulting file. Keep looping so the process stays
			// alive to be killed.
			continue
		}
	}
}
