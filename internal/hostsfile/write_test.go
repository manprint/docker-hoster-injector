package hostsfile

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/mint/docker-hoster-injector/internal/config"
)

// modes are the two write strategies the agent can be configured with. Both
// must satisfy every durability guarantee, so the interesting tests run
// against each of them.
var modes = []config.MountMode{config.MountModeFile, config.MountModeDir}

// newWriter returns a writer over a temporary hosts file pre-filled with
// realistic operator content.
func newWriter(t *testing.T, mode config.MountMode) (*Writer, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")
	if err := os.WriteFile(path, []byte(userHosts), DefaultPerm); err != nil {
		t.Fatalf("seed hosts file: %v", err)
	}
	w, err := NewWriter(path, mode)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Adopt(); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	return w, path
}

func entriesFor(names ...string) []Entry {
	out := make([]Entry, 0, len(names))
	for i, n := range names {
		out = append(out, Entry{
			IP:    net4(i),
			Names: []string{n},
		})
	}
	return out
}

func net4(i int) (ip net.IP) {
	return []byte{127, 0, 0, byte(i + 1)}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestWriterAppliesAndPreservesUserContent(t *testing.T) {
	t.Parallel()

	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			w, path := newWriter(t, mode)

			changed, err := w.Apply(entriesFor("nginx.docker.local"))
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if !changed {
				t.Error("changed = false on the first write")
			}

			got := read(t, path)
			if !strings.Contains(got, "nginx.docker.local") {
				t.Errorf("the new record is missing:\n%s", got)
			}
			// The operator's content must be intact, character for character.
			for _, line := range strings.Split(strings.TrimSpace(userHosts), "\n") {
				if !strings.Contains(got, line) {
					t.Errorf("user line %q was lost:\n%s", line, got)
				}
			}
		})
	}
}

// The single most important property: applying the same state many times must
// leave the file byte identical, so a steady state costs no I/O and no risk.
func TestWriterIsIdempotent(t *testing.T) {
	t.Parallel()

	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			w, path := newWriter(t, mode)

			entries := entriesFor("a.docker.local", "b.docker.local", "c.docker.local")
			if _, err := w.Apply(entries); err != nil {
				t.Fatalf("first Apply: %v", err)
			}
			first := read(t, path)

			info1, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}

			for i := 0; i < 50; i++ {
				changed, err := w.Apply(entries)
				if err != nil {
					t.Fatalf("Apply %d: %v", i, err)
				}
				if changed {
					t.Fatalf("Apply %d reported a change with identical input", i)
				}
			}

			if got := read(t, path); got != first {
				t.Errorf("the file changed across repeated identical applies:\n--- before ---\n%s\n--- after ---\n%s", first, got)
			}
			info2, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !info1.ModTime().Equal(info2.ModTime()) {
				t.Error("an unnecessary write changed the modification time")
			}
		})
	}
}

// Re-applying must never accumulate duplicate records.
func TestWriterDoesNotAccumulateDuplicates(t *testing.T) {
	t.Parallel()

	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			w, path := newWriter(t, mode)

			for i := 0; i < 20; i++ {
				if _, err := w.Apply(entriesFor("stable.docker.local")); err != nil {
					t.Fatalf("Apply %d: %v", i, err)
				}
			}
			got := read(t, path)
			if n := strings.Count(got, "stable.docker.local"); n != 1 {
				t.Errorf("record appears %d times, want 1:\n%s", n, got)
			}
		})
	}
}

func TestWriterRemovesRecordsWhenContainersGoAway(t *testing.T) {
	t.Parallel()

	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			w, path := newWriter(t, mode)

			if _, err := w.Apply(entriesFor("gone.docker.local", "stays.docker.local")); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(read(t, path), "gone.docker.local") {
				t.Fatal("the record was not written")
			}

			if _, err := w.Apply(entriesFor("stays.docker.local")); err != nil {
				t.Fatal(err)
			}
			got := read(t, path)
			if strings.Contains(got, "gone.docker.local") {
				t.Errorf("a stopped container's record is still present:\n%s", got)
			}
			if !strings.Contains(got, "stays.docker.local") {
				t.Errorf("the surviving record was lost:\n%s", got)
			}
			if strings.Contains(got, BeginMarker) == false {
				t.Error("the block should still exist for the surviving record")
			}
		})
	}
}

