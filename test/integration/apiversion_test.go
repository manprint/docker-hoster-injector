//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"
)

// An agent can be pinned to an older API version with DOCKER_API_VERSION, which
// makes every request it sends look like the ones an older client would send.
// This is the closest a single daemon can get to an older engine: it checks
// that the endpoints and fields the agent relies on exist, and are read the same
// way, back to the oldest API the agent accepts.
func TestAgentWorksWithOlderAPIVersions(t *testing.T) {
	requireDocker(t)
	const probe = "dhi-apiver"
	_ = runQuiet(20*time.Second, "docker", "rm", "-f", probe)
	if out, err := run(60*time.Second, "docker", "run", "-d", "--name", probe,
		"-p", "127.0.0.1::8080", "--expose", "9090", "busybox:latest", "sleep", "600"); err != nil {
		t.Fatalf("start the probe container: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = runQuiet(20*time.Second, "docker", "rm", "-f", probe) })

	for _, v := range []string{"1.40", "1.41", "1.43", "1.44"} {
		t.Run("API "+v, func(t *testing.T) {
			t.Setenv("DOCKER_API_VERSION", v)
			path := newSandboxHosts(t, seedHosts)
			a := startSandboxAgent(t, path, "file", dockerHost)
			waitFor(t, 30*time.Second, "the probe container in the block", func() bool {
				return strings.Contains(a.file(), probe+".docker.local")
			})
			if err := a.cmd.Process.Signal(stopSignals["SIGTERM"]); err != nil {
				t.Fatal(err)
			}
			if code := a.wait(); code != 0 {
				t.Fatalf("exit code %d, want 0\n%s", code, a.logText())
			}
			if got := a.file(); got != seedHosts {
				t.Fatalf("the hosts file is not back to the operator's content:\n%s", got)
			}
		})
	}
}
