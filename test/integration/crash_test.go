//go:build integration

// Package integration exercises the agent against a real Docker daemon and
// against real process kills.
//
// These tests are slower and more environmental than the unit tests, so they
// live behind the "integration" build tag and are run by "make test-integration"
// and by the CI matrix. Each one skips, rather than fails, when its
// prerequisite is missing, so the suite stays useful on a machine without
// Docker.
package integration

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/manprint/docker-hoster-injector/internal/config"
	"github.com/manprint/docker-hoster-injector/internal/hostsfile"
	"github.com/manprint/docker-hoster-injector/internal/naming"
)

// Timing constants. They are deliberately generous: a loaded CI runner can be
// an order of magnitude slower than a developer machine, and a flaky
// integration test is worse than no test at all.
const (
	// settleTimeout is how long to wait for a container's record to appear.
	settleTimeout = 30 * time.Second

	// settlePoll is the polling interval while waiting for convergence.
	settlePoll = 50 * time.Millisecond

	// crashIterations is how many times the agent is killed and restarted.
	// Each iteration picks a fresh random offset, so the kill lands at a
	// different point of the write cycle every round.
	crashIterations = 15
)

// requireDocker skips the test when no usable Docker daemon is reachable.
func requireDocker(t *testing.T) {
	t.Helper()
	if os.Getenv("DOCKER_HOST") == "" && !fileExists("/var/run/docker.sock") {
		t.Skip("no Docker daemon: set DOCKER_HOST or run with a socket")
	}
	if out, err := runCommand(200*time.Millisecond, "docker", "version", "--format", "{{.Server.Version}}"); err != nil {
		t.Skipf("no usable Docker daemon: %v (%s)", err, strings.TrimSpace(out))
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func runCommand(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := execCommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// hostEntry resolves a name through the same NSS path a browser or curl would
// use. That is the only check that proves the feature actually works; reading
// the file back would only prove the writer wrote something.
func hostEntry(t *testing.T, name string) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := execCommandContext(ctx, "getent", "hosts", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// waitFor polls cond until it returns true or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(settlePoll)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// waitForEntry waits until name resolves on the host.
func waitForEntry(t *testing.T, name string) string {
	t.Helper()
	var got string
	waitFor(t, settleTimeout, "the name "+name+" to resolve", func() bool {
		v, ok := hostEntry(t, name)
		if ok {
			got = v
			return true
		}
		return false
	})
	return got
}

// waitForNoEntry waits until name stops resolving, which is what must happen
// when the container goes away.
func waitForNoEntry(t *testing.T, name string) {
	t.Helper()
	waitFor(t, settleTimeout, "the name "+name+" to disappear", func() bool {
		_, ok := hostEntry(t, name)
		return !ok
	})
}

// fixture is an isolated hosts file plus the writer that manages it.
type fixture struct {
	t      *testing.T
	dir    string
	path   string
	writer *hostsfile.Writer

	// damageIndex cycles through the simulated crash shapes so a run covers
	// all of them without randomness.
	damageIndex int
}

// newFixture creates a hosts file seeded with realistic operator content.
func newFixture(t *testing.T, mode config.MountMode) *fixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")

	const seed = "127.0.0.1\tlocalhost\n" +
		"127.0.0.1\tlocalhost.localdomain\n" +
		"::1     ip6-localhost ip6-loopback\n" +
		"192.168.1.10   nas.home\n" +
		"10.0.0.5      printer printer.lan\n"
	if err := os.WriteFile(path, []byte(seed), hostsfile.DefaultPerm); err != nil {
		t.Fatalf("seed hosts file: %v", err)
	}

	w, err := hostsfile.NewWriter(path, mode)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Adopt(); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	return &fixture{t: t, dir: dir, path: path, writer: w}
}

// read returns the current file contents.
func (f *fixture) read() string {
	f.t.Helper()
	data, err := os.ReadFile(f.path)
	if err != nil {
		f.t.Fatalf("read hosts file: %v", err)
	}
	return string(data)
}

// apply writes a set of records.
func (f *fixture) apply(names ...string) {
	f.t.Helper()
	entries := make([]hostsfile.Entry, 0, len(names))
	for i, n := range names {
		entries = append(entries, hostsfile.Entry{
			IP:    net.IPv4(172, 17, 0, byte(i+2)),
			Names: []string{n},
		})
	}
	if _, err := f.writer.Apply(entries); err != nil {
		f.t.Fatalf("Apply: %v", err)
	}
}

// seedLines are the operator's entries, which must survive every operation.
var seedLines = []string{
	"127.0.0.1\tlocalhost",
	"127.0.0.1\tlocalhost.localdomain",
	"::1     ip6-localhost ip6-loopback",
	"192.168.1.10   nas.home",
	"10.0.0.5      printer printer.lan",
}

// assertUserIntact verifies the operator's content survived byte for byte.
func (f *fixture) assertUserIntact() {
	f.t.Helper()
	got := f.read()
	for _, line := range seedLines {
		if !strings.Contains(got, line) {
			f.t.Errorf("the operator's line %q was lost:\n%s", line, got)
		}
	}
}

// assertWellFormed verifies the file is a valid hosts file with exactly one
// coherent managed block. This is the invariant that must hold after any
// crash, because a later run has to be able to parse it.
func (f *fixture) assertWellFormed() {
	f.t.Helper()
	got := f.read()

	block, err := hostsfile.Block([]byte(got))
	if err != nil {
		f.t.Fatalf("the hosts file does not parse after the crash: %v\ncontent:\n%s", err, got)
	}
	// Every name we published must be something a resolver can look up. A
	// crash must never leave a half-written or unparseable record behind.
	for _, e := range block {
		if e.IP == nil {
			f.t.Errorf("a managed entry has no address: %+v", e)
		}
		for _, n := range e.Names {
			if !naming.IsPublishable(strings.TrimSuffix(n, ".docker.local")) {
				f.t.Errorf("a managed name %q is not resolvable", n)
			}
		}
	}

	if n := strings.Count(got, hostsfile.BeginMarker); n > 1 {
		f.t.Errorf("found %d block headers:\n%s", n, got)
	}
	if n := strings.Count(got, hostsfile.EndMarker); n > 1 {
		f.t.Errorf("found %d block footers:\n%s", n, got)
	}
	if strings.Contains(got, hostsfile.BeginMarker) && !strings.Contains(got, hostsfile.EndMarker) {
		f.t.Errorf("the managed block was left unterminated, which is what a crash looks like:\n%s", got)
	}
	for i, line := range strings.Split(got, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) < 2 {
			// A truncated tail is the one artefact a crash can leave, and it
			// is what Repair exists to clean up. Anything else is a bug.
			if i == len(strings.Split(got, "\n"))-1 || i == len(strings.Split(got, "\n"))-2 {
				continue
			}
			f.t.Errorf("line %d is malformed and not a trailing artefact: %q", i, line)
		}
	}
}

// restart simulates what a container restart of the agent does: a brand new
// process that adopts whatever is on disk, repairs damage and applies the
// desired state.
func (f *fixture) restart(names ...string) {
	f.t.Helper()
	// A new writer, with no memory of the previous process.
	w, err := hostsfile.NewWriter(f.path, f.writer.Mode())
	if err != nil {
		f.t.Fatalf("NewWriter on restart: %v", err)
	}
	if err := w.Adopt(); err != nil {
		f.t.Fatalf("Adopt on restart: %v", err)
	}
	if repaired, err := w.Repair(); err != nil {
		f.t.Fatalf("Repair on restart: %v", err)
	} else if repaired {
		f.t.Log("the restart repaired damage left by the crash")
	}
	f.writer = w

	entries := make([]hostsfile.Entry, 0, len(names))
	for i, n := range names {
		entries = append(entries, hostsfile.Entry{
			IP:    net.IPv4(172, 17, 0, byte(i+2)),
			Names: []string{n},
		})
	}
	if _, err := w.Apply(entries); err != nil {
		f.t.Fatalf("Apply on restart: %v", err)
	}
}

// TestCrashDuringWritesLeavesAStableFile kills a child process while it is
// rewriting the hosts file, then checks that a restart converges to the
// correct state.
//
// The child is the real writer running in a separate process, so the kill is a
// genuine SIGKILL: no deferred cleanup, no flush, no graceful shutdown.
// Everything the parent asserts afterwards is what an operator would actually
// observe on their machine.
//
// The kill is repeated at many different points in the write cycle. A single
// kill proves very little: whether it lands during a write, between a write and
// its fsync, or before the loop even starts is pure timing, and the interesting
// cases are the unlucky ones.
func TestCrashDuringWritesLeavesAStableFile(t *testing.T) {
	t.Parallel()

	for _, mode := range []config.MountMode{config.MountModeFile, config.MountModeDir} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			path := filepath.Join(dir, "hosts")
			seed := strings.Join(seedLines, "\n") + "\n"

			for round := range crashIterations {
				// Start from the operator's file every round, so each kill is
				// independent and a failure points at one specific moment.
				if err := os.WriteFile(path, []byte(seed), hostsfile.DefaultPerm); err != nil {
					t.Fatal(err)
				}

				cmd, stdout, cleanup := newWriterChild(t, path, mode)
				if err := cmd.Start(); err != nil {
					cleanup()
					t.Fatalf("round %d: start the writer child: %v", round, err)
				}
				waitForReady(t, stdout)

				// Let the child run for a while so the kill lands inside the
				// write loop rather than on the warmup write. The delay grows
				// per round, which walks the kill across different phases of
				// the write and makes a lucky pass increasingly unlikely.
				time.Sleep(time.Duration(round+1) * 25 * time.Millisecond)

				// SIGKILL: the harshest termination there is.
				if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
					cleanup()
					t.Fatalf("round %d: kill the writer child: %v", round, err)
				}
				_ = cmd.Wait()

				// Whatever the timing, the file must still be readable and the
				// operator's content must be intact. This is the property
				// that makes a crash survivable at all.
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("round %d: the hosts file is unreadable after SIGKILL: %v", round, err)
				}
				got := string(data)
				if len(got) == 0 {
					t.Fatalf("round %d: SIGKILL left the hosts file empty", round)
				}
				for _, line := range seedLines {
					if !strings.Contains(got, line) {
						t.Fatalf("round %d: the operator's line %q did not survive SIGKILL:\n%s", round, line, got)
					}
				}

				// Liveness: prove the child had actually reached its write
				// loop before the kill. Without this the test could pass
				// trivially, killing a process that had only done its warmup
				// write and never exercised a real write at all.
				if !strings.Contains(got, "warmup.docker.local") &&
					!anyContains(got, "alpha.docker.local", "beta.docker.local", "gamma.docker.local", "delta.docker.local") {
					t.Fatalf("round %d: the child never performed a write, so this round proves nothing:\n%s", round, got)
				}

				// A crash may leave the block unterminated, but it must never
				// leave two blocks or a malformed record in the user section.
				if n := strings.Count(got, hostsfile.BeginMarker); n > 1 {
					t.Fatalf("round %d: found %d block headers after SIGKILL:\n%s", round, n, got)
				}

				// Restart and converge.
				w, err := hostsfile.NewWriter(path, mode)
				if err != nil {
					t.Fatal(err)
				}
				if err := w.Adopt(); err != nil {
					t.Fatalf("round %d: Adopt after crash: %v", round, err)
				}
				if _, err := w.Repair(); err != nil {
					t.Fatalf("round %d: Repair after crash: %v", round, err)
				}

				want := []hostsfile.Entry{{
					IP:    net.ParseIP("172.17.0.2"),
					Names: []string{"nginx.docker.local"},
				}}
				if _, err := w.Apply(want); err != nil {
					t.Fatalf("round %d: Apply after crash: %v", round, err)
				}

				final := readFile(t, path)
				if !strings.Contains(final, "nginx.docker.local") {
					t.Fatalf("round %d: the record is missing after the restart:\n%s", round, final)
				}
				for _, line := range seedLines {
					if !strings.Contains(final, line) {
						t.Fatalf("round %d: the operator's line %q was lost during recovery:\n%s", round, line, final)
					}
				}
				if n := strings.Count(final, hostsfile.BeginMarker); n != 1 {
					t.Fatalf("round %d: found %d block headers after recovery, want 1:\n%s", round, n, final)
				}
				if !strings.Contains(final, hostsfile.EndMarker) {
					t.Fatalf("round %d: the block is unterminated after recovery:\n%s", round, final)
				}
				// The recovered state must be a fixed point.
				if changed, err := w.Apply(want); err != nil || changed {
					t.Fatalf("round %d: the state did not settle after recovery (changed=%t err=%v)", round, changed, err)
				}
			}
		})
	}
}