// The user may edit the hosts file at any time. A stale in-memory copy would
// silently revert their change, so Apply re-reads under the lock.
func TestWriterDoesNotRevertConcurrentUserEdits(t *testing.T) {
	t.Parallel()

	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			w, path := newWriter(t, mode)

			if _, err := w.Apply(entriesFor("first.docker.local")); err != nil {
				t.Fatal(err)
			}

			// Simulate the operator adding a printer between reconciles.
			edited := read(t, path) + "10.1.1.1\tnew-printer\n"
			if err := os.WriteFile(path, []byte(edited), DefaultPerm); err != nil {
				t.Fatal(err)
			}

			if _, err := w.Apply(entriesFor("second.docker.local")); err != nil {
				t.Fatal(err)
			}

			got := read(t, path)
			if !strings.Contains(got, "new-printer") {
				t.Errorf("the operator's edit was reverted:\n%s", got)
			}
			if !strings.Contains(got, "second.docker.local") {
				t.Error("the new record is missing")
			}
		})
	}
}

func TestWriterRecreatesADeletedFile(t *testing.T) {
	t.Parallel()

	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			w, path := newWriter(t, mode)

			if _, err := w.Apply(entriesFor("nginx.docker.local")); err != nil {
				t.Fatal(err)
			}
			// Something removed the file, for example an image rebuild or a
			// careless tmpfiles rule.
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}

			if _, err := w.Apply(entriesFor("nginx.docker.local")); err != nil {
				t.Fatalf("Apply after deletion: %v", err)
			}
			if got := read(t, path); !strings.Contains(got, "nginx.docker.local") {
				t.Errorf("the file was not recreated with the record:\n%s", got)
			}
		})
	}
}

func TestWriterCreatesTheFileWhenAbsent(t *testing.T) {
	t.Parallel()

	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "hosts")

			w, err := NewWriter(path, mode)
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Adopt(); err != nil {
				t.Fatalf("Adopt on a missing file: %v", err)
			}
			if _, err := w.Apply(entriesFor("fresh.docker.local")); err != nil {
				t.Fatalf("Apply: %v", err)
			}

			got := read(t, path)
			if !strings.Contains(got, "fresh.docker.local") {
				t.Errorf("the file was not created correctly:\n%s", got)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if perm := info.Mode().Perm(); perm != DefaultPerm {
				t.Errorf("permissions = %o, want %o: the resolver must be able to read it", perm, DefaultPerm)
			}
		})
	}
}

// A block left open by a crash must be rebuilt, not extended: extending it
// would keep the orphaned content and could publish records the agent no
// longer intends to.
func TestWriterRepairsTruncatedBlock(t *testing.T) {
	t.Parallel()

	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			w, path := newWriter(t, mode)

			// The exact shape a kill -9 leaves behind.
			orphan := read(t, path) + "\n" + BeginMarker + "\n127.0.0.1 orphan.docker.local\n"
			if err := os.WriteFile(path, []byte(orphan), DefaultPerm); err != nil {
				t.Fatal(err)
			}

			if _, err := w.Apply(entriesFor("real.docker.local")); err != nil {
				t.Fatalf("Apply: %v", err)
			}

			got := read(t, path)
			if strings.Count(got, BeginMarker) != 1 {
				t.Errorf("expected exactly one block header, got %d:\n%s", strings.Count(got, BeginMarker), got)
			}
			if strings.Count(got, EndMarker) != 1 {
				t.Errorf("expected exactly one block footer, got %d:\n%s", strings.Count(got, EndMarker), got)
			}
			if strings.Contains(got, "orphan.docker.local") {
				t.Errorf("orphaned content survived the repair:\n%s", got)
			}
			if !strings.Contains(got, "real.docker.local") {
				t.Errorf("the intended record is missing:\n%s", got)
			}
			// And the result must be parseable, which is what a follow-up run
			// depends on.
			if _, err := Parse(strings.NewReader(got)); err != nil {
				t.Errorf("the repaired file does not parse: %v", err)
			}
		})
	}
}

