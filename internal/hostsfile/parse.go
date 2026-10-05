package hostsfile

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strings"
)

// Markers delimiting the block this agent owns. Everything outside them is
// user territory and is preserved byte for byte.
const (
	// BeginMarker opens the managed block. The wording tells an operator who
	// wrote the block and that editing it is pointless, because the next
	// reconcile overwrites it.
	BeginMarker = "# BEGIN docker-hoster-injector (managed, do not edit)"

	// EndMarker closes the managed block.
	EndMarker = "# END docker-hoster-injector"
)

// LineKind classifies one physical line of a hosts file.
type LineKind int

const (
	// LineBlank is an empty or whitespace-only line.
	LineBlank LineKind = iota
	// LineComment is a full-line comment.
	LineComment
	// LineEntry is "<address> <name>...".
	LineEntry
	// LineMalformed is anything that does not parse. It is kept verbatim
	// rather than dropped, because guessing at an operator's intent and
	// then deleting their line would be far worse than carrying it along.
	LineMalformed
)

// Line is one physical line, retaining the original text so that a
// re-render can reproduce the file exactly when nothing changed.
type Line struct {
	Kind  LineKind
	Raw   string
	IP    net.IP
	Names []string
}

// Entry is a set of names that resolve to one address. Several names on a
// single line is standard hosts syntax and keeps the file compact.
type Entry struct {
	IP      net.IP
	Names   []string
	Comment string
}

// File is a parsed hosts file: the user part, with the managed block removed,
// plus whatever the block contained.
type File struct {
	// Lines is every line outside the managed block, in original order.
	Lines []Line
	// Managed is the content of the block we previously wrote.
	Managed []Entry
	// HadBlock records whether a managed block was present. It is false when
	// the file has never been touched by this agent, which is how the writer
	// knows to append rather than replace.
	HadBlock bool
	// BlockTruncated is true when a BeginMarker had no matching EndMarker.
	// That is a crash artefact, and the caller is told to rebuild the block
	// rather than trust it.
	BlockTruncated bool
	// Orphans holds every line that followed an unterminated BeginMarker. They
	// cannot be told apart from the operator's own lines by position alone, so
	// the caller decides which ones are ours (see Writer.SetOwnedSuffix) and
	// keeps the rest. Empty unless BlockTruncated.
	Orphans []Line
}

// Parse reads a hosts file.
//
// It never fails on malformed input: a corrupt line is classified as
// LineMalformed and preserved. Only a read error is returned, because losing
// the user's file because of one odd line would be a far worse outcome than
// carrying the line along.
func Parse(r io.Reader) (*File, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read hosts file: %w", err)
	}

	f := &File{}
	inBlock := false

	// Lines are split on '\n' only. A '\r' stays in the line, so that a file
	// with CRLF endings is written back with CRLF endings and the operator's
	// bytes survive a rewrite, which a line scanner that strips it would not
	// allow. There is no maximum line length either: a stray multi-megabyte
	// line must not abort the parse and cost the operator their file.
	for len(data) > 0 {
		var raw string
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			raw, data = string(data[:i]), data[i+1:]
		} else {
			raw, data = string(data), nil
		}
		trimmed := strings.TrimSpace(raw)

		switch {
		case trimmed == BeginMarker:
			inBlock = true
			f.HadBlock = true
			continue
		case trimmed == EndMarker:
			if inBlock {
				inBlock = false
			} else {
				// An EndMarker with no opening one is user content that
				// happens to match; keep it.
				f.Lines = append(f.Lines, newLine(raw))
			}
			continue
		case inBlock:
			if e, ok := parseEntry(raw); ok {
				f.Managed = append(f.Managed, e)
			}
			// Remembered in case the block turns out to have no end.
			f.Orphans = append(f.Orphans, newLine(raw))
			continue
		}

		f.Lines = append(f.Lines, newLine(raw))
	}

	if inBlock {
		f.BlockTruncated = true
	} else {
		f.Orphans = nil
	}
	return f, nil
}

func newLine(raw string) Line {
	trimmed := strings.TrimSpace(raw)
	switch {
	case trimmed == "":
		return Line{Kind: LineBlank, Raw: raw}
	case strings.HasPrefix(trimmed, "#"):
		return Line{Kind: LineComment, Raw: raw}
	default:
		if e, ok := parseEntry(raw); ok {
			return Line{Kind: LineEntry, Raw: raw, IP: e.IP, Names: e.Names}
		}
		return Line{Kind: LineMalformed, Raw: raw}
	}
}