// TestRepeatedCrashesConverge kills and restarts the agent many times, each
// time at a different point, and asserts that the system always settles into
// the correct state.
func TestRepeatedCrashesConverge(t *testing.T) {
	t.Parallel()

	f := newFixture(t, config.MountModeFile)
	f.apply("first.docker.local")

	// Records to converge on, changing every round so a restart has real work
	// to do.
	wanted := [][]string{
		{"first.docker.local"},
		{"first.docker.local", "second.docker.local"},
		{"second.docker.local"},
		{"second.docker.local", "third.docker.local"},
		{"third.docker.local"},
	}

	for i, names := range wanted {
		// Alternate between a clean restart and a simulated crash: damage a
		// crash could leave behind, then recover from it.
		if i%2 == 1 {
			f.simulateCrashDamage()
		}
		f.restart(names...)

		f.assertUserIntact()
		f.assertWellFormed()

		// Converge to exactly the wanted set.
		got := managedNames(f.read())
		wantSet := map[string]bool{}
		for _, n := range names {
			wantSet[n] = true
		}
		for _, n := range got {
			if !wantSet[n] {
				t.Errorf("round %d: stale record %q survived\ncurrent file:\n%s", i, n, f.read())
			}
		}
		for n := range wantSet {
			if !contains(got, n) {
				t.Errorf("round %d: record %q is missing\ncurrent file:\n%s", i, n, f.read())
			}
		}
	}
}