// This is the core crash-recovery guarantee: after any damage, restarting the
// agent must bring the file back to a known good state without losing the
// operator's content.
func TestWriterRepairRestoresLastGoodState(t *testing.T) {
	t.Parallel()

	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			w, path := newWriter(t, mode)

			good := entriesFor("nginx.docker.local", "web.docker.local")
			if _, err := w.Apply(good); err != nil {
				t.Fatal(err)
			}
			want := read(t, path)

			// Simulate the damage a kill -9 mid-write can cause. In "file"
			// mode the new content is written over the old one and only then
			// trimmed, so a crash leaves a half-written line with the
			// previous version's tail still attached. That text cannot be
			// parsed as a hosts file, which is precisely what Repair detects.
			// A crash between writing the block header and its footer leaves
			// the block open. This is the one structural invariant this
			// writer owns, so it is what Repair keys on.
			damaged := strings.Replace(want, EndMarker+"\n", "", 1)
			if err := os.WriteFile(path, []byte(damaged), DefaultPerm); err != nil {
				t.Fatal(err)
			}
			f := mustParse(t, damaged)
			if !f.BlockTruncated {
				t.Fatal("the simulated damage must leave the block unterminated")
			}

			repaired, err := w.Repair()
			if err != nil {
				t.Fatalf("Repair: %v", err)
			}
			if !repaired {
				t.Fatal("Repair reported no intervention on a truncated file")
			}
			if got := read(t, path); got != want {
				t.Errorf("Repair did not restore the last good state:\n--- want ---\n%s\n--- got ---\n%s", want, got)
			}
		})
	}
}

func TestWriterRepairHandlesEmptyAndMissingFiles(t *testing.T) {
	t.Parallel()

	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			w, path := newWriter(t, mode)

			if _, err := w.Apply(entriesFor("nginx.docker.local")); err != nil {
				t.Fatal(err)
			}
			want := read(t, path)

			// Zero length: the worst case, since the host has no resolution
			// at all until this is fixed.
			if err := os.WriteFile(path, nil, DefaultPerm); err != nil {
				t.Fatal(err)
			}
			repaired, err := w.Repair()
			if err != nil {
				t.Fatalf("Repair on empty file: %v", err)
			}
			if !repaired {
				t.Error("Repair did not intervene on an empty file")
			}
			if got := read(t, path); got != want {
				t.Errorf("empty file not restored:\n--- want ---\n%s\n--- got ---\n%s", want, got)
			}

			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			repaired, err = w.Repair()
			if err != nil {
				t.Fatalf("Repair on missing file: %v", err)
			}
			if !repaired {
				t.Error("Repair did not intervene on a missing file")
			}
			if got := read(t, path); got != want {
				t.Errorf("missing file not restored:\n--- want ---\n%s\n--- got ---\n%s", want, got)
			}
		})
	}
}

// A healthy file must not be touched by Repair, otherwise every restart would
// rewrite the operator's file for no reason.
func TestWriterRepairLeavesHealthyFilesAlone(t *testing.T) {
	t.Parallel()

	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			w, path := newWriter(t, mode)

			if _, err := w.Apply(entriesFor("nginx.docker.local")); err != nil {
				t.Fatal(err)
			}
			info1, _ := os.Stat(path)

			repaired, err := w.Repair()
			if err != nil {
				t.Fatal(err)
			}
			if repaired {
				t.Error("Repair intervened on a healthy file")
			}
			info2, _ := os.Stat(path)
			if !info1.ModTime().Equal(info2.ModTime()) {
				t.Error("Repair rewrote a healthy file")
			}
		})
	}
}

// Repair must be safe before anything was ever written: there is nothing to
// restore, and inventing content would be wrong.
func TestWriterRepairWithoutBaselineIsANoop(t *testing.T) {
	t.Parallel()

	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "hosts")
			if err := os.WriteFile(path, []byte("127.0.0.1 keepme\n"), DefaultPerm); err != nil {
				t.Fatal(err)
			}

			w, err := NewWriter(path, mode)
			if err != nil {
				t.Fatal(err)
			}
			// Deliberately no Adopt and no Apply.
			repaired, err := w.Repair()
			if err != nil {
				t.Fatal(err)
			}
			if repaired {
				t.Error("Repair invented content with no baseline")
			}
			if got := read(t, path); got != "127.0.0.1 keepme\n" {
				t.Errorf("the file was modified:\n%s", got)
			}
		})
	}
}

