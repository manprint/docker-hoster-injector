// Package naming turns Docker container identities into the host names that
// get published under the configured DNS suffix.
//
// Two problems are solved here:
//
//   - Docker allows characters that a DNS host name does not. Compose names
//     such as "proj_db_1" contain underscores. Because this agent writes
//     /etc/hosts rather than serving DNS, an underscore would in practice
//     still resolve, but it is not RFC 1123 conformant and some resolvers,
//     HTTP libraries and TLS stacks reject it. So both the raw name and a
//     sanitised variant are published.
//
//   - Two containers can compete for the same name. Assign makes that
//     deterministic instead of dependent on map iteration order.
package naming

import (
	"hash/fnv"
	"strconv"
	"strings"
)

// Limits from RFC 1035 and RFC 1123.
const (
	// MaxLabelLength is the maximum length of a single DNS label.
	MaxLabelLength = 63
	// MaxNameLength is the maximum length of a domain name in text form.
	MaxNameLength = 253
	// hashSuffixLength is how many hex chars are appended when a label has
	// to be truncated, so that "aaa...a" and "aaa...ab" stay distinct.
	hashSuffixLength = 8
)

// UnderscoreReplacement is the character sequence that stands in for an
// underscore in a sanitised name.
//
// Mapping "_" to "-" would be the obvious choice, but it is wrong: Compose
// names such as "web_1" and "web-1" both exist, so the substitution would make
// two genuinely different containers collide on one host name. Mapping to "-"
// therefore trades a resolvable name for an ambiguous one, which is the worse
// failure because the symptom is traffic going to the wrong container.
//
// A double underscore cannot occur in a sanitised name, since runs of
// separators are collapsed, so "web_1" becomes "web--1" and "web-1" stays
// "web-1": distinct before, distinct after.
const UnderscoreReplacement = "--"

// Sanitize converts an arbitrary Docker name into an RFC 1123 conformant
// name. Dots are treated as label separators and each label is sanitised
// independently, so "my.app" survives while "my_app" becomes "my--app".
//
// Empty labels are dropped: ".leading" still carries a usable name, and
// "../../etc/passwd" degrades to "etc-passwd" instead of losing the record.
//
// It returns false when nothing usable is left, which is the signal to drop
// the name entirely rather than publish a broken record.
func Sanitize(name string) (string, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return "", false
	}

	// Empty labels are dropped rather than treated as fatal: ".leading" and
	// "trailing." still carry a usable name, and "../../etc/passwd" must
	// degrade to "etc-passwd" instead of losing the record altogether.
	labels := strings.Split(n, ".")
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		if s, ok := sanitizeLabel(label); ok {
			out = append(out, s)
		}
	}

	res := strings.Join(out, ".")
	if res == "" || len(res) > MaxNameLength {
		return "", false
	}
	return res, true
}

// sanitizeLabel rewrites one label, or reports false if it is unusable.
func sanitizeLabel(label string) (string, bool) {
	if label == "" {
		return "", false
	}

	var b strings.Builder
	b.Grow(len(label))

	// pendingDash counts the hyphens owed for the run of separators seen so
	// far. An underscore owes two (UnderscoreReplacement), every other
	// non-alphanumeric character owes one, and the total is capped so a long
	// run cannot explode the label.
	pendingDash := 0
	const maxRun = 2

	for i := 0; i < len(label); i++ {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			if pendingDash > 0 && b.Len() > 0 {
				b.WriteString(strings.Repeat("-", pendingDash))
			}
			pendingDash = 0
			b.WriteByte(c)
		default:
			owed := 1
			if c == '_' {
				owed = len(UnderscoreReplacement)
			}
			if pendingDash < maxRun {
				pendingDash += owed
				if pendingDash > maxRun {
					pendingDash = maxRun
				}
			}
		}
	}

	// A trailing separator is dropped: it carries no information and would
	// make the label illegal.
	s := strings.TrimRight(b.String(), "-")
	if s == "" {
		return "", false
	}
	if len(s) > MaxLabelLength {
		s = truncate(s)
	}
	return s, true
}

