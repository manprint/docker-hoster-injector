// Package version compares Docker API versions.
//
// The comparison is numeric rather than lexicographic because these strings
// look sortable but are not: "1.9" is older than "1.44", yet it sorts after it
// as text. Getting this wrong would let an ancient engine pass the version
// check and then fail in a much more confusing way later.
package version

import (
	"fmt"
	"strconv"
	"strings"
)

// Compare returns a negative number if a is older than b, zero if they are
// equal, and a positive number if a is newer than b.
//
// A trailing suffix such as "1.44.0-rc.1" is handled: only the numeric parts
// are compared, and a version with fewer parts than the other is padded with
// zeroes, so "1.44" and "1.44.0" are equal.
func Compare(a, b string) int {
	as := parse(a)
	bs := parse(b)
	for i := 0; i < 3; i++ {
		if as[i] != bs[i] {
			return as[i] - bs[i]
		}
	}
	return 0
}

// AtLeast reports whether version is at least min.
func AtLeast(version, min string) bool {
	return Compare(version, min) >= 0
}

// String renders a parsed version for messages.
func String(v string) string {
	return fmt.Sprintf("%d.%d.%d", parse(v)[0], parse(v)[1], parse(v)[2])
}

// parse extracts the three numeric components, ignoring anything that is not a
// number. An unparsable component becomes 0 rather than an error, because this
// runs on a string from an external system where refusing to compare would be
// worse than a slightly wrong ordering; the caller's own floor check still
// protects the real behaviour.
func parse(v string) [3]int {
	var out [3]int

	v = strings.TrimSpace(v)
	// Docker reports versions as "v29.8.2" in some places and "1.51" in
	// others, so a leading "v" is stripped rather than treated as junk.
	v = strings.TrimPrefix(v, "v")

	// Cut any pre-release or build suffix: "1.44.0-rc.1" -> "1.44.0".
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}

	parts := strings.Split(v, ".")
	for i := 0; i < 3 && i < len(parts); i++ {
		n, err := strconv.Atoi(strings.TrimSpace(parts[i]))
		if err != nil {
			break
		}
		out[i] = n
	}
	return out
}