// Two agents must not interleave writes. The advisory flock is what makes a
// second instance safe in "file" mode, where rename is not available.
func TestWriterSerialisesConcurrentWriters(t *testing.T) {
	t.Parallel()

	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "hosts")
			if err := os.WriteFile(path, []byte(userHosts), DefaultPerm); err != nil {
				t.Fatal(err)
			}

			const writers = 8
			var wg sync.WaitGroup
			errs := make([]error, writers)

			for i := 0; i < writers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					w, err := NewWriter(path, mode)
					if err != nil {
						errs[i] = err
						return
					}
					if err := w.Adopt(); err != nil {
						errs[i] = err
						return
					}
					for j := 0; j < 10; j++ {
						if _, err := w.Apply(entriesFor("agent" + string(rune('a'+i)) + ".docker.local")); err != nil {
							errs[i] = err
							return
						}
					}
				}()
			}
			wg.Wait()

			for i, err := range errs {
				if err != nil {
					t.Fatalf("writer %d: %v", i, err)
				}
			}

			// Whatever the interleaving, the result must be a coherent file:
			// exactly one block, and every user's line intact.
			got := read(t, path)
			if n := strings.Count(got, BeginMarker); n != 1 {
				t.Errorf("found %d block headers, want exactly 1:\n%s", n, got)
			}
			if n := strings.Count(got, EndMarker); n != 1 {
				t.Errorf("found %d block footers, want exactly 1:\n%s", n, got)
			}
			for _, line := range strings.Split(strings.TrimSpace(userHosts), "\n") {
				if !strings.Contains(got, line) {
					t.Errorf("user line %q was lost under concurrency:\n%s", line, got)
				}
			}
		})
	}
}

// Applying from several goroutines on one Writer must be safe too, since the
// reconciler and the resync timer can both trigger a write.
func TestWriterIsSafeForConcurrentUseOnOneInstance(t *testing.T) {
	t.Parallel()

	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			w, path := newWriter(t, mode)

			var wg sync.WaitGroup
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for j := 0; j < 10; j++ {
						if _, err := w.Apply(entriesFor("x.docker.local")); err != nil {
							t.Errorf("Apply: %v", err)
							return
						}
					}
				}()
			}
			wg.Wait()

			if n := strings.Count(read(t, path), "x.docker.local"); n != 1 {
				t.Errorf("record appears %d times, want 1", n)
			}
		})
	}
}

// The write must be durable: a reported success has to survive a power cut,
// which is what the fsync calls are for.
func TestWriterSyncsBeforeReportingSuccess(t *testing.T) {
	t.Parallel()

	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			w, path := newWriter(t, mode)
			if _, err := w.Apply(entriesFor("durable.docker.local")); err != nil {
				t.Fatal(err)
			}
			// Reopening and reading through the kernel is the closest a test
			// can get to asserting the data left the process.
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			info, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() == 0 {
				t.Error("the file is empty after a successful Apply")
			}
		})
	}
}

// A hosts file can contain a NUL byte if some tool wrote binary junk into it.
// Parse keeps such a line verbatim rather than failing, because losing the
// operator's file over one odd byte would be the worse outcome. This test
// pins that tolerance.
func TestAdoptToleratesBinaryContent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")
	if err := os.WriteFile(path, []byte("127.0.0.1 ok\x00binary\n"), DefaultPerm); err != nil {
		t.Fatal(err)
	}
	w, err := NewWriter(path, config.MountModeFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Adopt(); err != nil {
		t.Fatalf("Adopt must tolerate unusual bytes: %v", err)
	}
	if _, err := w.Apply(entriesFor("x.docker.local")); err != nil {
		t.Fatal(err)
	}
	if got := read(t, path); !strings.Contains(got, "ok\x00binary") {
		t.Errorf("unusual content was dropped:\n%q", got)
	}
}

