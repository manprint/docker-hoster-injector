package hostsfile

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mint/docker-hoster-injector/internal/config"
)

// operatorFiles are the shapes a real hosts file comes in. After the block has
// been added and removed again each one must be back to the operator's own
// bytes: the agent must leave no trace of having been there.
var operatorFiles = map[string]string{
	"typical":         userHosts,
	"empty":           "",
	"only newline":    "\n",
	"comments only":   "# nothing here\n# at all\n",
	"crlf":            "127.0.0.1\tlocalhost\r\n::1\tlocalhost\r\n",
	"trailing blanks": "127.0.0.1 localhost\n\n\n",
	"leading blanks":  "\n\n127.0.0.1 localhost\n",
	"no final eol":    "127.0.0.1 localhost",
	"tabs and spaces": "127.0.0.1\t  localhost   \t\n  # indented comment\n",
	"odd bytes":       "127.0.0.1 localhost\n\xff\xfe not utf8\n",
	"long line":       "127.0.0.1 " + strings.Repeat("a", 6<<20) + "\n",
}

// wantAfterRoundTrip is what the file must equal after add and remove. The
// only accepted difference is a final newline when the original lacked one.
func wantAfterRoundTrip(orig string) string {
	if orig != "" && !strings.HasSuffix(orig, "\n") {
		return orig + "\n"
	}
	return orig
}

func TestAddThenClearReturnsTheOperatorsBytes(t *testing.T) {
	t.Parallel()

	for _, mode := range modes {
		for name, orig := range operatorFiles {
			t.Run(string(mode)+"/"+name, func(t *testing.T) {
				t.Parallel()
				path := filepath.Join(t.TempDir(), "hosts")
				if err := os.WriteFile(path, []byte(orig), DefaultPerm); err != nil {
					t.Fatal(err)
				}
				w, err := NewWriter(path, mode)
				if err != nil {
					t.Fatal(err)
				}
				if err := w.Adopt(); err != nil {
					t.Fatal(err)
				}

				if _, err := w.Apply(entriesFor("a.docker.local", "b.docker.local")); err != nil {
					t.Fatalf("Apply: %v", err)
				}
				got := read(t, path)
				if !strings.HasPrefix(got, wantAfterRoundTrip(orig)) {
					t.Fatalf("the operator's part was altered while adding the block")
				}

				changed, err := w.Clear()
				if err != nil || !changed {
					t.Fatalf("Clear = %v, %v; want true, nil", changed, err)
				}
				if got := read(t, path); got != wantAfterRoundTrip(orig) {
					t.Fatalf("not a round trip:\n got %q\nwant %q", clip(got), clip(wantAfterRoundTrip(orig)))
				}

				// Idempotent: a second Clear must not touch the file.
				before, _ := os.Stat(path)
				time.Sleep(20 * time.Millisecond)
				changed, err = w.Clear()
				if err != nil || changed {
					t.Fatalf("second Clear = %v, %v; want false, nil", changed, err)
				}
				after, _ := os.Stat(path)
				if !after.ModTime().Equal(before.ModTime()) {
					t.Error("a no-op Clear rewrote the file")
				}
			})
		}
	}
}

func clip(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

func TestClearOnMissingFileOrNoBlockDoesNothing(t *testing.T) {
	t.Parallel()
	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()

			missing := filepath.Join(dir, "absent")
			w, err := NewWriter(missing, mode)
			if err != nil {
				t.Fatal(err)
			}
			if changed, err := w.Clear(); err != nil || changed {
				t.Fatalf("Clear on a missing file = %v, %v", changed, err)
			}
			if _, err := os.Stat(missing); err == nil {
				t.Fatal("Clear created the hosts file")
			}

			w2, path := newWriter(t, mode)
			if changed, err := w2.Clear(); err != nil || changed {
				t.Fatalf("Clear without a block = %v, %v", changed, err)
			}
			if read(t, path) != userHosts {
				t.Fatal("Clear altered a file that had no block")
			}
		})
	}
}

func TestClearRemovesABlockLeftOpenByACrash(t *testing.T) {
	t.Parallel()
	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			w, path := newWriter(t, mode)
			w.SetOwnedSuffix("docker.local")

			// A block without its end marker, as a killed write leaves it.
			broken := userHosts + BeginMarker + "\n127.0.0.2\tghost.docker.local\n"
			if err := os.WriteFile(path, []byte(broken), DefaultPerm); err != nil {
				t.Fatal(err)
			}
			if _, err := w.Clear(); err != nil {
				t.Fatalf("Clear: %v", err)
			}
			got := read(t, path)
			if got != userHosts {
				t.Fatalf("leftovers after Clear:\n%s", got)
			}
		})
	}
}

