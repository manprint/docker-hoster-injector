package hostsfile

import (
	"net"
	"reflect"
	"strings"
	"testing"
)

const userHosts = `127.0.0.1	localhost
127.0.0.1	localhost.localdomain
::1     ip6-localhost ip6-loopback
fe00::0 ip6-localnet
ff02::1 ip6-allnodes
ff02::2 ip6-allrouters

# A comment the operator wrote
192.168.1.10   nas.home   # trailing comment
10.0.0.5      printer printer.lan
`

func mustParse(t *testing.T, raw string) *File {
	t.Helper()
	f, err := Parse(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return f
}

func TestParsePreservesUserContent(t *testing.T) {
	t.Parallel()

	f := mustParse(t, userHosts)

	if f.HadBlock {
		t.Error("HadBlock = true for a file with no managed block")
	}
	if len(f.Managed) != 0 {
		t.Errorf("Managed = %+v, want empty", f.Managed)
	}
	if got, want := len(f.Lines), 10; got != want {
		t.Fatalf("got %d user lines, want %d", got, want)
	}

	// Every line must come back byte for byte, including the original
	// mixed indentation, because that is what the operator's file contains.
	// Every non-blank line must come back byte for byte, including the
	// original mixed indentation, because that is what the operator's file
	// contains.
	for i, l := range f.Lines {
		if l.Kind == LineBlank {
			continue // a blank line legitimately has no content
		}
		if l.Raw == "" {
			t.Errorf("line %d lost its raw text", i)
		}
	}
	if got := f.Lines[0].Raw; got != "127.0.0.1\tlocalhost" {
		t.Errorf("first line = %q, want the tab separated original", got)
	}
	if got := f.Lines[8].Raw; got != "192.168.1.10   nas.home   # trailing comment" {
		t.Errorf("line 8 = %q, want the original spacing preserved", got)
	}
}

func TestParseClassifiesLines(t *testing.T) {
	t.Parallel()

	f := mustParse(t, "127.0.0.1 one\n\n# comment\n::1 ip6\nnot-an-ip name\nonlyonename\n")

	want := []LineKind{
		LineEntry,   // 127.0.0.1 one
		LineBlank,   //
		LineComment, // # comment
		LineEntry,   // ::1 ip6
		LineMalformed,
		LineMalformed,
	}
	if len(f.Lines) != len(want) {
		t.Fatalf("got %d lines, want %d", len(f.Lines), len(want))
	}
	for i, k := range want {
		if f.Lines[i].Kind != k {
			t.Errorf("line %d kind = %v, want %v (%q)", i, f.Lines[i].Kind, k, f.Lines[i].Raw)
		}
	}
}

// A malformed line must never be dropped: it belongs to the operator, and
// silently deleting content from someone's /etc/hosts is unacceptable.
func TestParseKeepsMalformedLines(t *testing.T) {
	t.Parallel()

	raw := "127.0.0.1 good\nthis is not a hosts entry\n#ok\ngarbage !!! more\n"
	f := mustParse(t, raw)

	rendered := Render(f, nil)
	if !strings.Contains(string(rendered), "this is not a hosts entry") {
		t.Errorf("a malformed line was lost:\n%s", rendered)
	}
	if !strings.Contains(string(rendered), "garbage !!! more") {
		t.Errorf("a garbage line was lost:\n%s", rendered)
	}
}

func TestParseExtractsManagedBlock(t *testing.T) {
	t.Parallel()

	raw := userHosts + `
` + BeginMarker + `
127.0.0.1 nginx.docker.local
172.17.0.2 web.docker.local
` + EndMarker + `
10.0.0.99 after.docker.local
`

	f := mustParse(t, raw)

	if !f.HadBlock {
		t.Error("HadBlock = false, want true")
	}
	if len(f.Managed) != 2 {
		t.Fatalf("got %d managed entries, want 2: %+v", len(f.Managed), f.Managed)
	}
	if got := f.Managed[0].Names[0]; got != "nginx.docker.local" {
		t.Errorf("first managed name = %q, want nginx.docker.local", got)
	}
	if f.BlockTruncated {
		t.Error("BlockTruncated = true for a well formed block")
	}

	// Content after the block belongs to the operator and must survive.
	if !strings.Contains(string(Render(f, f.Managed)), "after.docker.local") {
		t.Error("content after the managed block was lost")
	}
}

// This is the shape a crash leaves behind when the process dies between
// writing the header and the footer.
func TestParseDetectsTruncatedBlock(t *testing.T) {
	t.Parallel()

	raw := userHosts + "\n" + BeginMarker + "\n127.0.0.1 nginx.docker.local\n"
	f := mustParse(t, raw)

	if !f.BlockTruncated {
		t.Fatal("BlockTruncated = false, want true for a block with no EndMarker")
	}
	if !f.HadBlock {
		t.Error("HadBlock = false, want true")
	}
}

// A stray EndMarker with no opening one is user content that happens to match,
// not the tail of our block.
func TestParseKeepsStrayEndMarker(t *testing.T) {
	t.Parallel()

	raw := "127.0.0.1 x\n" + EndMarker + "\n127.0.0.1 y\n"
	f := mustParse(t, raw)

	if f.HadBlock {
		t.Error("HadBlock = true for a stray EndMarker")
	}
	if got := Render(f, nil); !strings.Contains(string(got), EndMarker) {
		t.Errorf("the stray EndMarker was swallowed:\n%s", got)
	}
}

func TestParseHandlesNoTrailingNewline(t *testing.T) {
	t.Parallel()

	f := mustParse(t, "127.0.0.1 one")
	if len(f.Lines) != 1 || f.Lines[0].Kind != LineEntry {
		t.Fatalf("unexpected parse of a file without a trailing newline: %+v", f.Lines)
	}
}

func TestParseHandlesCRLF(t *testing.T) {
	t.Parallel()

	// A hosts file edited on a DOS system, or by a careless tool, may carry
	// carriage returns. The raw text is preserved but classification must
	// still work.
	f := mustParse(t, "127.0.0.1 one\r\n127.0.0.1 two\r\n")
	if len(f.Lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(f.Lines))
	}
	for i, l := range f.Lines {
		if l.Kind != LineEntry {
			t.Errorf("line %d with CRLF classified as %v", i, l.Kind)
		}
	}
}

