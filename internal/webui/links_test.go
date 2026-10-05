package webui

import (
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/manprint/docker-hoster-injector/internal/dockerclient"
	"github.com/manprint/docker-hoster-injector/internal/hostsfile"
	"github.com/manprint/docker-hoster-injector/internal/reconcile"
)

func entry(addr string, side reconcile.AddressSide, name string, ports ...dockerclient.Port) reconcile.Entry {
	return reconcile.Entry{
		Entry:         hostsfile.Entry{IP: net.ParseIP(addr)},
		ContainerName: name,
		Side:          side,
		Ports:         ports,
	}
}

func urls(links []Link) []string {
	out := []string{}
	for _, l := range links {
		out = append(out, l.URL)
	}
	return out
}

// container builds the records of one container: host-side addresses first, as
// the reconciler orders them in "both" mode, then its own addresses.
func container(name string, ports []dockerclient.Port, host []string, own []string) []reconcile.Entry {
	var out []reconcile.Entry
	for _, a := range host {
		out = append(out, entry(a, reconcile.SideHost, name, ports...))
	}
	for _, a := range own {
		out = append(out, entry(a, reconcile.SideContainer, name, ports...))
	}
	return out
}

func allURLs(links [][]Link) [][]string {
	out := make([][]string, len(links))
	for i, l := range links {
		out[i] = urls(l)
	}
	return out
}

