// Package reconcile turns the containers Docker reports into the records that
// belong in the hosts file.
//
// It is a pure function of its input: given the same container list it always
// produces the same entries, in the same order. That determinism is what lets
// the writer skip work when nothing has changed, and what makes the published
// file identical across agent restarts.
package reconcile

import (
	"net"
	"net/netip"
	"sort"

	"strings"

	"github.com/mint/docker-hoster-injector/internal/config"
	"github.com/mint/docker-hoster-injector/internal/dockerclient"
	"github.com/mint/docker-hoster-injector/internal/hostsfile"
	"github.com/mint/docker-hoster-injector/internal/naming"
)

// Entry is a record destined for the hosts file, plus the provenance the web
// UI needs to explain it.
type Entry struct {
	hostsfile.Entry
	// ContainerID and ContainerName tie the record back to its container.
	ContainerID   string
	ContainerName string
	// State is the container's Docker state at the time of the reconcile.
	State string
}

// SkipReason explains why a container produced no records.
type SkipReason string

// The reasons a container is not published. They are returned rather than
// merely logged so that the web UI can show them and so the behaviour is
// testable.
const (
	// SkipNotRunning covers every state in which the container has no
	// reachable address: exited, created, restarting, dead, removing.
	SkipNotRunning SkipReason = "not running"

	// SkipPaused is separate from SkipNotRunning because a paused container
	// does still have an address; it simply does not answer.
	SkipPaused SkipReason = "paused"

	// SkipNoNetwork means the container has no address at all.
	SkipNoNetwork SkipReason = "no network"

	// SkipNetworkHost means the container shares the host's network namespace.
	//
	// This is the interesting one. Such a container has no address of its own:
	// it uses the host's interfaces directly. Publishing its name against the
	// host's IP would make every host service reachable under the container's
	// name, and would collide with every other host-network container, all of
	// which resolve to the same address. Neither is a useful mapping, so these
	// containers are skipped.
	SkipNetworkHost SkipReason = "shares the host network"

	// SkipNoAddress means the container is on a network but has no usable
	// address yet, which happens briefly while it is starting.
	SkipNoAddress SkipReason = "no address assigned yet"

	// SkipNoNames means the container's name could not be turned into a host
	// name, for example because every variant was rejected.
	SkipNoNames SkipReason = "no usable name"

	// SkipReservedName means every name the container offers is reserved by the
	// operating system, such as "localhost".
	//
	// It is reported separately from SkipNoNames because the two need very
	// different responses: a reserved name means the container was misnamed,
	// which the operator must fix, whereas an unusable name usually means a
	// Compose project with an awkward name that the sanitiser coped with.
	SkipReservedName SkipReason = "name is reserved by the system"

	// SkipNoPublishedAddress means TARGET_MODE is "published" but the
	// container publishes no ports, so there is no host-side address to
	// publish.
	SkipNoPublishedAddress SkipReason = "no published ports"
)

// Skipped records a container that produced no entries.
type Skipped struct {
	ContainerID   string
	ContainerName string
	State         string
	Reason        SkipReason
}

// Result is the outcome of a reconcile.
type Result struct {
	// Entries are the records to publish, in a deterministic order.
	Entries []Entry
	// Skipped lists the containers that were not published, and why.
	Skipped []Skipped
	// Conflicts lists names claimed by more than one container.
	Conflicts []naming.Conflict
}

// Reconciler computes the desired records.
type Reconciler struct {
	cfg config.Config
}

// New returns a Reconciler for the given configuration.
func New(cfg config.Config) *Reconciler {
	return &Reconciler{cfg: cfg}
}