func TestRenderAppendsBlockAndIsIdempotent(t *testing.T) {
	t.Parallel()

	f := mustParse(t, userHosts)
	entries := []Entry{
		{IP: net.ParseIP("127.0.0.1"), Names: []string{"nginx.docker.local"}},
		{IP: net.ParseIP("172.17.0.2"), Names: []string{"web.docker.local"}},
	}

	first := Render(f, entries)

	// Re-parsing and re-rendering must produce identical bytes, otherwise
	// every reconcile would rewrite the file and its mtime would churn.
	f2 := mustParse(t, string(first))
	second := Render(f2, f2.Managed)
	if string(first) != string(second) {
		t.Errorf("render is not idempotent:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}

	// And applying the same entries again must be a no-op.
	if !SameManagedBlock(f2, entries) {
		t.Error("SameManagedBlock = false for identical entries")
	}
}

func TestRenderWithNoEntriesOmitsBlock(t *testing.T) {
	t.Parallel()

	f := mustParse(t, userHosts)
	out := Render(f, nil)

	if strings.Contains(string(out), BeginMarker) {
		t.Errorf("an empty entry set still emitted a block:\n%s", out)
	}
	if !strings.Contains(string(out), "nas.home") {
		t.Error("user content was lost when the block became empty")
	}
}

func TestRenderKeepsBlockAtTheEnd(t *testing.T) {
	t.Parallel()

	// The block must come after all user content: a crash then costs at most
	// the container records, never the operator's own entries.
	raw := "127.0.0.1 first\n" + BeginMarker + "\n127.0.0.1 old.docker.local\n" + EndMarker + "\n"
	f := mustParse(t, raw)

	out := string(Render(f, []Entry{{IP: net.ParseIP("127.0.0.1"), Names: []string{"new.docker.local"}}}))
	blockAt := strings.Index(out, BeginMarker)
	firstAt := strings.Index(out, "first")
	if blockAt < firstAt {
		t.Errorf("the managed block precedes user content:\n%s", out)
	}
	if strings.Contains(out, "old.docker.local") {
		t.Errorf("the previous managed content was not replaced:\n%s", out)
	}
	if !strings.Contains(out, "127.0.0.1 first") {
		t.Errorf("user content was lost:\n%s", out)
	}
}

func TestGroupByAddr(t *testing.T) {
	t.Parallel()

	got := GroupByAddr([]Entry{
		{IP: net.ParseIP("127.0.0.1"), Names: []string{"a.docker.local"}},
		{IP: net.ParseIP("127.0.0.1"), Names: []string{"b.docker.local"}},
		{IP: net.ParseIP("127.0.0.1"), Names: []string{"a.docker.local"}},
		{IP: net.ParseIP("172.17.0.2"), Names: []string{"c.docker.local"}},
		{IP: nil, Names: []string{"dropped.docker.local"}},
		{IP: net.ParseIP("10.0.0.1")},
	})

	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(got), got)
	}
	if got[0].IP.String() != "127.0.0.1" {
		t.Errorf("first address = %s, want 127.0.0.1", got[0].IP)
	}
	if want := []string{"a.docker.local", "b.docker.local"}; !reflect.DeepEqual(got[0].Names, want) {
		t.Errorf("first names = %q, want %q", got[0].Names, want)
	}
	if got[1].IP.String() != "172.17.0.2" {
		t.Errorf("second address = %s, want 172.17.0.2", got[1].IP)
	}
}