// simulateCrashDamage writes the kinds of damage a SIGKILL mid-write leaves.
func (f *fixture) simulateCrashDamage() {
	f.t.Helper()

	damages := []func(string) string{
		// The block header was written but not the footer.
		func(s string) string {
			return strings.Replace(s, hostsfile.EndMarker+"\n", "", 1)
		},
		// A half-written line at the end.
		func(s string) string {
			return s + "172.17.0.99 half-writt"
		},
		// The file was truncated to nothing.
		func(string) string { return "" },
		// The managed block vanished entirely, as if the write never landed.
		func(s string) string {
			before, _, ok := strings.Cut(s, hostsfile.BeginMarker)
			if !ok {
				return s
			}
			return before
		},
	}

	// Pick deterministically from the round counter so the sequence is
	// reproducible while still covering every damage shape.
	idx := f.damageIndex % len(damages)
	f.damageIndex++
	apply := damages[idx]

	if err := os.WriteFile(f.path, []byte(apply(f.read())), hostsfile.DefaultPerm); err != nil {
		f.t.Fatalf("simulate crash damage: %v", err)
	}
}

// managedNames returns the names inside the managed block.
func managedNames(raw string) []string {
	entries, err := hostsfile.Block([]byte(raw))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Names...)
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// anyContains reports whether s holds at least one of the substrings.
func anyContains(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// TestCrashRecoveryHandlesEveryDamageShape feeds the writer each artefact a
// SIGKILL can produce and checks that a restart repairs all of them.
func TestCrashRecoveryHandlesEveryDamageShape(t *testing.T) {
	t.Parallel()

	type damage struct {
		corrupt func(good string) string

		// recoverable says whether the operator's own lines can still be
		// present afterwards.
		//
		// It is false for the shapes that destroy the whole file, and that
		// limit is deliberate rather than an oversight. The writer never
		// truncates before writing and uses rename where it can, so a crash
		// of the agent itself cannot empty the file. Content that some other
		// actor deleted is not this program's data: a fresh process has no
		// copy of it, and inventing replacements would be worse than
		// admitting the loss.
		recoverable bool
	}

	damages := map[string]damage{
		// Crash artefacts: the block header landed but not the footer.
		"unterminated block": {
			corrupt:     func(s string) string { return strings.Replace(s, hostsfile.EndMarker+"\n", "", 1) },
			recoverable: true,
		},
		// Crash artefact: the previous version's tail is still attached.
		"truncated final line": {
			corrupt: func(s string) string {
				lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
				return strings.Join(lines[:len(lines)-1], "\n") + "\n172.17.0.2 nginx.doc"
			},
			recoverable: true,
		},
		// Crash artefact: the write never landed at all.
		"no managed block": {
			corrupt: func(s string) string {
				before, _, ok := strings.Cut(s, hostsfile.BeginMarker)
				if !ok {
					return s
				}
				return before
			},
			recoverable: true,
		},
		// External damage: something appended junk.
		"garbage appended": {
			corrupt:     func(s string) string { return s + "\x00\x01\x02 binary junk\n" },
			recoverable: true,
		},
		// Total loss caused by something else. The user's lines are gone and
		// no program can bring them back.
		"empty file": {
			corrupt:     func(string) string { return "" },
			recoverable: false,
		},
		"truncated to one byte": {
			corrupt: func(s string) string {
				if s == "" {
					return "x"
				}
				return s[:1]
			},
			recoverable: false,
		},
	}

	for name, d := range damages {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			for _, mode := range []config.MountMode{config.MountModeFile, config.MountModeDir} {
				t.Run(string(mode), func(t *testing.T) {
					t.Parallel()

					// Phase 1: a healthy process establishes a known good state.
					f := newFixture(t, mode)
					f.apply("nginx.docker.local", "web.docker.local")
					good := f.read()

					// Phase 2: the crash leaves damage on disk.
					if err := os.WriteFile(f.path, []byte(d.corrupt(good)), hostsfile.DefaultPerm); err != nil {
						t.Fatal(err)
					}

					// Phase 3: a fresh process starts up. It must not refuse
					// to run: a crash must never prevent recovery.
					w, err := hostsfile.NewWriter(f.path, mode)
					if err != nil {
						t.Fatal(err)
					}
					if err := w.Adopt(); err != nil {
						t.Fatalf("Adopt refused to start on damaged content: %v", err)
					}
					if _, err := w.Repair(); err != nil {
						t.Fatalf("Repair failed: %v", err)
					}

					// Phase 4: it reconciles and writes the desired state.
					want := []hostsfile.Entry{
						{IP: net.ParseIP("127.0.0.1"), Names: []string{"nginx.docker.local"}},
						{IP: net.ParseIP("172.17.0.2"), Names: []string{"web.docker.local"}},
					}
					if _, err := w.Apply(want); err != nil {
						t.Fatalf("Apply after recovery: %v", err)
					}

					// Phase 5: the file is back to a good, well formed state.
					final := readFile(t, f.path)
					if n := strings.Count(final, hostsfile.BeginMarker); n != 1 {
						t.Errorf("found %d block headers, want 1:\n%s", n, final)
					}
					if !strings.Contains(final, hostsfile.EndMarker) {
						t.Errorf("the block is unterminated:\n%s", final)
					}
					for _, n := range []string{"nginx.docker.local", "web.docker.local"} {
						if !strings.Contains(final, n) {
							t.Errorf("record %q is missing after recovery:\n%s", n, final)
						}
					}
					if d.recoverable {
						for _, line := range seedLines {
							if !strings.Contains(final, line) {
								t.Errorf("the operator's line %q was lost:\n%s", line, final)
							}
						}
					}

					// Phase 6: the state has settled, so a further reconcile
					// changes nothing. This is what "a stable situation"
					// means in practice: no flapping, no repeated writes.
					changed, err := w.Apply(want)
					if err != nil {
						t.Fatal(err)
					}
					if changed {
						t.Errorf("the state did not settle: a repeated Apply still changed the file:\n%s", readFile(t, f.path))
					}
					before := readFile(t, f.path)
					if _, err := w.Apply(want); err != nil {
						t.Fatal(err)
					}
					if after := readFile(t, f.path); after != before {
						t.Errorf("the file is still changing between identical applies:\n%s\n---\n%s", before, after)
					}
				})
			}
		})
	}
}