// Reconcile converts a container list into entries.
//
// Publishing follows two rules, in this order:
//
//   - a name is always offered in its raw form when it is safe to write in a
//     hosts file, because Docker guarantees raw names are unique and that is
//     what keeps every container reachable;
//   - a sanitised form is added when it differs, so names that are not RFC 1123
//     conformant still work for clients that care.
//
// Addresses depend on TARGET_MODE, which is what makes
// "nginx.docker.local:8080" behave like the published port.
func (r *Reconciler) Reconcile(containers []dockerclient.Container) Result {
	var res Result

	// Names are collected across all containers first, because two containers
	// can want the same host name and the winner has to be chosen
	// deterministically rather than by iteration order.
	var claims []naming.Claim
	byContainer := make(map[string][]string, len(containers))
	byAddress := make(map[string][]string, len(containers))

	for _, c := range containers {
		if reason, ok := skippable(c); !ok {
			res.Skipped = append(res.Skipped, Skipped{
				ContainerID:   c.ID,
				ContainerName: c.Name,
				State:         c.State,
				Reason:        reason,
			})
			continue
		}

		names := r.namesFor(c)
		if len(names) == 0 {
			res.Skipped = append(res.Skipped, Skipped{
				ContainerID:   c.ID,
				ContainerName: c.Name,
				State:         c.State,
				Reason:        SkipNoNames,
			})
			continue
		}

		// Reserved names would shadow the host's own resolution.
		names, rejected := naming.FilterReserved(names)
		if len(names) == 0 {
			reason := SkipNoNames
			if len(rejected) > 0 {
				// Every candidate was reserved, which is a naming mistake the
				// operator needs to see rather than a sanitiser failure.
				reason = SkipReservedName
			}
			res.Skipped = append(res.Skipped, Skipped{
				ContainerID:   c.ID,
				ContainerName: c.Name,
				State:         c.State,
				Reason:        reason,
			})
			continue
		}

		addresses, ok := r.addressesForContainer(c)
		if !ok {
			reason := SkipNoPublishedAddress
			if r.cfg.TargetMode != config.TargetModePublished {
				reason = SkipNoAddress
			}
			res.Skipped = append(res.Skipped, Skipped{
				ContainerID:   c.ID,
				ContainerName: c.Name,
				State:         c.State,
				Reason:        reason,
			})
			continue
		}

		byContainer[c.ID] = names
		byAddress[c.ID] = addresses
		for _, n := range names {
			// Created is the collision priority, so the container that
			// existed first keeps a contested name.
			claims = append(claims, naming.Claim{
				Name:     cfgFQDN(r.cfg.DNSSuffix, n),
				Owner:    c.ID,
				Priority: c.Created.Unix(),
			})
		}
	}

	assignment := naming.Assign(claims)
	res.Conflicts = assignment.Conflicts

	// Keep only the names each container actually won.
	won := make(map[string][]string, len(byContainer))
	for _, c := range containers {
		names := byContainer[c.ID]
		if names == nil {
			continue
		}
		for _, n := range names {
			fqdn := cfgFQDN(r.cfg.DNSSuffix, n)
			if owner, ok := assignment.Owners[fqdn]; ok && owner.Owner == c.ID {
				won[c.ID] = append(won[c.ID], fqdn)
			}
		}
	}

	for _, c := range containers {
		names := won[c.ID]
		if len(names) == 0 {
			continue
		}
		for _, addr := range byAddress[c.ID] {
			ip := net.ParseIP(addr)
			if ip == nil {
				continue
			}
			res.Entries = append(res.Entries, Entry{
				Entry: hostsfile.Entry{
					IP:    ip,
					Names: names,
				},
				ContainerID:   c.ID,
				ContainerName: c.Name,
				State:         c.State,
			})
		}
	}

	r.sort(&res)
	return res
}

// skippable reports whether a container can be published at all.
func skippable(c dockerclient.Container) (SkipReason, bool) {
	switch c.State {
	case "running":
		// The only publishable state.
	case "paused":
		// A paused container has an address but does not answer. Publishing it
		// would produce a name that resolves and then hangs.
		return SkipPaused, false
	default:
		// exited, created, restarting, dead, removing, and anything a future
		// Docker version might add.
		return SkipNotRunning, false
	}

	// A container sharing the host's network namespace has no address of its
	// own. See SkipNetworkHost for why publishing it would be harmful.
	switch strings.ToLower(c.NetworkMode) {
	case "host":
		return SkipNetworkHost, false
	case "none", "":
		return SkipNoNetwork, false
	}

	if len(c.Networks) == 0 {
		return SkipNoNetwork, false
	}
	return "", true
}

