package webui

import (
	"sort"
	"time"

	"github.com/mint/docker-hoster-injector/internal/reconcile"
)

// buildSnapshot converts a reconcile result into the API read model.
//
// The ordering is total and deterministic so that two runs with the same state
// produce byte-identical JSON, which makes the output diffable and cacheable.
func (s *Server) buildSnapshot(res reconcile.Result) Snapshot {
	// The slices are pre-allocated to empty rather than left nil: a JSON
	// consumer must receive [] and not null, otherwise the first thing it
	// does on a fresh agent throws.
	snap := Snapshot{
		GeneratedAt: time.Now().UTC(),
		Records:     []Record{},
		Skipped:     []SkippedContainer{},
	}

	// Group the records by container: the page shows one row per container,
	// listing its addresses and names together. Doing it here keeps that
	// grouping out of the browser and out of the internal result type.
	type row struct {
		containerID string
		container   string
		state       string
		addresses   []string
		names       []string
	}

	byID := map[string]*row{}
	var order []string

	for _, e := range res.Entries {
		r, ok := byID[e.ContainerID]
		if !ok {
			r = &row{
				containerID: e.ContainerID,
				container:   e.ContainerName,
				state:       e.State,
			}
			byID[e.ContainerID] = r
			order = append(order, e.ContainerID)
		}
		r.addresses = append(r.addresses, e.IP.String())
		r.names = append(r.names, e.Names...)
	}

	sort.Strings(order)

	for _, id := range order {
		r := byID[id]
		names := dedupeSorted(r.names)
		sort.Strings(r.addresses)

		// One Record per address, each carrying the full name list, so the
		// file on disk and the page agree on what resolves where.
		for _, addr := range r.addresses {
			snap.Records = append(snap.Records, Record{
				Address:     addr,
				Names:       names,
				Container:   r.container,
				ContainerID: r.containerID,
				State:       r.state,
			})
			snap.Summary.Names += len(names)
		}
		snap.Summary.Containers++
	}

	for _, sk := range res.Skipped {
		snap.Skipped = append(snap.Skipped, SkippedContainer{
			Container:   sk.ContainerName,
			ContainerID: sk.ContainerID,
			State:       sk.State,
			Reason:      string(sk.Reason),
		})
		snap.Summary.Skipped++
	}

	sort.Slice(snap.Skipped, func(i, j int) bool {
		if snap.Skipped[i].Container != snap.Skipped[j].Container {
			return snap.Skipped[i].Container < snap.Skipped[j].Container
		}
		return snap.Skipped[i].ContainerID < snap.Skipped[j].ContainerID
	})

	snap.Summary.Records = len(snap.Records)

	for _, c := range res.Conflicts {
		snap.Skipped = append(snap.Skipped, SkippedContainer{
			Container:   c.Name,
			ContainerID: c.Winner.Owner,
			State:       "conflict",
			Reason:      "name claimed by more than one container",
		})
	}

	return snap
}

func dedupeSorted(in []string) []string {
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
