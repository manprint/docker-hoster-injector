package reconcile

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mint/docker-hoster-injector/internal/config"
	"github.com/mint/docker-hoster-injector/internal/dockerclient"
	"github.com/mint/docker-hoster-injector/internal/hostsfile"
)

// testConfig returns a validated default configuration for the given mode.
func testConfig(t *testing.T, mode config.TargetMode) config.Config {
	t.Helper()
	cfg, err := config.LoadFrom(func(k string) (string, bool) {
		switch k {
		case config.EnvTargetMode:
			return string(mode), true
		default:
			return "", false
		}
	})
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	return cfg
}

// container builds a minimal running container for tests.
func container(id, name string, opts ...func(*dockerclient.Container)) dockerclient.Container {
	c := dockerclient.Container{
		ID:          id,
		Name:        name,
		State:       "running",
		Created:     time.Unix(1700000000, 0).UTC(),
		Networks:    []dockerclient.Network{{Name: "bridge", IPv4: "172.17.0.2"}},
		NetworkMode: "bridge",
	}
	for _, o := range opts {
		o(&c)
	}
	return c
}

func withPort(hostIP string, hostPort, containerPort int) func(*dockerclient.Container) {
	return func(c *dockerclient.Container) {
		c.PublishedPorts = append(c.PublishedPorts, dockerclient.Port{
			HostIP:        hostIP,
			HostPort:      hostPort,
			ContainerPort: containerPort,
			Protocol:      "tcp",
		})
	}
}

func withNetworkMode(mode string) func(*dockerclient.Container) {
	return func(c *dockerclient.Container) { c.NetworkMode = mode }
}

func withState(s string) func(*dockerclient.Container) {
	return func(c *dockerclient.Container) { c.State = s }
}

// names returns every published name, sorted, for easy comparison.
func names(res Result) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, e := range res.Entries {
		for _, n := range e.Names {
			if _, dup := seen[n]; dup {
				continue
			}
			seen[n] = struct{}{}
			out = append(out, n)
		}
	}
	return out
}

func addressesFor(res Result, name string) []string {
	var out []string
	for _, e := range res.Entries {
		for _, n := range e.Names {
			if n == name {
				out = append(out, e.IP.String())
			}
		}
	}
	return out
}

func TestReconcileSimpleContainer(t *testing.T) {
	t.Parallel()

	r := New(testConfig(t, config.TargetModeBoth))
	res := r.Reconcile([]dockerclient.Container{container("id1", "nginx")})

	if len(res.Skipped) != 0 {
		t.Fatalf("unexpected skips: %+v", res.Skipped)
	}
	want := []string{"nginx.docker.local"}
	if got := names(res); !reflect.DeepEqual(got, want) {
		t.Errorf("names = %q, want %q", got, want)
	}
	if got := addressesFor(res, "nginx.docker.local"); !reflect.DeepEqual(got, []string{"172.17.0.2"}) {
		t.Errorf("addresses = %q, want [172.17.0.2]", got)
	}
}

// This is the exact scenario from the specification.
func TestReconcilePublishedPortMakesHostAddressResolveFirst(t *testing.T) {
	t.Parallel()

	r := New(testConfig(t, config.TargetModeBoth))
	res := r.Reconcile([]dockerclient.Container{
		container("id1", "nginx", withPort("", 8080, 80)),
	})

	got := addressesFor(res, "nginx.docker.local")
	if len(got) != 2 {
		t.Fatalf("got %d addresses %q, want 2", len(got), got)
	}
	// The host address must come first, otherwise nginx.docker.local:8080
	// would look for port 8080 inside the container, where nothing listens.
	if got[0] != "127.0.0.1" {
		t.Errorf("first address = %q, want 127.0.0.1 so the published port applies", got[0])
	}
	if got[1] != "172.17.0.2" {
		t.Errorf("second address = %q, want the container address", got[1])
	}
}