// namesFor returns every host name variant for a container.
func (r *Reconciler) namesFor(c dockerclient.Container) []string {
	candidates := []string{c.Name}
	// Network aliases and DNS names are how Compose service names reach a
	// container, so they are published too.
	for _, n := range c.Networks {
		candidates = append(candidates, n.Aliases...)
		candidates = append(candidates, n.DNSNames...)
	}

	var out []string
	for _, v := range naming.Variants(candidates...) {
		out = append(out, v.Name)
	}
	return naming.Dedupe(out)
}

// addressesForContainer returns the published addresses.
//
// In "both" mode the host-reachable address comes first, because that is what
// makes "nginx.docker.local:8080" work for "-p 8080:80": the name has to
// resolve on the host for the port mapping to apply. The container address
// follows so that direct access to the container's own ports keeps working.
func (r *Reconciler) addressesForContainer(c dockerclient.Container) ([]string, bool) {
	hostSide := hostAddresses(c)
	containerSide := containerAddresses(c)

	var out []string
	switch r.cfg.TargetMode {
	case config.TargetModePublished:
		out = hostSide
	case config.TargetModeContainerIP:
		out = containerSide
	default: // both
		out = append(out, hostSide...)
		out = append(out, containerSide...)
	}

	out = dedupeStrings(out)
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// hostAddresses returns the addresses on which the published ports are
// reachable.
//
// A port published without an explicit bind address listens on every
// interface, which includes loopback, so 127.0.0.1 is correct and does not
// require guessing the host's outward facing address. An explicitly bound
// address is used as given, because that is the only interface where the port
// is actually listening.
func hostAddresses(c dockerclient.Container) []string {
	var out []string
	for _, p := range c.PublishedPorts {
		if !p.Published() {
			continue
		}
		switch p.HostIP {
		case "", "0.0.0.0":
			out = append(out, "127.0.0.1")
		case "::", "[::]":
			out = append(out, "::1")
		default:
			if isUsableAddr(p.HostIP) {
				out = append(out, p.HostIP)
			}
		}
	}
	return out
}

// containerAddresses returns the container's own addresses, one per network,
// IPv4 before IPv6 within each network.
func containerAddresses(c dockerclient.Container) []string {
	var out []string
	for _, n := range c.Networks {
		if isUsableAddr(n.IPv4) {
			out = append(out, n.IPv4)
		}
		if isUsableAddr(n.IPv6) {
			out = append(out, n.IPv6)
		}
	}
	return out
}

// sort puts the result in a deterministic order so that identical state always
// renders to identical bytes.
func (r *Reconciler) sort(res *Result) {
	sort.SliceStable(res.Entries, func(i, j int) bool {
		a, b := res.Entries[i], res.Entries[j]
		if a.IP.String() != b.IP.String() {
			return a.IP.String() < b.IP.String()
		}
		if a.ContainerName != b.ContainerName {
			return a.ContainerName < b.ContainerName
		}
		return strings.Join(a.Names, ",") < strings.Join(b.Names, ",")
	})

	sort.SliceStable(res.Skipped, func(i, j int) bool {
		a, b := res.Skipped[i], res.Skipped[j]
		if a.ContainerName != b.ContainerName {
			return a.ContainerName < b.ContainerName
		}
		return a.ContainerID < b.ContainerID
	})
}

func dedupeStrings(in []string) []string {
	if len(in) < 2 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
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

// cfgFQDN joins a name and the suffix, dropping anything that does not fit.
func cfgFQDN(suffix, name string) string {
	if fqdn, ok := naming.FQDN(name, suffix); ok {
		return fqdn
	}
	return ""
}

// HostsEntries flattens the result into what the writer consumes.
func (r Result) HostsEntries() []hostsfile.Entry {
	out := make([]hostsfile.Entry, 0, len(r.Entries))
	for _, e := range r.Entries {
		out = append(out, e.Entry)
	}
	return out
}

// Summary returns the names published for a container, for the web UI.
func (r Result) Summary(containerID string) []string {
	var out []string
	for _, e := range r.Entries {
		if e.ContainerID == containerID {
			out = append(out, e.Names...)
		}
	}
	return out
}

// isUsableAddr filters addresses that cannot appear in a hosts file.
//
// An unspecified address such as "0.0.0.0" or "::" is rejected: publishing it
// would point a container name at every local interface, which is exactly the
// outcome this project exists to avoid.
func isUsableAddr(s string) bool {
	if s == "" {
		return false
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return false
	}
	return !addr.IsUnspecified()
}