// A shrinking write must not leave the tail of the longer old content behind
// the closing marker.
func TestShrinkingWriteLeavesNothingAfterTheEndMarker(t *testing.T) {
	t.Parallel()
	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			w, path := newWriter(t, mode)
			var names []string
			for i := 0; i < 50; i++ {
				names = append(names, "svc"+string(rune('a'+i%26))+string(rune('a'+i/26))+".docker.local")
			}
			if _, err := w.Apply(entriesFor(names...)); err != nil {
				t.Fatal(err)
			}
			if _, err := w.Apply(entriesFor("one.docker.local")); err != nil {
				t.Fatal(err)
			}
			got := read(t, path)
			if !strings.HasSuffix(got, EndMarker+"\n") {
				t.Fatalf("content after the end marker:\n%s", got)
			}
			if strings.Count(got, BeginMarker) != 1 || strings.Count(got, EndMarker) != 1 {
				t.Fatalf("markers duplicated:\n%s", got)
			}
		})
	}
}

func TestZeroEntriesRemovesTheBlockEntirely(t *testing.T) {
	t.Parallel()
	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			w, path := newWriter(t, mode)
			if _, err := w.Apply(entriesFor("a.docker.local")); err != nil {
				t.Fatal(err)
			}
			if _, err := w.Apply(nil); err != nil {
				t.Fatal(err)
			}
			if got := read(t, path); got != userHosts {
				t.Fatalf("an empty block was left behind:\n%s", got)
			}
		})
	}
}

func TestCleanStaleRemovesOnlyOldTempFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "hosts")
	if err := os.WriteFile(path, []byte(userHosts), DefaultPerm); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, TempPrefix+"old")
	young := filepath.Join(dir, TempPrefix+"young")
	other := filepath.Join(dir, "unrelated")
	for _, f := range []string{old, young, other} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	w, err := NewWriter(path, config.MountModeDir)
	if err != nil {
		t.Fatal(err)
	}
	n, err := w.CleanStale()
	if err != nil || n != 1 {
		t.Fatalf("CleanStale = %d, %v; want 1, nil", n, err)
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("the old temp file survived")
	}
	for _, f := range []string{young, other, path} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s was removed: %v", filepath.Base(f), err)
		}
	}

	// File mode never creates temp files, so it never looks for them.
	wf, _ := NewWriter(path, config.MountModeFile)
	if n, err := wf.CleanStale(); err != nil || n != 0 {
		t.Fatalf("file mode CleanStale = %d, %v", n, err)
	}
}

func TestAtomicWriteKeepsSymlinkAndMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real-hosts")
	link := filepath.Join(dir, "hosts")
	if err := os.WriteFile(real, []byte(userHosts), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(real, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	w, err := NewWriter(link, config.MountModeDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Adopt(); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Apply(entriesFor("a.docker.local")); err != nil {
		t.Fatal(err)
	}

	li, err := os.Lstat(link)
	if err != nil || li.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was replaced by a regular file: %v %v", li, err)
	}
	ri, _ := os.Stat(real)
	if ri.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", ri.Mode().Perm())
	}
	if !bytes.Contains([]byte(read(t, real)), []byte("a.docker.local")) {
		t.Error("the target was not written")
	}
}

func FuzzAddThenClearRoundTrip(f *testing.F) {
	for _, s := range operatorFiles {
		if len(s) < 1<<16 {
			f.Add(s)
		}
	}
	f.Fuzz(func(t *testing.T, orig string) {
		// A file that already contains the markers is not an operator file
		// in the sense of this property.
		if strings.Contains(orig, "docker-hoster-injector") {
			t.Skip()
		}
		path := filepath.Join(t.TempDir(), "hosts")
		if err := os.WriteFile(path, []byte(orig), DefaultPerm); err != nil {
			t.Fatal(err)
		}
		w, err := NewWriter(path, config.MountModeDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Adopt(); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Apply(entriesFor("a.docker.local")); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Clear(); err != nil {
			t.Fatal(err)
		}
		if got := read(t, path); got != wantAfterRoundTrip(orig) {
			t.Fatalf("round trip changed the file:\n got %q\nwant %q", got, wantAfterRoundTrip(orig))
		}
	})
}
