package naming

import "strings"

// reservedNames are host names that Linux distributions place in /etc/hosts
// with a meaning of their own. Publishing a container under one of these would
// either shadow that meaning or, worse, break the host's own resolution, so
// the reconciler refuses them.
var reservedNames = map[string]struct{}{
	"localhost":             {},
	"localhost4":            {},
	"localhost6":            {},
	"localhost.localdomain": {},
	"localdomain":           {},
	"ip6-localhost":         {},
	"ip6-localhost4":        {},
	"ip6-localhost6":        {},
	"ip6-loopback":          {},
	"ip6-allnodes":          {},
	"ip6-allrouters":        {},
	"broadcasthost":         {},
	"allhosts":              {},
	"allnodes":              {},
	"local":                 {},
	"none":                  {},
	"unspecified":           {},
}

// IsReserved reports whether a name collides with a well known hosts entry.
//
// The comparison is case insensitive, since hosts file lookups are, and it
// considers only the first label: "localhost.localdomain" and a container
// called "ip6-localhost" must both be caught, while "localhost-proxy" must not.
func IsReserved(name string) bool {
	base, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(name)), ".")
	_, ok := reservedNames[base]
	return ok
}

// FilterReserved removes the names that must not be published and returns the
// rest in their original order, together with the rejected ones so the caller
// can log exactly what was skipped and why.
func FilterReserved(names []string) (allowed, rejected []string) {
	for _, n := range names {
		if IsReserved(n) {
			rejected = append(rejected, n)
			continue
		}
		allowed = append(allowed, n)
	}
	return allowed, rejected
}