// Grouping must be order stable, or the same state would render differently
// on two consecutive reconciles.
func TestGroupByAddrIsStable(t *testing.T) {
	t.Parallel()

	in := []Entry{
		{IP: net.ParseIP("127.0.0.1"), Names: []string{"a.docker.local"}},
		{IP: net.ParseIP("172.17.0.2"), Names: []string{"b.docker.local"}},
		{IP: net.ParseIP("127.0.0.1"), Names: []string{"c.docker.local"}},
	}
	first := GroupByAddr(in)
	for i := 0; i < 20; i++ {
		if !reflect.DeepEqual(GroupByAddr(in), first) {
			t.Fatalf("GroupByAddr is not stable at iteration %d", i)
		}
	}
}

func TestEntryString(t *testing.T) {
	t.Parallel()

	e := Entry{
		IP:      net.ParseIP("172.17.0.2"),
		Names:   []string{"web.docker.local", "web-1.docker.local"},
		Comment: "# container web",
	}
	got := e.String()
	want := "172.17.0.2 web.docker.local web-1.docker.local\t# container web"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}

	// The rendered line must parse back into the same entry, which is the
	// real invariant: write then read must round-trip.
	back, ok := parseEntry(got)
	if !ok {
		t.Fatalf("the rendered line does not parse back: %q", got)
	}
	if !back.IP.Equal(e.IP) || !reflect.DeepEqual(back.Names, e.Names) || back.Comment != e.Comment {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", back, e)
	}
}

func TestParseSplitsInlineComment(t *testing.T) {
	t.Parallel()

	e, ok := parseEntry("10.0.0.1  host.local  # note here")
	if !ok {
		t.Fatal("parseEntry failed")
	}
	if want := []string{"host.local"}; !reflect.DeepEqual(e.Names, want) {
		t.Errorf("names = %q, want %q", e.Names, want)
	}
	if e.Comment != "# note here" {
		t.Errorf("comment = %q, want %q", e.Comment, "# note here")
	}
}

func TestParseRejectsEntriesWithoutAddressOrName(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"",
		"   ",
		"onlyonename",
		"999.999.999.999 name",
		"host.local",
		"# just a comment",
		"127.0.0.1",
	} {
		if _, ok := parseEntry(raw); ok {
			t.Errorf("parseEntry(%q) accepted a malformed line", raw)
		}
	}
}
