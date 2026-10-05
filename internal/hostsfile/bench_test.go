// Benchmarks for the hosts file: parsing, rendering and the steady-state
// comparison that decides whether a write is needed at all.
package hostsfile

import (
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/manprint/docker-hoster-injector/internal/config"
)

// buildSampleFile returns a hosts file resembling a real one: operator
// content, plus a managed block with a realistic number of records.
func buildSampleFile(records int) string {
	var b strings.Builder
	b.WriteString(userHosts)
	b.WriteString(BeginMarker)
	b.WriteByte('\n')
	for i := 0; i < records; i++ {
		b.WriteString("172.17.0.")
		b.WriteString(strconv.Itoa(i%250 + 1))
		b.WriteByte(' ')
		b.WriteString("container-")
		b.WriteString(strconv.Itoa(i))
		b.WriteString(".docker.local")
		b.WriteByte('\n')
	}
	b.WriteString(EndMarker)
	b.WriteByte('\n')
	return b.String()
}

// buildEntries returns the entry set matching buildSampleFile.
func buildEntries(n int) []Entry {
	out := make([]Entry, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Entry{
			IP:    net.IPv4(172, 17, 0, byte(i%250+1)),
			Names: []string{"container-" + strconv.Itoa(i) + ".docker.local"},
		})
	}
	return out
}

// BenchmarkParse measures the cost of reading a hosts file, which happens on
// every reconcile because the operator may have edited it in the meantime.
func BenchmarkParse(b *testing.B) {
	for _, records := range []int{1, 20, 200} {
		raw := buildSampleFile(records)
		b.Run("records="+strconv.Itoa(records), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := Parse(strings.NewReader(raw)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSameManagedBlock measures the cheap path taken when nothing has
// changed. This runs on every reconcile, so its cost is paid continuously and
// it must not allocate.
func BenchmarkSameManagedBlock(b *testing.B) {
	for _, records := range []int{1, 20, 200} {
		raw := buildSampleFile(records)
		entries := buildEntries(records)
		f, err := Parse(strings.NewReader(raw))
		if err != nil {
			b.Fatal(err)
		}
		b.Run("records="+strconv.Itoa(records), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if !SameManagedBlock(f, entries) {
					b.Fatal("expected the blocks to match")
				}
			}
		})
	}
}

// BenchmarkRender measures producing the new file text.
func BenchmarkRender(b *testing.B) {
	for _, records := range []int{1, 20, 200} {
		raw := buildSampleFile(records)
		f, err := Parse(strings.NewReader(raw))
		if err != nil {
			b.Fatal(err)
		}
		entries := buildEntries(records)
		b.Run("records="+strconv.Itoa(records), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if len(Render(f, entries)) == 0 {
					b.Fatal("Render produced nothing")
				}
			}
		})
	}
}

// BenchmarkApplyNoChange measures the steady state: a reconcile where nothing
// moved. This is the overwhelmingly common case on an idle host, and it must
// not touch the disk at all.
func BenchmarkApplyNoChange(b *testing.B) {
	b.ReportAllocs()
	dir := b.TempDir()
	path := dir + "/hosts"
	entries := buildEntries(20)

	w, err := NewWriter(path, config.MountModeFile)
	if err != nil {
		b.Fatal(err)
	}
	if err := w.Adopt(); err != nil {
		b.Fatal(err)
	}
	if _, err := w.Apply(entries); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for b.Loop() {
		changed, err := w.Apply(entries)
		if err != nil {
			b.Fatal(err)
		}
		if changed {
			b.Fatal("an identical Apply reported a change")
		}
	}
}

// BenchmarkApplyChanged measures a reconcile that really rewrites the file,
// which is the write path including the fsync.
func BenchmarkApplyChanged(b *testing.B) {
	dir := b.TempDir()
	path := dir + "/hosts"
	w, err := NewWriter(path, config.MountModeFile)
	if err != nil {
		b.Fatal(err)
	}
	if err := w.Adopt(); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		// Alternate the content so every iteration is a genuine write.
		entries := buildEntries(20 + i%2)
		if _, err := w.Apply(entries); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGroupByAddr measures the grouping that keeps the file compact.
func BenchmarkGroupByAddr(b *testing.B) {
	entries := buildEntries(200)
	b.ReportAllocs()
	for b.Loop() {
		if len(GroupByAddr(entries)) == 0 {
			b.Fatal("GroupByAddr produced nothing")
		}
	}
}