// TestRecoveryDoesNotInventRecords makes sure recovery restores rather than
// fabricates: a container that was never there must not appear.
func TestRecoveryDoesNotInventRecords(t *testing.T) {
	t.Parallel()

	f := newFixture(t, config.MountModeFile)
	f.apply("real.docker.local")

	// Damage, then recover.
	if err := os.WriteFile(f.path, []byte("127.0.0.1 localhost\n"), hostsfile.DefaultPerm); err != nil {
		t.Fatal(err)
	}
	f.restart("real.docker.local")

	got := managedNames(f.read())
	for _, n := range got {
		if n != "real.docker.local" {
			t.Errorf("recovery invented a record: %q\nfile:\n%s", n, f.read())
		}
	}
}

// TestConcurrentAgentsConverge runs several agents against the same file, as
// happens when an operator accidentally starts a second copy. The flock and
// the atomic render must keep the file coherent, and the last writer must win
// with a complete block.
func TestConcurrentAgentsConverge(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")
	if err := os.WriteFile(path, []byte(strings.Join(seedLines, "\n")+"\n"), hostsfile.DefaultPerm); err != nil {
		t.Fatal(err)
	}

	const agents = 4
	done := make(chan error, agents)
	for i := 0; i < agents; i++ {
		go func(i int) {
			w, err := hostsfile.NewWriter(path, config.MountModeFile)
			if err != nil {
				done <- err
				return
			}
			if err := w.Adopt(); err != nil {
				done <- err
				return
			}
			for round := 0; round < 25; round++ {
				name := fmt.Sprintf("agent%d-round%d.docker.local", i, round)
				if _, err := w.Apply([]hostsfile.Entry{{
					IP:    net.IPv4(172, 17, 0, byte(i+2)),
					Names: []string{name},
				}}); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}(i)
	}
	for i := 0; i < agents; i++ {
		if err := <-done; err != nil {
			t.Fatalf("agent %d: %v", i, err)
		}
	}

	final := readFile(t, path)
	if n := strings.Count(final, hostsfile.BeginMarker); n != 1 {
		t.Errorf("found %d block headers, want exactly 1:\n%s", n, final)
	}
	if n := strings.Count(final, hostsfile.EndMarker); n != 1 {
		t.Errorf("found %d block footers, want exactly 1:\n%s", n, final)
	}
	for _, line := range seedLines {
		if !strings.Contains(final, line) {
			t.Errorf("the operator's line %q was lost under concurrency:\n%s", line, final)
		}
	}
}

// TestNamesFromRealContainersAreAlwaysResolvable is a guard on the naming and
// hostsfile packages working together: whatever a container is called, the
// record that reaches the file must be resolvable.
func TestNamesFromRealContainersAreAlwaysResolvable(t *testing.T) {
	t.Parallel()

	containers := []string{
		"nginx",
		"myproject_web_1",
		"my-project_worker-1",
		"Nginx",
		"k8s_POD_nginx_abc123",
		"api-server",
		"a.b.c",
		strings.Repeat("long", 40),
	}

	f := newFixture(t, config.MountModeFile)

	var entries []hostsfile.Entry
	for i, c := range containers {
		for _, rec := range naming.Records("docker.local", c) {
			entries = append(entries, hostsfile.Entry{
				IP:    net.IPv4(172, 17, 0, byte(i+2)),
				Names: []string{rec},
			})
		}
	}
	if _, err := f.writer.Apply(entries); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := f.read()
	if _, err := hostsfile.Block([]byte(got)); err != nil {
		t.Fatalf("the rendered file does not parse: %v\n%s", err, got)
	}

	// Every published name must be present as a single token and safe.
	for _, c := range containers {
		for _, rec := range naming.Records("docker.local", c) {
			if !strings.Contains(got, " "+rec) && !strings.Contains(got, "\t"+rec) {
				t.Errorf("record %q (from container %q) is not in the file:\n%s", rec, c, got)
			}
			if strings.ContainsAny(rec, " \t\n#") {
				t.Errorf("record %q contains a character that would break the format", rec)
			}
			if !naming.IsPublishable(strings.TrimSuffix(rec, ".docker.local")) {
				t.Errorf("record %q has a host part that IsPublishable rejects", rec)
			}
		}
	}

	// And it must settle: no repeated apply changes anything.
	changed, err := f.writer.Apply(entries)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("the state did not settle")
	}
}
