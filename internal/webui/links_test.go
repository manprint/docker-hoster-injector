package webui

import (
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/mint/docker-hoster-injector/internal/dockerclient"
	"github.com/mint/docker-hoster-injector/internal/hostsfile"
	"github.com/mint/docker-hoster-injector/internal/reconcile"
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

func TestLinksForHostAddressUseNameAndHostPort(t *testing.T) {
	t.Parallel()
	s := New(Config{DNSSuffix: "docker.local", TargetMode: "both"}, nil)
	names := []string{"web.docker.local"}

	e := entry("127.0.0.1", reconcile.SideHost, "web",
		dockerclient.Port{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
		dockerclient.Port{HostIP: "127.0.0.1", HostPort: 9090, ContainerPort: 81, Protocol: "tcp"},
		dockerclient.Port{HostIP: "192.168.1.5", HostPort: 7070, ContainerPort: 82, Protocol: "tcp"},
		dockerclient.Port{ContainerPort: 83, Protocol: "tcp"}, // exposed only
	)
	got := urls(s.linksFor(e, names))
	want := []string{"http://web.docker.local:8080/", "http://web.docker.local:9090/"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("links = %v, want %v", got, want)
	}
}

func TestLinksForContainerAddressUseTheAddress(t *testing.T) {
	t.Parallel()
	s := New(Config{DNSSuffix: "docker.local", TargetMode: "both"}, nil)
	names := []string{"web.docker.local"}

	e := entry("172.17.0.2", reconcile.SideContainer, "web",
		dockerclient.Port{HostPort: 8080, ContainerPort: 80, Protocol: "tcp"},
		dockerclient.Port{ContainerPort: 80, Protocol: "tcp"}, // same port listed again
		dockerclient.Port{ContainerPort: 443, Protocol: "tcp"},
	)
	got := urls(s.linksFor(e, names))
	want := []string{"http://172.17.0.2:80/", "https://172.17.0.2:443/"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("links = %v, want %v", got, want)
	}
}

func TestLinksInContainerIPModeUseTheName(t *testing.T) {
	t.Parallel()
	s := New(Config{DNSSuffix: "docker.local", TargetMode: "container-ip"}, nil)
	e := entry("172.17.0.2", reconcile.SideContainer, "web", dockerclient.Port{ContainerPort: 80, Protocol: "tcp"})
	got := urls(s.linksFor(e, []string{"web.docker.local"}))
	if want := []string{"http://web.docker.local:80/"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("links = %v, want %v", got, want)
	}
}

func TestLinksBracketIPv6(t *testing.T) {
	t.Parallel()
	s := New(Config{DNSSuffix: "docker.local", TargetMode: "both"}, nil)
	e := entry("fd00::2", reconcile.SideContainer, "web", dockerclient.Port{ContainerPort: 8000, Protocol: "tcp"})
	got := urls(s.linksFor(e, []string{"web.docker.local"}))
	if want := []string{"http://[fd00::2]:8000/"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("links = %v, want %v", got, want)
	}
}

// Ports that do not speak HTTP, and UDP, are listed but offered no URL.
func TestLinksSkipNonWebPorts(t *testing.T) {
	t.Parallel()
	s := New(Config{DNSSuffix: "docker.local", TargetMode: "both"}, nil)
	e := entry("172.17.0.2", reconcile.SideContainer, "db",
		dockerclient.Port{ContainerPort: 5432, Protocol: "tcp"},
		dockerclient.Port{ContainerPort: 53, Protocol: "udp"},
		dockerclient.Port{ContainerPort: 8080, Protocol: "tcp"},
	)
	links := s.linksFor(e, []string{"db.docker.local"})
	if len(links) != 3 {
		t.Fatalf("links = %+v, want 3 entries", links)
	}
	for _, l := range links {
		switch l.Port {
		case 8080:
			if l.URL == "" {
				t.Error("8080/tcp has no URL")
			}
		default:
			if l.URL != "" {
				t.Errorf("%d/%s has URL %q", l.Port, l.Protocol, l.URL)
			}
		}
	}
}

func TestLinksAreNeverNull(t *testing.T) {
	t.Parallel()
	s := New(Config{DNSSuffix: "docker.local"}, nil)
	if got := s.linksFor(entry("172.17.0.2", reconcile.SideContainer, "web"), nil); got == nil {
		t.Fatal("linksFor returned nil, which marshals to null")
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

// The protocol is decided by the port inside the container: a database
// published on a high port is still not a web page.
func TestLinksJudgeTheServiceByTheContainerPort(t *testing.T) {
	t.Parallel()
	s := New(Config{DNSSuffix: "docker.local", TargetMode: "both"}, nil)
	e := entry("127.0.0.1", reconcile.SideHost, "db",
		dockerclient.Port{HostPort: 49153, ContainerPort: 5432, Protocol: "tcp"},
		dockerclient.Port{HostPort: 49154, ContainerPort: 443, Protocol: "tcp"},
	)
	links := s.linksFor(e, []string{"db.docker.local"})
	if len(links) != 2 {
		t.Fatalf("links = %+v", links)
	}
	if links[0].URL != "" {
		t.Errorf("a database port got a URL: %q", links[0].URL)
	}
	if links[1].URL != "https://db.docker.local:49154/" {
		t.Errorf("a TLS service got %q", links[1].URL)
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