// truncate shortens an over-long label to MaxLabelLength while keeping it
// unique, by replacing the tail with a hash of the full label.
func truncate(label string) string {
	keep := MaxLabelLength - hashSuffixLength - 1 // room for '-' plus hash
	if keep < 1 {
		keep = 1
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(label))
	suffix := strconv.FormatUint(uint64(h.Sum32()&0xffffffff), 16)
	// FormatUint on a 32 bit value can yield fewer than hashSuffixLength
	// chars; pad so the length stays predictable.
	for len(suffix) < hashSuffixLength {
		suffix = "0" + suffix
	}
	return label[:keep] + "-" + suffix
}

// TruncateLabel exposes truncate for callers that must shorten a label while
// preserving the uniqueness guarantee.
func TruncateLabel(label string) string { return truncate(label) }

// Variant is one publishable form of a container name.
type Variant struct {
	// Name is the host name to publish, without the suffix.
	Name string
	// Sanitized reports whether Name was rewritten to satisfy RFC 1123.
	Sanitized bool
}

// Variants returns every form of name worth publishing, in a deterministic
// order, with duplicates removed.
//
// The raw lowercase name comes first because it is what an operator expects to
// type; the sanitised variant follows only when it actually differs.
func Variants(names ...string) []Variant {
	seen := make(map[string]struct{}, len(names)*2)
	out := make([]Variant, 0, len(names)*2)

	for _, raw := range names {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		// The raw form is published whenever it is safe to write in a hosts
		// file. That deliberately includes names with underscores, which are
		// not RFC 1123 conformant but do resolve through glibc and musl,
		// and which operators expect to be able to type.
		if IsPublishable(name) {
			if _, dup := seen[name]; !dup {
				seen[name] = struct{}{}
				out = append(out, Variant{Name: name})
			}
		}
		if s, ok := Sanitize(name); ok && s != name {
			if _, dup := seen[s]; !dup {
				seen[s] = struct{}{}
				out = append(out, Variant{Name: s, Sanitized: true})
			}
		}
	}

	return out
}

// IsPublishable reports whether name can be written verbatim into a hosts
// file and still be found by an ordinary resolver.
//
// It is deliberately more permissive than IsValidName. Lookups in a hosts
// file are exact string matches, so nothing about RFC 1123 applies: glibc and
// musl resolve "proj_db_1.docker.local", "-x.docker.local" and "x_.docker.local"
// without complaint. The only real constraints are the file format itself, so
// this rejects whitespace, '#', empty labels and over-long names, and nothing
// more.
func IsPublishable(name string) bool {
	if name == "" || len(name) > MaxNameLength {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > MaxLabelLength {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			case c >= '0' && c <= '9':
			case c == '-', c == '_', c == '.':
			default:
				// Space, tab, '#' and anything else either breaks the
				// file format or cannot appear in a host name at all.
				return false
			}
		}
	}
	return true
}

// IsValidName reports whether name is already a legal multi-label host name:
// lowercase letters, digits, hyphens, dots, no empty label, no leading or
// trailing hyphen, within the length limits.
func IsValidName(name string) bool {
	if name == "" || len(name) > MaxNameLength || strings.HasPrefix(name, ".") ||
		strings.HasSuffix(name, ".") {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > MaxLabelLength {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
				continue
			}
			return false
		}
	}
	return true
}

// FQDN appends the suffix to a name, skipping the join when the result would
// exceed the maximum domain length. Callers treat false as "do not publish".
func FQDN(name, suffix string) (string, bool) {
	if name == "" || suffix == "" {
		return "", false
	}
	fqdn := name + "." + suffix
	if len(fqdn) > MaxNameLength {
		return "", false
	}
	return fqdn, true
}

// Records renders the final "name -> suffix" list for a container.
func Records(suffix string, names ...string) []string {
	fqdns := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, v := range Variants(names...) {
		fqdn, ok := FQDN(v.Name, suffix)
		if !ok {
			continue
		}
		if _, dup := seen[fqdn]; dup {
			continue
		}
		seen[fqdn] = struct{}{}
		fqdns = append(fqdns, fqdn)
	}
	return fqdns
}
