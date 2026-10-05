package webui

import (
	"net"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/mint/docker-hoster-injector/internal/config"
	"github.com/mint/docker-hoster-injector/internal/dockerclient"
	"github.com/mint/docker-hoster-injector/internal/reconcile"
)

// notHTTP lists well-known TCP ports whose service does not speak HTTP. Opening
// one in a browser shows an error at best, so they are listed but not linked.
// The list is deliberately short and about protocols, not about guesses: any
// other port is offered as a link, because there is no way to tell from outside
// what a container serves, and a link that fails is cheaper than a missing one.
var notHTTP = map[int]bool{
	21: true, 22: true, 23: true, 25: true, 53: true, 110: true, 143: true,
	389: true, 465: true, 587: true, 636: true, 993: true, 995: true,
	1433: true, 1521: true, 1883: true, 2181: true, 3306: true, 4222: true,
	5432: true, 5672: true, 6379: true, 8883: true, 9092: true, 11211: true,
	27017: true,
}

// tlsPorts are the ports conventionally served over TLS.
var tlsPorts = map[int]bool{443: true, 4443: true, 8443: true, 9443: true}

// linksByEntry decides, for one container, which link goes on which of its
// records. Every port of the container gets ONE link, on the record whose
// address it is reached through:
//
//   - A published port is reached through the host: the link uses the container's
//     name and the HOST port, and sits on the host-side record that listens for
//     it. That is the whole purpose of publishing the name.
//   - A port that is only exposed is reached on the container's own address, so
//     the link uses that address and the container port, and sits on every
//     container-side record (one per network: each address is a different way
//     in). The address is used and not the name because the name may resolve to
//     the host first, and a service of the host listening on the same port would
//     answer in the container's place. With TARGET_MODE=container-ip the name
//     only resolves to container addresses, so there it is used.
//
// A port that is both published and listening in the container is therefore not
// listed twice: the published link is the one to use.
func (s *Server) linksByEntry(entries []reconcile.Entry, names []string) [][]Link {
	out := make([][]Link, len(entries))
	for i := range out {
		// Empty, not nil: the JSON must say [] and never null.
		out[i] = []Link{}
	}
	if len(entries) == 0 {
		return out
	}

	name := primaryName(entries[0].ContainerName, names, s.cfg.DNSSuffix)
	ports := entries[0].Ports

	type key struct {
		port  int
		proto string
	}
	// reached are the container ports that already have a published link.
	reached := map[key]bool{}
	// published are the host ports that already have a link.
	published := map[key]bool{}
	// onEntry avoids the same port twice on one record: Docker lists a port
	// once per bind address.
	onEntry := make([]map[key]bool, len(entries))
	for i := range onEntry {
		onEntry[i] = map[key]bool{}
	}
	add := func(i int, host string, port, service int, proto string) {
		k := key{port, proto}
		if onEntry[i][k] {
			return
		}
		onEntry[i][k] = true
		out[i] = append(out[i], makeLink(host, port, service, proto))
	}

	if name != "" {
		// Docker does not promise an order for the port list. IPv4 bindings go
		// first so that a port bound both ways is linked on the IPv4 record
		// whichever way the daemon listed it.
		ordered := slices.Clone(ports)
		sort.SliceStable(ordered, func(i, j int) bool {
			return !strings.Contains(ordered[i].HostIP, ":") && strings.Contains(ordered[j].HostIP, ":")
		})
		for _, p := range ordered {
			if !p.Published() {
				continue
			}
			if i := hostRecordFor(entries, p); i >= 0 {
				// Docker lists a port bound to every interface twice, for
				// 0.0.0.0 and for ::, with the same host port. One link is
				// enough, and it goes on the record found first (IPv4).
				if !published[key{p.HostPort, protocolOf(p)}] {
					add(i, name, p.HostPort, p.ContainerPort, protocolOf(p))
					published[key{p.HostPort, protocolOf(p)}] = true
				}
				reached[key{p.ContainerPort, protocolOf(p)}] = true
			}
		}
	}

	for i, e := range entries {
		if e.Side != reconcile.SideContainer {
			continue
		}
		host := e.IP.String()
		if s.cfg.TargetMode == string(config.TargetModeContainerIP) && name != "" {
			host = name
		}
		for _, p := range ports {
			if reached[key{p.ContainerPort, protocolOf(p)}] {
				continue
			}
			add(i, host, p.ContainerPort, p.ContainerPort, protocolOf(p))
		}
	}

	for i := range out {
		sort.SliceStable(out[i], func(a, b int) bool {
			if out[i][a].Port != out[i][b].Port {
				return out[i][a].Port < out[i][b].Port
			}
			return out[i][a].Protocol < out[i][b].Protocol
		})
	}
	return out
}

// makeLink builds one link. port is the one in the URL; service is the port
// inside the container, which is what says what protocol is spoken: 5432
// published as 49153 is still a database.
func makeLink(host string, port, service int, proto string) Link {
	l := Link{Label: hostPort(host, port), Port: port, Protocol: proto}
	if proto == "tcp" && port > 0 && port <= 65535 && !notHTTP[service] {
		scheme := "http"
		if tlsPorts[service] {
			scheme = "https"
		}
		l.URL = scheme + "://" + hostPort(host, port) + "/"
	}
	return l
}

// hostRecordFor returns the host-side record a published port listens on, or
// -1. IPv4 is preferred: a port bound to every interface is listed by Docker
// for 0.0.0.0 and for ::, and the link belongs with the IPv4 address.
func hostRecordFor(entries []reconcile.Entry, p dockerclient.Port) int {
	for _, wantV4 := range []bool{true, false} {
		for i, e := range entries {
			if e.Side == reconcile.SideHost && isIPv4(e.IP) == wantV4 && listensOn(p.HostIP, e.IP.String()) {
				return i
			}
		}
	}
	return -1
}

// isIPv4 reports whether ip is an IPv4 address, including the IPv4-mapped form.
func isIPv4(ip net.IP) bool { return ip.To4() != nil }

// primaryName picks the name a link is built on: the container's own name when
// it was published, else the first name that is safe in a URL. Names with an
// underscore are passed over when another exists, because not every client
// accepts them in a host name.
func primaryName(container string, names []string, suffix string) string {
	own := container + "." + suffix
	var fallback string
	for _, n := range names {
		if n == own && !strings.Contains(n, "_") {
			return n
		}
		if fallback == "" && !strings.Contains(n, "_") {
			fallback = n
		}
	}
	if fallback != "" {
		return fallback
	}
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// listensOn reports whether a port bound to hostIP is reachable on address.
func listensOn(hostIP, addr string) bool {
	switch hostIP {
	case "", "0.0.0.0":
		return addr == "127.0.0.1"
	case "::", "[::]":
		return addr == "::1"
	}
	a, err1 := netip.ParseAddr(hostIP)
	b, err2 := netip.ParseAddr(addr)
	return err1 == nil && err2 == nil && a == b
}

func protocolOf(p dockerclient.Port) string {
	if p.Protocol == "" {
		return "tcp"
	}
	return p.Protocol
}

// hostPort joins a host and a port, bracketing an IPv6 literal.
func hostPort(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}
