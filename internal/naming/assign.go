package naming

import (
	"sort"
	"strings"
)

// Claim is one container's bid for a host name.
type Claim struct {
	// Name is the FQDN, suffix included.
	Name string
	// Owner identifies the container, normally its ID. It is used to make
	// collision handling deterministic.
	Owner string
	// Priority orders competing claims. Lower wins. A container that was
	// created earlier should keep a name it already had, so the reconciler
	// sets Priority from the creation timestamp.
	Priority int64
}

// Conflict records a name that several containers wanted.
type Conflict struct {
	Name    string
	Winner  Claim
	Losers  []Claim
	Dropped int
}

// Assignment is the outcome of resolving competing claims.
type Assignment struct {
	// Owners maps a host name to the container that won it.
	Owners map[string]Claim
	// Conflicts lists every contested name, sorted by name.
	Conflicts []Conflict
}

// Assign resolves competing claims deterministically.
//
// Ordering rules, in order of importance:
//
//  1. lowest Priority wins, so the longest-lived container keeps the name;
//  2. then the lexicographically smallest Owner ID, so two containers created
//     in the same second still resolve the same way on every host and every
//     restart;
//  3. names are processed in sorted order so the log is reproducible.
//
// Empty names and claims without an owner are ignored.
func Assign(claims []Claim) Assignment {
	byName := make(map[string][]Claim, len(claims))
	for _, c := range claims {
		if c.Name == "" || c.Owner == "" {
			continue
		}
		byName[c.Name] = append(byName[c.Name], c)
	}

	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)

	out := Assignment{Owners: make(map[string]Claim, len(byName))}
	for _, name := range names {
		group := byName[name]
		sort.SliceStable(group, func(i, j int) bool {
			if group[i].Priority != group[j].Priority {
				return group[i].Priority < group[j].Priority
			}
			return group[i].Owner < group[j].Owner
		})
		out.Owners[name] = group[0]
		if len(group) > 1 {
			out.Conflicts = append(out.Conflicts, Conflict{
				Name:   name,
				Winner: group[0],
				Losers: group[1:],
			})
		}
	}
	return out
}

// Dedupe removes repeated entries while keeping the first occurrence, which
// is how aliases gathered from several sources are merged before assignment.
func Dedupe(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