func TestTargetModePublishedOnlyHostAddresses(t *testing.T) {
	t.Parallel()

	r := New(testConfig(t, config.TargetModePublished))
	res := r.Reconcile([]dockerclient.Container{
		container("id1", "nginx", withPort("", 8080, 80)),
	})

	got := addressesFor(res, "nginx.docker.local")
	if !reflect.DeepEqual(got, []string{"127.0.0.1"}) {
		t.Errorf("addresses = %q, want [127.0.0.1]", got)
	}
}

func TestTargetModeContainerIPOnlyContainerAddresses(t *testing.T) {
	t.Parallel()

	r := New(testConfig(t, config.TargetModeContainerIP))
	res := r.Reconcile([]dockerclient.Container{
		container("id1", "nginx", withPort("", 8080, 80)),
	})

	got := addressesFor(res, "nginx.docker.local")
	if !reflect.DeepEqual(got, []string{"172.17.0.2"}) {
		t.Errorf("addresses = %q, want [172.17.0.2]", got)
	}
}

// A container with no published ports has no host-side address, so in
// "published" mode there is nothing to publish.
func TestTargetModePublishedWithNoPorts(t *testing.T) {
	t.Parallel()

	r := New(testConfig(t, config.TargetModePublished))
	res := r.Reconcile([]dockerclient.Container{container("id1", "nginx")})

	if len(res.Entries) != 0 {
		t.Errorf("got %d entries, want 0: nothing is published on the host", len(res.Entries))
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Reason != SkipNoPublishedAddress {
		t.Errorf("skips = %+v, want one with reason %q", res.Skipped, SkipNoPublishedAddress)
	}
}

// --- network host --------------------------------------------------------

// A container sharing the host network has no address of its own. Publishing
// it would make every host service reachable under the container's name, and
// two such containers would both claim the host's address.
func TestReconcileSkipsHostNetworkContainers(t *testing.T) {
	t.Parallel()

	r := New(testConfig(t, config.TargetModeBoth))
	res := r.Reconcile([]dockerclient.Container{
		container("id1", "hostapp", withNetworkMode("host")),
	})

	if len(res.Entries) != 0 {
		t.Fatalf("got %d entries for a host-network container, want 0: %+v",
			len(res.Entries), res.Entries)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("got %d skips, want 1: %+v", len(res.Skipped), res.Skipped)
	}
	if res.Skipped[0].Reason != SkipNetworkHost {
		t.Errorf("reason = %q, want %q", res.Skipped[0].Reason, SkipNetworkHost)
	}
}

// The container list from Docker for a host-network container carries no
// endpoints, but the mode field is what decides. Defence in depth: if a future
// API version did report an address, the container must still be skipped.
func TestReconcileSkipsHostNetworkEvenWithAnAddress(t *testing.T) {
	t.Parallel()

	r := New(testConfig(t, config.TargetModeBoth))
	res := r.Reconcile([]dockerclient.Container{
		container("id1", "hostapp", withNetworkMode("host"), withPort("0.0.0.0", 8080, 80)),
	})

	for _, e := range res.Entries {
		t.Errorf("host-network container produced an entry: %+v", e)
		// Specifically: it must never carry a routable address.
		if e.IP.String() == "172.17.0.2" {
			t.Error("a host-network container was published with the bridge address")
		}
	}
}

func TestReconcileSkipsSeveralHostNetworkContainers(t *testing.T) {
	t.Parallel()

	// They must all be skipped, and none of them may appear.
	r := New(testConfig(t, config.TargetModeBoth))
	res := r.Reconcile([]dockerclient.Container{
		container("id1", "a", withNetworkMode("host")),
		container("id2", "b", withNetworkMode("host")),
		container("id3", "c", withNetworkMode("host")),
	})

	if len(res.Entries) != 0 {
		t.Errorf("got %d entries, want 0", len(res.Entries))
	}
	if len(res.Skipped) != 3 {
		t.Errorf("got %d skips, want 3", len(res.Skipped))
	}
	for _, s := range res.Skipped {
		if s.Reason != SkipNetworkHost {
			t.Errorf("%s: reason = %q, want %q", s.ContainerName, s.Reason, SkipNetworkHost)
		}
	}
}

// The network mode is compared case-insensitively because it is operator
// supplied and Docker does not normalise it.
func TestReconcileHostNetworkModeIsCaseInsensitive(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{"host", "HOST", "Host"} {
		r := New(testConfig(t, config.TargetModeBoth))
		res := r.Reconcile([]dockerclient.Container{
			container("id1", "app", withNetworkMode(mode)),
		})
		if len(res.Entries) != 0 {
			t.Errorf("mode %q: got %d entries, want 0", mode, len(res.Entries))
		}
	}
}

// A host-network container must not steal or shadow the name of a bridged one.
func TestHostNetworkDoesNotCollideWithBridgedContainer(t *testing.T) {
	t.Parallel()

	r := New(testConfig(t, config.TargetModeBoth))
	res := r.Reconcile([]dockerclient.Container{
		container("id1", "shared", withNetworkMode("host")),
		container("id2", "shared2", withPort("", 8080, 80)),
	})

	got := addressesFor(res, "shared2.docker.local")
	if !reflect.DeepEqual(got, []string{"127.0.0.1", "172.17.0.2"}) {
		t.Errorf("addresses = %q, want [127.0.0.1 172.17.0.2]", got)
	}
	if len(res.Conflicts) != 0 {
		t.Errorf("unexpected conflicts: %+v", res.Conflicts)
	}
}

func TestReconcileSkipsNoneNetwork(t *testing.T) {
	t.Parallel()

	r := New(testConfig(t, config.TargetModeBoth))
	res := r.Reconcile([]dockerclient.Container{
		container("id1", "isolated", withNetworkMode("none"), func(c *dockerclient.Container) {
			c.Networks = nil
		}),
	})

	if len(res.Entries) != 0 {
		t.Errorf("got %d entries, want 0", len(res.Entries))
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Reason != SkipNoNetwork {
		t.Errorf("skips = %+v, want one with reason %q", res.Skipped, SkipNoNetwork)
	}
}

// --- states --------------------------------------------------------------

func TestReconcileStateFiltering(t *testing.T) {
	t.Parallel()

	cases := map[string]SkipReason{
		"exited":     SkipNotRunning,
		"created":    SkipNotRunning,
		"restarting": SkipNotRunning,
		"dead":       SkipNotRunning,
		"removing":   SkipNotRunning,
		"paused":     SkipPaused,
		// A state no current Docker version produces must not be published
		// either: the safe default for an unknown state is to hide it.
		"something-new": SkipNotRunning,
		"":              SkipNotRunning,
	}

	for state, wantReason := range cases {
		r := New(testConfig(t, config.TargetModeBoth))
		res := r.Reconcile([]dockerclient.Container{
			container("id1", "app", withState(state)),
		})
		if len(res.Entries) != 0 {
			t.Errorf("state %q: got %d entries, want 0", state, len(res.Entries))
		}
		if len(res.Skipped) != 1 {
			t.Errorf("state %q: got %d skips, want 1", state, len(res.Skipped))
			continue
		}
		if res.Skipped[0].Reason != wantReason {
			t.Errorf("state %q: reason = %q, want %q", state, res.Skipped[0].Reason, wantReason)
		}
	}
}

func TestReconcileSkipsContainerWithoutAddress(t *testing.T) {
	t.Parallel()

	r := New(testConfig(t, config.TargetModeBoth))
	res := r.Reconcile([]dockerclient.Container{
		container("id1", "starting", func(c *dockerclient.Container) {
			c.Networks = []dockerclient.Network{{Name: "bridge"}}
		}),
	})

	if len(res.Entries) != 0 {
		t.Errorf("got %d entries, want 0", len(res.Entries))
	}
	if len(res.Skipped) != 1 || res.Skipped[0].Reason != SkipNoAddress {
		t.Errorf("skips = %+v, want one with reason %q", res.Skipped, SkipNoAddress)
	}
}

// --- names ---------------------------------------------------------------

func TestReconcileComposeNameVariants(t *testing.T) {
	t.Parallel()

	r := New(testConfig(t, config.TargetModeBoth))
	res := r.Reconcile([]dockerclient.Container{container("id1", "myproject_web_1")})

	want := []string{
		"myproject_web_1.docker.local",
		"myproject--web--1.docker.local",
	}
	got := names(res)
	if len(got) != 2 {
		t.Fatalf("names = %q, want %q", got, want)
	}
	for _, w := range want {
		if !contains(got, w) {
			t.Errorf("names %q missing %q", got, w)
		}
	}
}

func TestReconcilePublishesNetworkAliases(t *testing.T) {
	t.Parallel()

	r := New(testConfig(t, config.TargetModeBoth))
	res := r.Reconcile([]dockerclient.Container{
		container("id1", "app", func(c *dockerclient.Container) {
			c.Networks = []dockerclient.Network{{
				Name:     "proj_default",
				IPv4:     "172.18.0.3",
				Aliases:  []string{"web", "frontend"},
				DNSNames: []string{"app", "web"},
			}}
		}),
	})

	got := names(res)
	for _, want := range []string{
		"app.docker.local",
		"web.docker.local",
		"frontend.docker.local",
	} {
		if !contains(got, want) {
			t.Errorf("names %q missing alias %q", got, want)
		}
	}
}

func TestReconcileRejectsReservedNames(t *testing.T) {
	t.Parallel()

	// A container called "localhost" must not be published, because it would
	// break the host's own resolution.
	r := New(testConfig(t, config.TargetModeBoth))
	res := r.Reconcile([]dockerclient.Container{container("id1", "localhost")})

	if len(res.Entries) != 0 {
		t.Errorf("got %d entries for a reserved name, want 0: %+v", len(res.Entries), res.Entries)
	}
	if len(res.Skipped) != 1 {
		t.Fatalf("got %d skips, want 1: %+v", len(res.Skipped), res.Skipped)
	}
	if res.Skipped[0].Reason != SkipReservedName {
		t.Errorf("reason = %q, want %q: a reserved name is a naming mistake the "+
			"operator must see, not a sanitiser failure",
			res.Skipped[0].Reason, SkipReservedName)
	}
}

// The raw name must survive even when the sanitised form would collide, so that
// the container stays reachable.
func TestReconcileKeepsRawNameWhenVariantsCollide(t *testing.T) {
	t.Parallel()

	r := New(testConfig(t, config.TargetModeBoth))
	res := r.Reconcile([]dockerclient.Container{
		container("id1", "web_1"),
		container("id2", "web-1"),
	})

	// Both raw names must be owned by their own container.
	owners := map[string]string{}
	for _, e := range res.Entries {
		for _, n := range e.Names {
			owners[n] = e.ContainerID
		}
	}
	if owners["web_1.docker.local"] != "id1" {
		t.Errorf("web_1.docker.local owned by %q, want id1", owners["web_1.docker.local"])
	}
	if owners["web-1.docker.local"] != "id2" {
		t.Errorf("web-1.docker.local owned by %q, want id2", owners["web-1.docker.local"])
	}
}

// The older container keeps a contested name.
//
// Two containers cannot share a raw name, since Docker guarantees those are
// unique. The realistic collision is therefore between sanitised variants:
// "a__b" and "a--b" both reduce to "a--b", which the sanitizer cannot avoid
// because its output alphabet is smaller than its input alphabet.
func TestReconcileCollisionPrefersOlderContainer(t *testing.T) {
	t.Parallel()

	older := container("id-old", "a--b")
	older.Created = time.Unix(1000, 0).UTC()
	newer := container("id-new", "a__b")
	newer.Created = time.Unix(2000, 0).UTC()

	r := New(testConfig(t, config.TargetModeBoth))
	res := r.Reconcile([]dockerclient.Container{newer, older})

	if len(res.Conflicts) != 1 {
		t.Fatalf("got %d conflicts, want 1: %+v", len(res.Conflicts), res.Conflicts)
	}
	c := res.Conflicts[0]
	if c.Name != "a--b.docker.local" {
		t.Errorf("contested name = %q, want a--b.docker.local", c.Name)
	}
	if c.Winner.Owner != "id-old" {
		t.Errorf("winner = %q, want id-old (the older container)", c.Winner.Owner)
	}

	// Both containers stay reachable: each keeps its own raw name, and only
	// the shared sanitised variant is contested.
	owners := map[string]string{}
	for _, e := range res.Entries {
		for _, n := range e.Names {
			owners[n] = e.ContainerID
		}
	}
	if owners["a--b.docker.local"] != "id-old" {
		t.Errorf("a--b.docker.local owned by %q, want id-old", owners["a--b.docker.local"])
	}
	if owners["a__b.docker.local"] != "id-new" {
		t.Errorf("a__b.docker.local owned by %q, want id-new: a container must never become unreachable",
			owners["a__b.docker.local"])
	}
}

// --- determinism ---------------------------------------------------------

func TestReconcileIsDeterministic(t *testing.T) {
	t.Parallel()

	containers := []dockerclient.Container{
		container("id1", "alpha", withPort("", 8080, 80)),
		container("id2", "beta", withPort("192.168.1.5", 9090, 90)),
		container("id3", "gamma_1"),
		container("id4", "delta", func(c *dockerclient.Container) {
			c.Networks = []dockerclient.Network{
				{Name: "z", IPv4: "172.20.0.2"},
				{Name: "a", IPv4: "172.21.0.2"},
			}
		}),
	}

	r := New(testConfig(t, config.TargetModeBoth))
	first := render(r.Reconcile(containers))
	for i := 0; i < 50; i++ {
		if got := render(r.Reconcile(containers)); got != first {
			t.Fatalf("iteration %d differs:\n%s\n---\n%s", i, first, got)
		}
	}
}

// render produces the file text a given result would write.
func render(res Result) string {
	return string(hostsfile.Render(&hostsfile.File{}, res.HostsEntries()))
}

func TestReconcileEmptyInput(t *testing.T) {
	t.Parallel()

	r := New(testConfig(t, config.TargetModeBoth))
	res := r.Reconcile(nil)
	if len(res.Entries) != 0 || len(res.Skipped) != 0 {
		t.Errorf("got %+v, want an empty result", res)
	}
}

// --- addresses -----------------------------------------------------------

func TestHostAddressesBindHandling(t *testing.T) {
	t.Parallel()

	cases := []struct {
		bind string
		want string
	}{
		{"", "127.0.0.1"},              // no explicit bind: listens everywhere
		{"0.0.0.0", "127.0.0.1"},       // explicit wildcard
		{"::", "::1"},                  // IPv6 wildcard
		{"[::]", "::1"},                // bracketed IPv6 wildcard
		{"192.168.1.5", "192.168.1.5"}, // explicit address is used as given
		{"127.0.0.1", "127.0.0.1"},
	}

	for _, tc := range cases {
		c := container("id1", "app", withPort(tc.bind, 8080, 80))
		got := hostAddresses(c)
		if !contains(got, tc.want) {
			t.Errorf("hostAddresses(bind=%q) = %q, want it to contain %q", tc.bind, got, tc.want)
		}
	}
}

// An unspecified address must never reach the file: it would point a name at
// every local interface.
func TestIsUsableAddrRejectsUnspecified(t *testing.T) {
	t.Parallel()

	rejected := []string{"", "0.0.0.0", "::", "[::]", "not-an-ip", "999.999.999.999"}
	for _, s := range rejected {
		if isUsableAddr(s) {
			t.Errorf("isUsableAddr(%q) = true, want false", s)
		}
	}

	accepted := []string{"127.0.0.1", "172.17.0.2", "10.0.0.1", "fd00::2", "192.168.1.5"}
	for _, s := range accepted {
		if !isUsableAddr(s) {
			t.Errorf("isUsableAddr(%q) = false, want true", s)
		}
	}
}

func TestReconcileMultipleNetworksAndIPv6(t *testing.T) {
	t.Parallel()

	r := New(testConfig(t, config.TargetModeContainerIP))
	res := r.Reconcile([]dockerclient.Container{
		container("id1", "dual", func(c *dockerclient.Container) {
			c.Networks = []dockerclient.Network{
				{Name: "v4only", IPv4: "172.17.0.2"},
				{Name: "v6only", IPv6: "fd00::2"},
			}
		}),
	})

	got := addressesFor(res, "dual.docker.local")
	if len(got) != 2 {
		t.Fatalf("got %d addresses %q, want 2", len(got), got)
	}
	if !contains(got, "172.17.0.2") || !contains(got, "fd00::2") {
		t.Errorf("addresses = %q, want both families", got)
	}
}

// The rendered output must never contain an unusable address.
func TestReconcileNeverEmitsUnspecifiedAddress(t *testing.T) {
	t.Parallel()

	r := New(testConfig(t, config.TargetModeBoth))
	res := r.Reconcile([]dockerclient.Container{
		container("id1", "app", withPort("0.0.0.0", 80, 80), func(c *dockerclient.Container) {
			c.Networks = []dockerclient.Network{{Name: "bridge", IPv4: "0.0.0.0"}}
		}),
	})

	// The container's own address was "0.0.0.0", which must be rejected, but
	// the host address derived from the wildcard bind is legitimately
	// 127.0.0.1 and must survive.
	if len(res.Entries) != 1 {
		t.Fatalf("got %d entries, want 1 (only the host address): %+v", len(res.Entries), res.Entries)
	}
	got := addressesFor(res, "app.docker.local")
	if !reflect.DeepEqual(got, []string{"127.0.0.1"}) {
		t.Errorf("addresses = %q, want [127.0.0.1]: the container address 0.0.0.0 must be dropped", got)
	}
	for _, e := range res.Entries {
		if e.IP.IsUnspecified() {
			t.Errorf("entry with an unspecified address: %+v", e)
		}
	}
}

func TestHostsEntriesAndSummary(t *testing.T) {
	t.Parallel()

	r := New(testConfig(t, config.TargetModeBoth))
	res := r.Reconcile([]dockerclient.Container{
		container("id1", "alpha", withPort("", 8080, 80)),
	})

	entries := res.HostsEntries()
	if len(entries) != 2 {
		t.Fatalf("got %d hosts entries, want 2 (one per address)", len(entries))
	}
	for _, e := range entries {
		if len(e.Names) == 0 {
			t.Errorf("entry with no names: %+v", e)
		}
	}

	summary := res.Summary("id1")
	if len(summary) != 2 {
		t.Errorf("Summary = %q, want the name twice (once per address)", summary)
	}
	if got := res.Summary("nonexistent"); len(got) != 0 {
		t.Errorf("Summary of an unknown container = %q, want empty", got)
	}
}

// The rendered file must be a valid hosts file with exactly one block.
func TestReconcileRendersValidHostsFile(t *testing.T) {
	t.Parallel()

	r := New(testConfig(t, config.TargetModeBoth))
	res := r.Reconcile([]dockerclient.Container{
		container("id1", "alpha", withPort("", 8080, 80)),
		container("id2", "myproject_web_1"),
		container("id3", "hostapp", withNetworkMode("host")),
		container("id4", "stopped", withState("exited")),
	})

	out := render(res)
	if n := strings.Count(out, hostsfile.BeginMarker); n != 1 {
		t.Errorf("got %d block headers, want 1:\n%s", n, out)
	}
	if n := strings.Count(out, hostsfile.EndMarker); n != 1 {
		t.Errorf("got %d block footers, want 1:\n%s", n, out)
	}
	// The skipped containers must not appear.
	for _, absent := range []string{"hostapp", "stopped"} {
		if strings.Contains(out, absent) {
			t.Errorf("a skipped container (%s) appears in the file:\n%s", absent, out)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
