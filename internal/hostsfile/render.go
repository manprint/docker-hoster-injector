package hostsfile

import (
	"bytes"
	"fmt"
	"strings"
)

// headerLines are the comments written right after the BeginMarker. They are
// kept in one place so that recovery can recognise them as its own.
var headerLines = [...]string{
	"# Generated automatically. Edits are overwritten on the next",
	"# container lifecycle event. Safe to delete: it is rebuilt.",
}

// Render produces the complete file text: the user part verbatim, followed by
// the managed block.
//
// The block always goes last. That is the property that makes a crash
// survivable: the section of the file the host's resolver depends on is never
// rewritten, only appended to, so a partially completed write can lose the
// container records but can never damage the operator's own entries.
func Render(f *File, managed []Entry) []byte {
	var buf bytes.Buffer

	for _, l := range f.Lines {
		buf.WriteString(l.Raw)
		buf.WriteByte('\n')
	}

	// The user part is written exactly as it was read, and nothing is added to
	// it or taken away. That is what makes adding the block and later removing
	// it a round trip: the file returns to the operator's own bytes.
	if len(managed) > 0 {
		buf.WriteString(BeginMarker)
		buf.WriteByte('\n')
		for _, h := range headerLines {
			buf.WriteString(h)
			buf.WriteByte('\n')
		}
		for _, e := range GroupByAddr(managed) {
			// GroupByAddr drops entries with nothing writable, so an empty
			// result here would be a bug rather than a case to guard.
			line := e.String()
			if line == "" {
				continue
			}
			buf.WriteString(line)
			buf.WriteByte('\n')
		}
		buf.WriteString(EndMarker)
		buf.WriteByte('\n')
	}

	return buf.Bytes()
}

// RenderBlock renders just the managed block, for inspection in the web UI.
func RenderBlock(managed []Entry) string {
	if len(managed) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(BeginMarker + "\n")
	for _, e := range GroupByAddr(managed) {
		if line := e.String(); line != "" {
			b.WriteString(line + "\n")
		}
	}
	b.WriteString(EndMarker + "\n")
	return b.String()
}

// Block extracts the managed entries from raw file text.
func Block(raw []byte) ([]Entry, error) {
	f, err := Parse(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse hosts file: %w", err)
	}
	return f.Managed, nil
}

// SameManagedBlock reports whether desired would render a managed block
// identical to the one already present.
//
// This is the guard that lets the writer skip touching the file when nothing
// actually changed, so it runs on every reconcile. It compares the two grouped
// entry sets directly instead of rendering both to text and diffing the
// strings: rendering twice was measurably the most allocation-heavy step in
// the steady state, and the answer is available without building a single
// string.
func SameManagedBlock(f *File, desired []Entry) bool {
	want := GroupByAddr(desired)
	got := GroupByAddr(f.Managed)

	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if !want[i].IP.Equal(got[i].IP) ||
			want[i].Comment != got[i].Comment ||
			!equalNames(want[i].Names, got[i].Names) {
			return false
		}
	}
	return true
}

func equalNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