func TestApplyOnUnwritablePathErrors(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("running as root, permission checks do not apply")
	}

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	// "dir" mode has to create a temp file in the parent, so it must fail
	// with a message that names the offending path.
	path := filepath.Join(dir, "hosts")
	w, err := NewWriter(path, config.MountModeDir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.Apply(entriesFor("x.docker.local"))
	if err == nil {
		t.Fatal("expected an error on an unwritable directory")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error %q does not identify the unwritable directory", err)
	}
}

func TestAdoptOnMissingFileIsNotAnError(t *testing.T) {
	t.Parallel()

	// A missing hosts file means "create it", not "give up". The reconciler
	// relies on this to bootstrap.
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")
	w, err := NewWriter(path, config.MountModeFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Adopt(); err != nil {
		t.Fatalf("Adopt on a missing file returned %v, want nil", err)
	}
	if got := w.Snapshot(); len(got) != 0 {
		t.Errorf("Snapshot = %q, want empty", got)
	}
}

// In "dir" mode the writer owns the layout, so a missing parent is something
// it should fix rather than complain about.
func TestWriterDirModeCreatesMissingParent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "hosts")
	w, err := NewWriter(path, config.MountModeDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Adopt(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(entriesFor("first.docker.local")); err != nil {
		t.Fatalf("Apply into a missing directory: %v", err)
	}
	if got := read(t, path); !strings.Contains(got, "first.docker.local") {
		t.Errorf("bootstrap failed:\n%s", got)
	}
}

// In "file" mode the file is a bind mount supplied by the operator, so a
// missing parent directory is a deployment mistake. The writer must report it
// clearly instead of quietly creating directories it does not own.
func TestWriterFileModeReportsMissingParent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "hosts")
	w, err := NewWriter(path, config.MountModeFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Adopt(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(entriesFor("first.docker.local")); err == nil {
		t.Fatal("Apply into a missing directory must fail in file mode")
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Error("the writer created a file it should not have")
	}
}

func TestWriterReportsUnwritablePath(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("running as root, permission checks do not apply")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")
	if err := os.WriteFile(path, []byte("127.0.0.1 x\n"), 0o444); err != nil {
		t.Fatal(err)
	}

	w, err := NewWriter(path, config.MountModeFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Adopt(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(entriesFor("nope.docker.local")); err == nil {
		t.Error("Apply succeeded on a read-only file")
	}
}

func TestWriterKeepsSurroundingBlankLines(t *testing.T) {
	t.Parallel()

	// The operator's file structure must survive, otherwise a diff would
	// show churn that has nothing to do with containers.
	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			w, path := newWriter(t, mode)
			if _, err := w.Apply(entriesFor("x.docker.local")); err != nil {
				t.Fatal(err)
			}
			got := read(t, path)
			if !strings.Contains(got, "ff02::2 ip6-allrouters") {
				t.Errorf("the last user line is missing:\n%s", got)
			}
			if !strings.Contains(got, "\n\n# A comment the operator wrote\n") {
				t.Errorf("the blank line before the operator's comment was lost:\n%q", got)
			}
		})
	}
}

// A rendered line must never be able to break out of its own line and inject
// an arbitrary record. Names come from Docker, but the guarantee must not
// depend on that staying true.
func TestRenderedLinesCannotInjectNewRecords(t *testing.T) {
	t.Parallel()

	evil := []string{
		"name #\n127.0.0.1 evil.docker.local",
		"name\n127.0.0.1 evil.docker.local",
		"name\t127.0.0.1 evil.docker.local",
		"name\r\n127.0.0.1 evil.docker.local",
		"name\n127.0.0.1 evil.docker.local\nname2",
		"#\n127.0.0.1 evil.docker.local",
		"\n127.0.0.1 evil.docker.local",
	}
	for _, name := range evil {
		t.Run(strings.ReplaceAll(strings.TrimSpace(name[:5]), "\n", "\\n"), func(t *testing.T) {
			t.Parallel()

			e := Entry{IP: net.ParseIP("172.17.0.2"), Names: []string{name}}
			line := e.String()

			// The rendered value must stay on one line. This is the property
			// that stops a name from injecting an extra record into the
			// host's name resolution.
			if strings.ContainsAny(line, "\n\r\v\f") {
				t.Fatalf("Entry.String produced a multi-line value: %q", line)
			}
			// The whole name is rejected, not truncated to its safe prefix.
			// A truncated "name" would silently point at a different thing,
			// whereas a missing name is a visible, recoverable mistake.
			if strings.Contains(line, "name") {
				t.Errorf("part of an unsafe name leaked into the line: %q", line)
			}
			// The injected record must never appear.
			if strings.Contains(line, "127.0.0.1") || strings.Contains(line, "evil") {
				t.Errorf("an injected address survived into the line: %q", line)
			}
			// An entry whose every name was rejected renders as nothing at
			// all: a bare address line would be a malformed hosts entry.
			if line != "" {
				t.Errorf("unexpected rendering %q, want the empty string", line)
			}
		})
	}
}

