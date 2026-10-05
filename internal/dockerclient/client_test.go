package dockerclient

import (
	"net/netip"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
)

func TestConvertExtractsNameWithoutSlash(t *testing.T) {
	t.Parallel()

	got := convert(container.Summary{
		ID:    "abc123",
		Names: []string{"/nginx"},
		State: container.StateRunning,
	})
	if got.Name != "nginx" {
		t.Errorf("Name = %q, want nginx (Docker's leading slash must be stripped)", got.Name)
	}
	if got.ID != "abc123" {
		t.Errorf("ID = %q, want abc123", got.ID)
	}
	if got.State != "running" {
		t.Errorf("State = %q, want running", got.State)
	}
}

func TestConvertHandlesMultipleNames(t *testing.T) {
	t.Parallel()

	// Docker returns the container name first, then any links. The first is
	// the authoritative name.
	got := convert(container.Summary{
		Names: []string{"/web", "/linked"},
	})
	if got.Name != "web" {
		t.Errorf("Name = %q, want web", got.Name)
	}
}

func TestConvertHandlesNoNames(t *testing.T) {
	t.Parallel()

	got := convert(container.Summary{ID: "x"})
	if got.Name != "" {
		t.Errorf("Name = %q, want empty", got.Name)
	}
}

func TestConvertSkipsEmptyNames(t *testing.T) {
	t.Parallel()

	got := convert(container.Summary{Names: []string{"", "/real"}})
	if got.Name != "real" {
		t.Errorf("Name = %q, want real", got.Name)
	}
}

// The network map has no defined order, so the output must be sorted or the
// hosts file would change on every reconcile.
func TestConvertSortsNetworks(t *testing.T) {
	t.Parallel()

	for range 20 {
		got := convert(container.Summary{
			NetworkSettings: &container.NetworkSettingsSummary{
				Networks: map[string]*network.EndpointSettings{
					"zeta":  {IPAddress: netip.MustParseAddr("172.18.0.2")},
					"alpha": {IPAddress: netip.MustParseAddr("172.17.0.2")},
					"mid":   {IPAddress: netip.MustParseAddr("172.19.0.2")},
				},
			},
		})
		want := []string{"alpha", "mid", "zeta"}
		if len(got.Networks) != len(want) {
			t.Fatalf("got %d networks, want %d", len(got.Networks), len(want))
		}
		for i, n := range got.Networks {
			if n.Name != want[i] {
				t.Fatalf("network %d = %q, want %q (order must be stable)", i, n.Name, want[i])
			}
		}
	}
}

func TestConvertIPv6(t *testing.T) {
	t.Parallel()

	got := convert(container.Summary{
		NetworkSettings: &container.NetworkSettingsSummary{
			Networks: map[string]*network.EndpointSettings{
				"v6": {IPAddress: netip.MustParseAddr("fd00::2")},
			},
		},
	})
	if len(got.Networks) != 1 {
		t.Fatalf("got %d networks, want 1", len(got.Networks))
	}
	n := got.Networks[0]
	if n.IPv6 != "fd00::2" {
		t.Errorf("IPv6 = %q, want fd00::2", n.IPv6)
	}
	if n.IPv4 != "" {
		t.Errorf("IPv4 = %q, want empty for a pure IPv6 address", n.IPv4)
	}
}

func TestConvertIPv4In6IsNotTreatedAsIPv6(t *testing.T) {
	t.Parallel()

	// ::ffff:172.17.0.2 is IPv4 written in IPv6 form. Publishing it as IPv6
	// would produce a record the resolver cannot use as intended.
	got := convert(container.Summary{
		NetworkSettings: &container.NetworkSettingsSummary{
			Networks: map[string]*network.EndpointSettings{
				"bridge": {IPAddress: netip.MustParseAddr("::ffff:172.17.0.2")},
			},
		},
	})
	n := got.Networks[0]
	if n.IPv6 != "" {
		t.Errorf("IPv6 = %q, want empty for an IPv4-mapped address", n.IPv6)
	}
}

func TestConvertNilNetworkSettings(t *testing.T) {
	t.Parallel()

	got := convert(container.Summary{ID: "x"})
	if len(got.Networks) != 0 {
		t.Errorf("got %d networks, want 0 when NetworkSettings is nil", len(got.Networks))
	}
}

func TestConvertNilEndpoint(t *testing.T) {
	t.Parallel()

	got := convert(container.Summary{
		NetworkSettings: &container.NetworkSettingsSummary{
			Networks: map[string]*network.EndpointSettings{"broken": nil},
		},
	})
	if len(got.Networks) != 0 {
		t.Errorf("got %d networks, want 0 for a nil endpoint", len(got.Networks))
	}
}

func TestConvertPorts(t *testing.T) {
	t.Parallel()

	got := convert(container.Summary{
		Ports: []container.PortSummary{
			{IP: netip.MustParseAddr("0.0.0.0"), PrivatePort: 80, PublicPort: 8080, Type: "tcp"},
			{PrivatePort: 5432, Type: "tcp"}, // exposed but not published
		},
	})

	if len(got.PublishedPorts) != 2 {
		t.Fatalf("got %d ports, want 2", len(got.PublishedPorts))
	}
	if got.PublishedPorts[0].HostPort != 8080 {
		t.Errorf("HostPort = %d, want 8080", got.PublishedPorts[0].HostPort)
	}
	if got.PublishedPorts[0].ContainerPort != 80 {
		t.Errorf("ContainerPort = %d, want 80", got.PublishedPorts[0].ContainerPort)
	}
	if got.PublishedPorts[0].HostIP != "0.0.0.0" {
		t.Errorf("HostIP = %q, want 0.0.0.0", got.PublishedPorts[0].HostIP)
	}
	if got.PublishedPorts[0].Published() != true {
		t.Error("Published() = false for a published port")
	}
	if got.PublishedPorts[1].Published() != false {
		t.Error("Published() = true for a port that is only exposed")
	}
}

func TestConvertNetworkMode(t *testing.T) {
	t.Parallel()

	var hostConfig container.Summary // placeholder to keep the import obvious
	_ = hostConfig

	got := convert(container.Summary{
		HostConfig: struct {
			NetworkMode string            `json:",omitempty"`
			Annotations map[string]string `json:",omitempty"`
		}{NetworkMode: "host"},
	})
	if got.NetworkMode != "host" {
		t.Errorf("NetworkMode = %q, want host", got.NetworkMode)
	}
}

func TestConvertCreated(t *testing.T) {
	t.Parallel()

	got := convert(container.Summary{Created: 1700000000})
	if got.Created.IsZero() {
		t.Fatal("Created is zero")
	}
	if got.Created.Unix() != 1700000000 {
		t.Errorf("Created.Unix() = %d, want 1700000000", got.Created.Unix())
	}
}

func TestTrimLeadingSlash(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"/nginx":  "nginx",
		"nginx":   "nginx",
		"//nginx": "nginx",
		"":        "",
		"/":       "",
		"///":     "",
		"/a/b":    "a/b",
	}
	for in, want := range cases {
		if got := trimLeadingSlash(in); got != want {
			t.Errorf("trimLeadingSlash(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPortPublished(t *testing.T) {
	t.Parallel()

	if (Port{HostPort: 0}).Published() {
		t.Error("an unpublished port must report false")
	}
	if (Port{HostPort: 1}).Published() != true {
		t.Error("a published port must report true")
	}
	// A host port is never zero in practice, but the check must not depend on
	// that: -1 would be a bug in the converter, not a published port.
	if (Port{HostPort: -1}).Published() {
		t.Error("a negative port must not report as published")
	}
}