func TestEachPortGetsOneLinkOnTheRecordItIsReachedThrough(t *testing.T) {
	t.Parallel()
	s := New(Config{DNSSuffix: "docker.local", TargetMode: "both"}, nil)

	ports := []dockerclient.Port{
		// Published on every interface: Docker lists it for IPv4 and IPv6.
		{HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
		{HostIP: "::", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
		// Published on loopback only.
		{HostIP: "127.0.0.1", HostPort: 9090, ContainerPort: 81, Protocol: "tcp"},
		// Published on another interface: no record for it, so it falls back
		// to the container's own address.
		{HostIP: "192.168.1.5", HostPort: 7070, ContainerPort: 82, Protocol: "tcp"},
		// Exposed only.
		{ContainerPort: 83, Protocol: "tcp"},
	}
	entries := container("web", ports, []string{"127.0.0.1", "::1"}, []string{"172.17.0.2"})

	got := allURLs(s.linksByEntry(entries, []string{"web.docker.local"}))
	want := [][]string{
		// 127.0.0.1: both published ports that listen here.
		{"http://web.docker.local:8080/", "http://web.docker.local:9090/"},
		// ::1: nothing, the IPv4 record already carries 8080.
		{},
		// The container's own address: the ports that are not published.
		{"http://172.17.0.2:82/", "http://172.17.0.2:83/"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("links = %v\nwant    %v", got, want)
	}
}

func TestAPublishedPortIsNotListedAgainOnTheContainerAddress(t *testing.T) {
	t.Parallel()
	s := New(Config{DNSSuffix: "docker.local", TargetMode: "both"}, nil)
	ports := []dockerclient.Port{
		{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
		{ContainerPort: 80, Protocol: "tcp"},
	}
	entries := container("web", ports, []string{"127.0.0.1"}, []string{"172.17.0.2"})
	got := allURLs(s.linksByEntry(entries, []string{"web.docker.local"}))
	want := [][]string{{"http://web.docker.local:8080/"}, {}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("links = %v, want %v", got, want)
	}
}

// Each network is a different way in, so each container address carries the
// exposed ports.
func TestEveryContainerAddressCarriesTheExposedPorts(t *testing.T) {
	t.Parallel()
	s := New(Config{DNSSuffix: "docker.local", TargetMode: "both"}, nil)
	ports := []dockerclient.Port{{ContainerPort: 8082, Protocol: "tcp"}}
	entries := container("twonets", ports, nil, []string{"172.17.0.6", "172.18.0.3"})
	got := allURLs(s.linksByEntry(entries, []string{"twonets.docker.local"}))
	want := [][]string{{"http://172.17.0.6:8082/"}, {"http://172.18.0.3:8082/"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("links = %v, want %v", got, want)
	}
}

func TestLinksInContainerIPModeUseTheName(t *testing.T) {
	t.Parallel()
	s := New(Config{DNSSuffix: "docker.local", TargetMode: "container-ip"}, nil)
	entries := container("web", []dockerclient.Port{{ContainerPort: 80, Protocol: "tcp"}}, nil, []string{"172.17.0.2"})
	got := allURLs(s.linksByEntry(entries, []string{"web.docker.local"}))
	if want := [][]string{{"http://web.docker.local:80/"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("links = %v, want %v", got, want)
	}
}

func TestLinksBracketIPv6(t *testing.T) {
	t.Parallel()
	s := New(Config{DNSSuffix: "docker.local", TargetMode: "both"}, nil)
	entries := container("web", []dockerclient.Port{{ContainerPort: 8000, Protocol: "tcp"}}, nil, []string{"fd00::2"})
	got := allURLs(s.linksByEntry(entries, []string{"web.docker.local"}))
	if want := [][]string{{"http://[fd00::2]:8000/"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("links = %v, want %v", got, want)
	}
}

// Ports that do not speak HTTP, and UDP, are listed but offered no URL. The
// protocol is judged by the port inside the container: a database published on
// a high port is still not a web page.
func TestLinksSkipNonWebPorts(t *testing.T) {
	t.Parallel()
	s := New(Config{DNSSuffix: "docker.local", TargetMode: "both"}, nil)
	ports := []dockerclient.Port{
		{HostPort: 49153, ContainerPort: 5432, Protocol: "tcp"},
		{HostPort: 49154, ContainerPort: 443, Protocol: "tcp"},
		{ContainerPort: 53, Protocol: "udp"},
		{ContainerPort: 8080, Protocol: "tcp"},
	}
	entries := container("db", ports, []string{"127.0.0.1"}, []string{"172.17.0.2"})
	links := s.linksByEntry(entries, []string{"db.docker.local"})

	byPort := map[int]Link{}
	for _, l := range append(append([]Link{}, links[0]...), links[1]...) {
		byPort[l.Port] = l
	}
	if len(byPort) != 4 {
		t.Fatalf("links = %+v, want 4 distinct ports", links)
	}
	if byPort[49153].URL != "" {
		t.Errorf("a database port got a URL: %q", byPort[49153].URL)
	}
	if byPort[49154].URL != "https://db.docker.local:49154/" {
		t.Errorf("a TLS service got %q", byPort[49154].URL)
	}
	if byPort[53].URL != "" || byPort[53].Protocol != "udp" {
		t.Errorf("the udp port = %+v", byPort[53])
	}
	if byPort[8080].URL != "http://172.17.0.2:8080/" {
		t.Errorf("the exposed port got %q", byPort[8080].URL)
	}
}

func TestLinksAreNeverNull(t *testing.T) {
	t.Parallel()
	s := New(Config{DNSSuffix: "docker.local"}, nil)
	for _, l := range s.linksByEntry(container("web", nil, nil, []string{"172.17.0.2"}), nil) {
		if l == nil {
			t.Fatal("a record has nil links, which marshals to null")
		}
	}
}

func TestPrimaryNamePrefersTheContainersOwnNameWithoutUnderscore(t *testing.T) {
	t.Parallel()
	cases := []struct {
		container string
		names     []string
		want      string
	}{
		{"web", []string{"alias.docker.local", "web.docker.local"}, "web.docker.local"},
		{"my_app", []string{"my--app.docker.local", "my_app.docker.local"}, "my--app.docker.local"},
		{"x_y", []string{"x_y.docker.local"}, "x_y.docker.local"},
		{"gone", nil, ""},
	}
	for _, c := range cases {
		if got := primaryName(c.container, c.names, "docker.local"); got != c.want {
			t.Errorf("primaryName(%q, %v) = %q, want %q", c.container, c.names, got, c.want)
		}
	}
}

// The snapshot is placed inside a <script> element, so a value that contains
// the closing tag must not be able to end it. encoding/json escapes the angle
// brackets; this keeps it that way.
func TestSnapshotCannotBreakOutOfTheScriptElement(t *testing.T) {
	t.Parallel()
	s := New(Config{DNSSuffix: "docker.local", TargetMode: "both"}, nil)

	hostile := `</script><script>alert(1)</script><!--`
	res := reconcile.Result{
		Entries: []reconcile.Entry{entry("127.0.0.1", reconcile.SideHost, hostile,
			dockerclient.Port{HostPort: 80, ContainerPort: 80, Protocol: "tcp"})},
		Skipped: []reconcile.Skipped{{ContainerName: hostile, State: hostile, Reason: reconcile.SkipReason(hostile)}},
	}
	res.Entries[0].Names = []string{hostile}

	page, err := renderIndex(s.cfg, s.buildSnapshot(res))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// The only closing script tag is the page's own, and the hostile text
	// appears nowhere unescaped.
	if n := strings.Count(page, "</script>"); n != 1 {
		t.Errorf("%d closing script tags in the page, want 1", n)
	}
	if strings.Contains(page, "<script>alert(1)") {
		t.Error("the hostile script is in the page unescaped")
	}
}

func resultOf(entries ...reconcile.Entry) reconcile.Result {
	for i := range entries {
		entries[i].ContainerID = entries[i].ContainerName + "-id"
		entries[i].State = "running"
		entries[i].Names = []string{entries[i].ContainerName + ".docker.local"}
	}
	return reconcile.Result{Entries: entries}
}

func addresses(snap Snapshot) []string {
	var out []string
	for _, r := range snap.Records {
		out = append(out, r.Address)
	}
	return out
}

// The page lists IPv4 only; the hosts file keeps both.
func TestTheSnapshotHidesTheIPv6TwinOfARecord(t *testing.T) {
	t.Parallel()
	s := New(Config{DNSSuffix: "docker.local", TargetMode: "both"}, nil)
	web := []dockerclient.Port{{HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}, {HostIP: "::", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}}

	snap := s.buildSnapshot(resultOf(container("web", web, []string{"127.0.0.1", "::1"}, []string{"172.17.0.2", "fd00::2"})...))
	if got, want := addresses(snap), []string{"127.0.0.1", "172.17.0.2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("addresses = %v, want %v", got, want)
	}
	if snap.Summary.Records != 2 || snap.Summary.Containers != 1 {
		t.Errorf("summary = %+v", snap.Summary)
	}
	if !reflect.DeepEqual(urls(snap.Records[0].Links), []string{"http://web.docker.local:8080/"}) {
		t.Errorf("links = %+v", snap.Records[0].Links)
	}
}

// A container that can only be reached over IPv6 must not vanish from the page.
func TestAnIPv6OnlyContainerIsStillListed(t *testing.T) {
	t.Parallel()
	s := New(Config{DNSSuffix: "docker.local", TargetMode: "both"}, nil)
	snap := s.buildSnapshot(resultOf(container("v6", nil, nil, []string{"fd00::9"})...))
	if got, want := addresses(snap), []string{"fd00::9"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("addresses = %v, want %v", got, want)
	}
}

// A published port that only listens on IPv6 keeps the IPv6 record that carries
// its link.
func TestAnIPv6RecordThatCarriesALinkIsKept(t *testing.T) {
	t.Parallel()
	s := New(Config{DNSSuffix: "docker.local", TargetMode: "both"}, nil)
	ports := []dockerclient.Port{{HostIP: "::", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}}
	snap := s.buildSnapshot(resultOf(container("v6web", ports, []string{"::1"}, []string{"172.17.0.2"})...))
	if got, want := addresses(snap), []string{"::1", "172.17.0.2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("addresses = %v, want %v", got, want)
	}
}

// The order Docker lists the ports in must not change which record carries the
// link.
func TestTheLinkOfAPortBoundBothWaysDoesNotDependOnTheListOrder(t *testing.T) {
	t.Parallel()
	s := New(Config{DNSSuffix: "docker.local", TargetMode: "both"}, nil)
	v4 := dockerclient.Port{HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}
	v6 := dockerclient.Port{HostIP: "::", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}
	for _, ports := range [][]dockerclient.Port{{v4, v6}, {v6, v4}} {
		entries := container("web", ports, []string{"127.0.0.1", "::1"}, nil)
		got := allURLs(s.linksByEntry(entries, []string{"web.docker.local"}))
		want := [][]string{{"http://web.docker.local:8080/"}, {}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("order %v: links = %v, want %v", ports, got, want)
		}
	}
}