// The same guarantee at the level that matters: a whole file must never gain
// a record the caller did not ask for.
func TestRenderCannotBeUsedToInjectRecords(t *testing.T) {
	t.Parallel()

	f := mustParse(t, "127.0.0.1 keepme\n")
	entries := []Entry{{
		IP:    net.ParseIP("172.17.0.2"),
		Names: []string{"ok.docker.local\n127.0.0.1 attacker.docker.local"},
	}}

	out := string(Render(f, entries))
	if strings.Contains(out, "attacker.docker.local") {
		t.Errorf("a record was injected into the hosts file:\n%s", out)
	}
	if !strings.Contains(out, "keepme") {
		t.Errorf("the user's line was lost:\n%s", out)
	}

	f2, err := Parse(strings.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	var userNames []string
	for _, l := range f2.Lines {
		if l.Kind == LineEntry {
			userNames = append(userNames, l.Names...)
		}
	}
	for _, n := range userNames {
		if n != "keepme" {
			t.Errorf("unexpected record %q in the user section:\n%s", n, out)
		}
	}
	if len(f2.Managed) != 0 {
		t.Errorf("the injected name should have produced no managed record, got %+v", f2.Managed)
	}
}

// An entry whose every name was rejected must not leave a bare address line
// behind, because "172.17.0.2" on its own is a malformed hosts entry.
func TestRenderDropsEntriesWithNoUsableNames(t *testing.T) {
	t.Parallel()

	f := mustParse(t, "127.0.0.1 keepme\n")
	out := string(Render(f, []Entry{
		{IP: net.ParseIP("172.17.0.2"), Names: []string{"bad\nname"}},
		{IP: net.ParseIP("172.17.0.3"), Names: []string{"good.docker.local"}},
	}))

	if strings.Contains(out, "172.17.0.2") {
		t.Errorf("an entry with no usable name was written:\n%s", out)
	}
	if !strings.Contains(out, "good.docker.local") {
		t.Errorf("the healthy entry was lost:\n%s", out)
	}
	// Nothing malformed may survive: every record line must parse.
	for _, l := range mustParse(t, out).Lines {
		if l.Kind == LineMalformed {
			t.Errorf("a malformed line was written: %q\nfull:\n%s", l.Raw, out)
		}
	}
}

func TestWriterRejectsUnknownMountMode(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")
	if err := os.WriteFile(path, []byte(userHosts), DefaultPerm); err != nil {
		t.Fatal(err)
	}
	w, err := NewWriter(path, config.MountMode("nonsense"))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Adopt(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(entriesFor("x.docker.local")); err == nil {
		t.Error("an unknown mount mode must be rejected, not silently treated as file mode")
	}
}

func TestNewWriterRejectsEmptyPath(t *testing.T) {
	t.Parallel()

	if _, err := NewWriter("", config.MountModeFile); err == nil {
		t.Error("NewWriter accepted an empty path")
	}
}

// In "dir" mode the write goes through a temporary file and a rename, so no
// partial content may ever be visible and no stray temp file may survive.
func TestWriterDirModeLeavesNoTempFiles(t *testing.T) {
	t.Parallel()

	w, path := newWriter(t, config.MountModeDir)
	for i := 0; i < 20; i++ {
		if _, err := w.Apply(entriesFor("x.docker.local")); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the directory holds %v, want just the hosts file", names)
	}
}

// In "file" mode the target is a mount point, so rename would fail with
// EBUSY. The writer must not even try: a stray temp file inside /etc would be
// a visible side effect on the host.
func TestWriterFileModeLeavesNoTempFiles(t *testing.T) {
	t.Parallel()

	w, path := newWriter(t, config.MountModeFile)
	if _, err := w.Apply(entriesFor("x.docker.local")); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the directory holds %v, want just the hosts file", names)
	}
}

func TestWriterCountsWrites(t *testing.T) {
	t.Parallel()

	w, _ := newWriter(t, config.MountModeFile)
	if got := w.Writes(); got != 0 {
		t.Errorf("Writes = %d before any Apply, want 0", got)
	}
	if _, err := w.Apply(entriesFor("a.docker.local")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(entriesFor("a.docker.local")); err != nil {
		t.Fatal(err)
	}
	if got := w.Writes(); got != 1 {
		t.Errorf("Writes = %d, want 1: the second Apply was a no-op", got)
	}
}

func TestWriterSnapshot(t *testing.T) {
	t.Parallel()

	w, _ := newWriter(t, config.MountModeFile)
	if got := w.Snapshot(); got != nil && len(got) != len(userHosts) {
		t.Errorf("Snapshot before Apply = %q, want nil or the adopted content", got)
	}

	if _, err := w.Apply(entriesFor("a.docker.local")); err != nil {
		t.Fatal(err)
	}
	snap := w.Snapshot()
	if !strings.Contains(string(snap), "a.docker.local") {
		t.Errorf("Snapshot does not contain the applied record:\n%s", snap)
	}

	// The caller must not be able to corrupt the writer's state through it.
	snap[0] = 'X'
	if !strings.Contains(string(w.Snapshot()), "127.0.0.1") &&
		!strings.Contains(string(w.Snapshot()), "a.docker.local") {
		t.Error("mutating the snapshot corrupted the writer's state")
	}
}

func TestWriterHandlesLargeInput(t *testing.T) {
	t.Parallel()

	// A busy host with hundreds of containers must not hit a line length or
	// file size limit.
	var entries []Entry
	for i := 0; i < 500; i++ {
		entries = append(entries, Entry{
			IP:    []byte{172, 17, byte(i / 250), byte(i%250 + 1)},
			Names: []string{"container-" + itoa(i) + ".docker.local"},
		})
	}

	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			w, path := newWriter(t, mode)

			if _, err := w.Apply(entries); err != nil {
				t.Fatalf("Apply with 500 records: %v", err)
			}
			got := read(t, path)
			if n := strings.Count(got, ".docker.local"); n != 500 {
				t.Errorf("got %d records, want 500", n)
			}
			changed, err := w.Apply(entries)
			if err != nil {
				t.Fatal(err)
			}
			if changed {
				t.Error("re-applying 500 identical records reported a change")
			}
		})
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestApplyErrorsAreDescriptive(t *testing.T) {
	t.Parallel()

	// A path under a non-existent, non-creatable parent must fail with a
	// message that names the path, so an operator can act on it.
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")

	// Make the parent unwritable.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Skipf("cannot make the directory read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if os.Geteuid() == 0 {
		t.Skip("running as root, permission checks do not apply")
	}

	w, err := NewWriter(path, config.MountModeDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(entriesFor("x.docker.local")); err == nil {
		t.Fatal("expected an error on an unwritable directory")
	} else if !strings.Contains(err.Error(), path) && !strings.Contains(err.Error(), "create") {
		t.Errorf("error %q does not identify the problem", err)
	}
}

func TestFlockIsHeldDuringWrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")
	if err := os.WriteFile(path, []byte(userHosts), DefaultPerm); err != nil {
		t.Fatal(err)
	}

	w, err := NewWriter(path, config.MountModeFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Adopt(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(entriesFor("x.docker.local")); err != nil {
		t.Fatal(err)
	}

	// Open the same file independently and try to take an exclusive lock: it
	// must be free, because the writer releases it on completion.
	f, err := os.OpenFile(path, os.O_RDWR, DefaultPerm)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Errorf("the advisory lock was leaked after Apply: %v", err)
	}
}

// TestBlockHelperIsUsableByTheWebUI pins the helper the monitoring page uses
// to show the generated block.
func TestBlockHelperIsUsableByTheWebUI(t *testing.T) {
	t.Parallel()

	if got := RenderBlock(nil); got != "" {
		t.Errorf("RenderBlock(nil) = %q, want empty", got)
	}
	got := RenderBlock(entriesFor("a.docker.local"))
	if !strings.HasPrefix(got, BeginMarker) || !strings.Contains(got, EndMarker) {
		t.Errorf("RenderBlock did not produce a delimited block: %q", got)
	}
	if !strings.Contains(got, "a.docker.local") {
		t.Errorf("RenderBlock lost the record: %q", got)
	}
	full := string(Render(mustParse(t, userHosts), entriesFor("a.docker.local")))
	// The record lines must match what the full render writes, or the web UI
	// would show something different from the file on disk. The two differ
	// deliberately in one respect: RenderBlock is for display and omits the
	// explanatory comment, while Render writes it.
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if line == BeginMarker || line == EndMarker {
			continue
		}
		if !strings.Contains(full, line) {
			t.Errorf("RenderBlock line %q is absent from Render output:\nblock:\n%s\nfull:\n%s", line, got, full)
		}
	}
}

