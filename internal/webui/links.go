package webui

import (
	"net"
	"net/netip"
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

// linksFor lists the ports reachable through one record.
//
// Which ones depends on where the address points:
//
//   - A host address reaches the published ports. The container's name resolves
//     to it, so the link uses the name and the HOST port: that is the whole
//     purpose of publishing the name.
//   - A container address reaches the ports the container listens on, published
//     or not, on the container port. The link uses the address itself, because
//     the name may resolve to the host first and a service of the host that
//     happens to listen on the same port would answer in the container's place.
//     In container-ip mode the name only resolves to container addresses, so
//     there it is used.
func (s *Server) linksFor(e reconcile.Entry, names []string) []Link {
	addr := e.IP.String()
	name := primaryName(e.ContainerName, names, s.cfg.DNSSuffix)

	type key struct {
		port  int
		proto string
	}
	seen := map[key]bool{}
	// Empty, not nil: the JSON must say [] and never null.
	links := []Link{}

	// port is the one in the URL; service is the port inside the container,
	// which is what says what protocol is spoken. For a published port the two
	// differ: 5432 published as 49153 is still a database.
	add := func(host string, port, service int, proto string) {
		k := key{port, proto}
		if seen[k] || port <= 0 || port > 65535 {
			return
		}
		seen[k] = true
		l := Link{Label: hostPort(host, port), Port: port, Protocol: proto}
		if proto == "tcp" && !notHTTP[service] {
			scheme := "http"
			if tlsPorts[service] {
				scheme = "https"
			}
			l.URL = scheme + "://" + hostPort(host, port) + "/"
		}
		links = append(links, l)
	}

	switch e.Side {
	case reconcile.SideHost:
		if name == "" {
			break
		}
		for _, p := range e.Ports {
			if p.Published() && listensOn(p.HostIP, addr) {
				add(name, p.HostPort, p.ContainerPort, protocolOf(p))
			}
		}
	default:
		host := addr
		if s.cfg.TargetMode == string(config.TargetModeContainerIP) && name != "" {
			host = name
		}
		for _, p := range e.Ports {
			add(host, p.ContainerPort, p.ContainerPort, protocolOf(p))
		}
	}

	sort.SliceStable(links, func(i, j int) bool {
		if links[i].Port != links[j].Port {
			return links[i].Port < links[j].Port
		}
		return links[i].Protocol < links[j].Protocol
	})
	return links
}

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