// parseEntry parses "<address> <name> [name...] [# comment]".
//
// ok is false when the line has no address or no name, which is what makes a
// line malformed.
func parseEntry(raw string) (Entry, bool) {
	body, comment := splitComment(raw)
	fields := strings.Fields(body)
	if len(fields) < 2 {
		return Entry{}, false
	}
	ip := net.ParseIP(fields[0])
	if ip == nil {
		return Entry{}, false
	}
	names := make([]string, 0, len(fields)-1)
	for _, n := range fields[1:] {
		names = append(names, strings.ToLower(n))
	}
	return Entry{IP: ip, Names: names, Comment: comment}, true
}

// splitComment separates the trailing comment from the data part, tolerating
// both "#" and " #" and preserving the original spacing of the comment.
func splitComment(raw string) (body, comment string) {
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		return strings.TrimRight(raw[:i], " \t"), strings.TrimSpace(raw[i:])
	}
	return raw, ""
}

// GroupByAddr merges entries that share an address and comment into one line.
//
// The reconciler produces one entry per published name; folding them keeps the
// hosts file small and, more importantly, keeps consecutive reconciles from
// producing different text for the same state.
//
// Order follows first appearance, so the result is deterministic.
func GroupByAddr(entries []Entry) []Entry {
	out := make([]Entry, 0, len(entries))

	for _, e := range entries {
		if e.IP == nil || len(e.Names) == 0 {
			continue
		}
		// Keep only the names that will actually survive rendering, so that
		// grouping agrees with String and a fully rejected entry disappears.
		usable := make([]string, 0, len(e.Names))
		for _, n := range e.Names {
			if s, ok := safeName(n); ok {
				usable = append(usable, s)
			}
		}
		if len(usable) == 0 {
			continue
		}

		merged := false
		for i := range out {
			if out[i].IP.Equal(e.IP) && out[i].Comment == e.Comment {
				out[i].Names = append(out[i].Names, usable...)
				merged = true
				break
			}
		}
		if !merged {
			out = append(out, Entry{
				IP:      e.IP,
				Comment: e.Comment,
				Names:   usable,
			})
		}
	}

	for i := range out {
		out[i].Names = dedupe(out[i].Names)
	}
	return out
}

func dedupe(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// String renders one entry as a hosts file line, or the empty string when
// nothing about it is safe to write.
//
// Names are validated on the way out. This writer's whole job is to modify a
// critical system file, so it must not trust its input: a name carrying a
// newline or a '#' would otherwise inject an arbitrary record into the host's
// name resolution. Today the names come from Docker and are already clean, but
// a guarantee that depends on an upstream invariant holding forever is not a
// guarantee.
//
// An unsafe name is dropped whole, never truncated to its safe prefix: a
// truncated name would silently resolve to something else, whereas a missing
// record is a visible mistake an operator can act on. An entry left with no
// names at all renders as the empty string so that no bare, malformed address
// line reaches the file.
func (e Entry) String() string {
	if e.IP == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(e.IP.String())
	wrote := 0
	for _, n := range e.Names {
		if safe, ok := safeName(n); ok {
			b.WriteByte(' ')
			b.WriteString(safe)
			wrote++
		}
	}
	if wrote == 0 {
		return ""
	}
	if e.Comment != "" {
		if c, ok := safeComment(e.Comment); ok {
			b.WriteByte('\t')
			b.WriteString(c)
		}
	}
	return b.String()
}

// safeName reports whether a name can be written verbatim, i.e. whether it is a
// single token free of whitespace, '#' and control characters.
func safeName(name string) (string, bool) {
	if name == "" || len(name) > maxNameLength {
		return "", false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c <= ' ' || c == 0x7f || c == '#' {
			return "", false
		}
	}
	return name, true
}

// safeComment keeps a comment on a single line.
//
// Only the line structure is enforced. Spaces are legitimate inside a comment
// and must survive, because comments carry the provenance shown in the web UI.
// A '#' after the first character is fine too: everything from the first '#'
// onwards is a comment anyway.
func safeComment(comment string) (string, bool) {
	if strings.ContainsAny(comment, "\n\r\v\f") || strings.ContainsRune(comment, 0) {
		return "", false
	}
	c := strings.Map(func(r rune) rune {
		if r <= ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, comment)
	c = strings.TrimSpace(c)
	if c == "" {
		return "", false
	}
	return c, true
}

// maxNameLength mirrors the DNS limit; a name longer than this cannot be
// resolved anyway, so writing it would only bloat the file.
const maxNameLength = 253