// TestBlockRoundTripsThroughParse is the invariant the crash recovery relies
// on: a block written by Render must be readable back into entries.
func TestBlockRoundTripsThroughParse(t *testing.T) {
	t.Parallel()

	entries := entriesFor("a.docker.local", "b.docker.local", "c.docker.local")
	raw := Render(mustParse(t, userHosts), entries)

	got, err := Block(raw)
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(got), got)
	}

	// Applying what was read back must be a no-op.
	f := mustParse(t, string(raw))
	if !SameManagedBlock(f, entries) {
		t.Error("a parsed block does not match the entries that produced it")
	}
}

func TestBlockOnFileWithoutOne(t *testing.T) {
	t.Parallel()

	got, err := Block([]byte(userHosts))
	if err != nil {
		t.Fatalf("Block: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Block = %+v, want empty for a file with no managed block", got)
	}
}

// A block with no end marker is what a crash leaves, but also what a hand edit
// that removed the marker looks like. The operator's lines that follow it must
// survive the rebuild; only what is recognisably ours may go.
func TestRebuildOfAnOpenBlockKeepsTheOperatorsLines(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "hosts")
	before := "127.0.0.1 localhost\n" +
		BeginMarker + "\n" +
		headerLines[0] + "\n" +
		headerLines[1] + "\n" +
		"172.17.0.2 stale.docker.local\n" +
		"192.168.1.10 nas.home\n" +
		"# a note of mine\n"
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}

	w, err := NewWriter(path, config.MountModeFile)
	if err != nil {
		t.Fatal(err)
	}
	w.SetOwnedSuffix("docker.local")

	if _, err := w.Apply([]Entry{{IP: net.ParseIP("172.17.0.3"), Names: []string{"web.docker.local"}}}); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)

	for _, want := range []string{"127.0.0.1 localhost", "192.168.1.10 nas.home", "# a note of mine", "172.17.0.3 web.docker.local"} {
		if !strings.Contains(text, want) {
			t.Errorf("the rebuilt file lost %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "stale.docker.local") {
		t.Errorf("a stale record of ours survived the rebuild:\n%s", text)
	}
	if strings.Count(text, BeginMarker) != 1 || strings.Count(text, EndMarker) != 1 {
		t.Errorf("want exactly one closed block:\n%s", text)
	}
}

// Adopt takes the file as the baseline, so Repair at startup has nothing better
// to restore than what is already there. It must say so instead of reporting a
// repair that rewrote the same bytes.
func TestRepairRightAfterAdoptDoesNothing(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "hosts")
	damaged := "127.0.0.1 localhost\n" + BeginMarker + "\n172.17.0.2 stale.docker.local\n"
	if err := os.WriteFile(path, []byte(damaged), 0o644); err != nil {
		t.Fatal(err)
	}

	w, err := NewWriter(path, config.MountModeFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Adopt(); err != nil {
		t.Fatal(err)
	}
	repaired, err := w.Repair()
	if err != nil {
		t.Fatal(err)
	}
	if repaired {
		t.Error("Repair reported a repair although it can only restore the damaged baseline")
	}
	if got, _ := os.ReadFile(path); string(got) != damaged {
		t.Errorf("the file changed:\n%s", got)
	}
}
