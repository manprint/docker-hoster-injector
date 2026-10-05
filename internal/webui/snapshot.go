package webui

import (
	"fmt"
	"slices"
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

	// By name, then ID: a row order a person can scan, and still total.
	sort.Slice(order, func(i, j int) bool {
		a, b := byID[order[i]], byID[order[j]]
		if a.container != b.container {
			return a.container < b.container
		}
		return a.containerID < b.containerID
	})

	for _, id := range order {
		r := byID[id]
		names := dedupeSorted(r.names)

		// Counted once per container: the same names are listed against each
		// of its addresses, and counting them per address would inflate the
		// figure whenever a container has more than one.
		snap.Summary.Names += len(names)

		// One Record per address, each carrying the full name list, so the
		// file on disk and the page agree on what resolves where. The
		// addresses keep the order of the file, which is the order a resolver
		// tries them in.
		for _, addr := range r.addresses {
			snap.Records = append(snap.Records, Record{
				Address:     addr,
				Names:       names,
				Container:   r.container,
				ContainerID: r.containerID,
				State:       r.state,
			})
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
	}

	snap.Summary.Records = len(snap.Records)

	// A contested name is shown next to the containers that are not published:
	// a container that lost every one of its names appears nowhere else.
	owners := make(map[string]string, len(byID))
	for _, r := range byID {
		owners[r.containerID] = r.container
	}
	for _, c := range res.Conflicts {
		winner := owners[c.Winner.Owner]
		if winner == "" {
			winner = shortID(c.Winner.Owner)
		}
		snap.Skipped = append(snap.Skipped, SkippedContainer{
			Container:   c.Name,
			ContainerID: c.Winner.Owner,
			State:       "conflict",
			Reason: fmt.Sprintf("name wanted by %d containers, kept by %s",
				len(c.Losers)+1, winner),
		})
	}
	sort.Slice(snap.Skipped, func(i, j int) bool {
		if snap.Skipped[i].Container != snap.Skipped[j].Container {
			return snap.Skipped[i].Container < snap.Skipped[j].Container
		}
		return snap.Skipped[i].ContainerID < snap.Skipped[j].ContainerID
	})

	// The counter always matches the rows the page shows.
	snap.Summary.Skipped = len(snap.Skipped)

	return snap
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// dedupeSorted returns the distinct values in ascending order.
func dedupeSorted(in []string) []string {
	if len(in) < 2 {
		return in
	}
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}
