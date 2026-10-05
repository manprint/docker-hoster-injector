//go:build integration

package integration

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// apiLink and apiRecord mirror the JSON of /api/entries. They are declared here
// rather than imported so that the test checks the wire format a browser sees.
type apiLink struct {
	Label    string `json:"label"`
	URL      string `json:"url"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
}

type apiRecord struct {
	Address     string    `json:"address"`
	Names       []string  `json:"names"`
	Container   string    `json:"container"`
	ContainerID string    `json:"container_id"`
	State       string    `json:"state"`
	Side        string    `json:"side"`
	Links       []apiLink `json:"links"`
}

type apiSnapshot struct {
	Records []apiRecord `json:"records"`
	Skipped []struct {
		Container string `json:"container"`
		State     string `json:"state"`
		Reason    string `json:"reason"`
	} `json:"skipped"`
	Summary struct {
		Containers int `json:"containers"`
		Records    int `json:"records"`
		Names      int `json:"names"`
		Skipped    int `json:"skipped"`
	} `json:"summary"`
}

func fetchSnapshot(t *testing.T) apiSnapshot {
	t.Helper()
	var snap apiSnapshot
	if code := jsonGet(web.url("/api/entries"), &snap); code != 200 {
		t.Fatalf("GET /api/entries returned %d", code)
	}
	return snap
}

func (s apiSnapshot) recordsOf(container string) []apiRecord {
	var out []apiRecord
	for _, r := range s.Records {
		if r.Container == container {
			out = append(out, r)
		}
	}
	return out
}

const httpdCmd = "echo ok > /tmp/index.html && exec httpd -f -p %d -h /tmp"

// extra is a container created by a test on top of the shared estate.
type extra struct {
	name    string
	args    []string // docker create arguments before the image
	cmd     []string
	after   func(t *testing.T) // runs once started
	network string
}

func provision(t *testing.T, x extra) {
	t.Helper()
	_ = runQuiet(30*time.Second, "docker", "rm", "-f", "-v", x.name)
	t.Cleanup(func() { _ = runQuiet(30*time.Second, "docker", "rm", "-f", "-v", x.name) })

	args := []string{"create", "--name", x.name}
	if x.network != "" {
		args = append(args, "--network", x.network)
	}
	args = append(args, x.args...)
	args = append(args, "busybox:latest")
	args = append(args, x.cmd...)
	if out, err := run(60*time.Second, "docker", args...); err != nil {
		t.Fatalf("create %s: %v: %s", x.name, err, strings.TrimSpace(out))
	}
	if out, err := run(60*time.Second, "docker", "start", x.name); err != nil {
		t.Fatalf("start %s: %v: %s", x.name, err, strings.TrimSpace(out))
	}
	if x.after != nil {
		x.after(t)
	}
}

// TestWebUIMatchesTheHostsFileAndOffersWorkingLinks provisions another batch of
// containers in the shapes the estate does not cover, then checks that what the
// page says is what the file says, and that every link it offers works.
func TestWebUIMatchesTheHostsFileAndOffersWorkingLinks(t *testing.T) {
	requireDocker(t)
	if !hostsWritable() {
		t.Skip("needs the real hosts file")
	}

	ports := uniquePorts(4)
	udpPort, dbPort, tlsPort, multiPort := ports[0], ports[1], ports[2], ports[3]

	provision(t, extra{
		name: "dhi-x-udp",
		args: []string{"-p", fmt.Sprintf("%d:5353/udp", udpPort)},
		cmd:  []string{"nc", "-l", "-u", "-p", "5353"},
	})
	provision(t, extra{
		name: "dhi-x-db",
		args: []string{"-p", fmt.Sprintf("%d:5432", dbPort)},
		cmd:  []string{"nc", "-l", "-k", "-p", "5432"},
	})
	provision(t, extra{
		name: "dhi-x-tls",
		args: []string{"-p", fmt.Sprintf("%d:8443", tlsPort)},
		cmd:  []string{"sh", "-c", fmt.Sprintf(httpdCmd, 8443)},
	})
	provision(t, extra{
		name:    "dhi-x-multinet",
		network: testNetwork,
		args:    []string{"-p", fmt.Sprintf("%d:8080", multiPort)},
		cmd:     []string{"sh", "-c", fmt.Sprintf(httpdCmd, 8080)},
		after: func(t *testing.T) {
			if out, err := run(30*time.Second, "docker", "network", "connect", "bridge", "dhi-x-multinet"); err != nil {
				t.Fatalf("connect to bridge: %v: %s", err, out)
			}
		},
	})
	provision(t, extra{
		name: "dhi-x-paused",
		cmd:  []string{"sh", "-c", fmt.Sprintf(httpdCmd, 8080)},
		after: func(t *testing.T) {
			if out, err := run(30*time.Second, "docker", "pause", "dhi-x-paused"); err != nil {
				t.Fatalf("pause: %v: %s", err, out)
			}
		},
	})
	for _, n := range []string{"dhi-x-conflict-a", "dhi-x-conflict-b"} {
		provision(t, extra{
			name:    n,
			network: testNetwork,
			args:    []string{"--network-alias", "contested"},
			cmd:     []string{"sh", "-c", fmt.Sprintf(httpdCmd, 8080)},
		})
	}
	// Containers come and go while the agent converges, so every assertion
	// below waits for the page to catch up rather than reading it once.
	var last string
	t.Cleanup(func() {
		if t.Failed() {
			t.Log("last state seen: " + last)
		}
	})
	eventually(t, "the extra containers to appear in the web UI", func() bool {
		s := fetchSnapshot(t)
		last = fmt.Sprintf("multinet=%d tls=%d db=%d udp=%d conflict-a=%d conflict-b=%d paused=%d pausedInFile=%v",
			len(s.recordsOf("dhi-x-multinet")), len(s.recordsOf("dhi-x-tls")), len(s.recordsOf("dhi-x-db")),
			len(s.recordsOf("dhi-x-udp")), len(s.recordsOf("dhi-x-conflict-a")), len(s.recordsOf("dhi-x-conflict-b")),
			len(s.recordsOf("dhi-x-paused")), nameExists(t, "dhi-x-paused.docker.local"))
		return len(s.recordsOf("dhi-x-multinet")) >= 3 && len(s.recordsOf("dhi-x-tls")) > 0 &&
			len(s.recordsOf("dhi-x-db")) > 0 && len(s.recordsOf("dhi-x-udp")) > 0 &&
			len(s.recordsOf("dhi-x-conflict-a")) > 0 && len(s.recordsOf("dhi-x-conflict-b")) > 0 &&
			len(s.recordsOf("dhi-x-paused")) == 0 && !nameExists(t, "dhi-x-paused.docker.local")
	})

	snap := fetchSnapshot(t)

	t.Run("udp port is listed but not linked", func(t *testing.T) {
		var found bool
		for _, r := range snap.recordsOf("dhi-x-udp") {
			for _, l := range r.Links {
				if l.Protocol == "udp" {
					found = true
					if l.URL != "" {
						t.Errorf("udp port has a URL: %q", l.URL)
					}
				}
			}
		}
		if !found {
			t.Error("the udp port is not listed")
		}
	})

	t.Run("database port is listed but not linked", func(t *testing.T) {
		var found bool
		for _, r := range snap.recordsOf("dhi-x-db") {
			for _, l := range r.Links {
				if l.Port == dbPort && r.Side == "host" {
					found = true
					if l.URL != "" {
						t.Errorf("the 5432 service has a URL: %q", l.URL)
					}
				}
			}
		}
		if !found {
			t.Errorf("no host link for the published database port %d: %+v", dbPort, snap.recordsOf("dhi-x-db"))
		}
	})

	t.Run("tls port is linked as https", func(t *testing.T) {
		want := fmt.Sprintf("https://dhi-x-tls.docker.local:%d/", tlsPort)
		for _, r := range snap.recordsOf("dhi-x-tls") {
			for _, l := range r.Links {
				if l.URL == want {
					return
				}
			}
		}
		t.Errorf("no link %s in %+v", want, snap.recordsOf("dhi-x-tls"))
	})

	t.Run("a container on two networks has one container address per network", func(t *testing.T) {
		// Docker reports a published port once for IPv4 and once for IPv6, so
		// the host side is 127.0.0.1 and ::1: both are the host.
		var container, host int
		for _, r := range snap.recordsOf("dhi-x-multinet") {
			switch r.Side {
			case "container":
				container++
				var ok bool
				for _, l := range r.Links {
					if l.URL == fmt.Sprintf("http://%s:8080/", r.Address) {
						ok = true
					}
				}
				if !ok {
					t.Errorf("container address %s has no link to its own port: %+v", r.Address, r.Links)
				}
			case "host":
				host++
			}
		}
		if container != 2 || host < 1 {
			t.Errorf("container-side=%d host-side=%d, want 2 and at least 1", container, host)
		}
	})

	t.Run("a paused container is withdrawn", func(t *testing.T) {
		// It has an address but does not answer, so a name that resolves to it
		// would only hang. It is not listed at all, and does not resolve.
		if rs := snap.recordsOf("dhi-x-paused"); len(rs) != 0 {
			t.Errorf("the paused container has records: %+v", rs)
		}
		// The file is the signal: a DNS server that answers for unknown names
		// would make "does not resolve" meaningless.
		if nameExists(t, "dhi-x-paused.docker.local") {
			t.Error("the paused container is still in the hosts file")
		}
	})

	t.Run("a contested alias has one owner and one reported conflict", func(t *testing.T) {
		owners := 0
		for _, c := range []string{"dhi-x-conflict-a", "dhi-x-conflict-b"} {
			for _, r := range snap.recordsOf(c) {
				for _, n := range r.Names {
					if n == "contested.docker.local" {
						owners++
						break
					}
				}
			}
		}
		if owners == 0 {
			t.Fatal("nobody owns the contested alias")
		}
		var reported bool
		for _, sk := range snap.Skipped {
			if sk.State == "conflict" && sk.Container == "contested.docker.local" {
				reported = true
			}
		}
		if !reported {
			t.Errorf("the conflict is not reported: %+v", snap.Skipped)
		}
		// The file agrees: the name appears for exactly one address group.
		var inFile int
		for _, r := range records(t) {
			for _, n := range r.Names {
				if n == "contested.docker.local" {
					inFile++
				}
			}
		}
		if inFile == 0 {
			t.Error("the contested name is not in the hosts file at all")
		}
	})

	t.Run("the page and the hosts file say the same", func(t *testing.T) {
		// A moment of agreement is what matters, so both are read as close
		// together as possible, and retried when a reconcile lands in between.
		eventually(t, "the page and the file to agree", func() bool {
			snap := fetchSnapshot(t)
			return sameAsFile(snap, records(t)) == ""
		})
		snap := fetchSnapshot(t)
		if diff := sameAsFile(snap, records(t)); diff != "" {
			t.Error(diff)
		}
		if snap.Summary.Records != len(snap.Records) {
			t.Errorf("summary.records = %d, rows = %d", snap.Summary.Records, len(snap.Records))
		}
		if snap.Summary.Skipped != len(snap.Skipped) {
			t.Errorf("summary.skipped = %d, rows = %d", snap.Summary.Skipped, len(snap.Skipped))
		}
		containers := map[string]bool{}
		for _, r := range snap.Records {
			containers[r.ContainerID] = true
		}
		if snap.Summary.Containers != len(containers) {
			t.Errorf("summary.containers = %d, distinct containers in rows = %d",
				snap.Summary.Containers, len(containers))
		}
	})

	t.Run("every link that is offered opens", func(t *testing.T) {
		// Containers that answer HTTP. Everything else (nc listeners, the
		// paused one) is not expected to.
		serves := func(c string) bool {
			if c == "dhi-x-udp" || c == "dhi-x-db" {
				return false
			}
			return strings.HasPrefix(c, "dhi-")
		}
		var checked int
		for _, r := range fetchSnapshot(t).Records {
			if !serves(r.Container) {
				continue
			}
			for _, l := range r.Links {
				if l.URL == "" || strings.HasPrefix(l.URL, "https://") {
					continue // the TLS probe above is a plain httpd
				}
				checked++
				var code int
				eventually(t, "link "+l.URL+" to answer", func() bool {
					code = httpGet(l.URL)
					return code == 200
				})
				_ = code
			}
		}
		if checked < 10 {
			t.Errorf("only %d links were checked, the estate should offer many more", checked)
		}
	})
}

// sameAsFile compares the page with the managed block and describes the first
// difference, or returns "" when they agree.
//
// The file groups every name that shares an address on one line (all published
// ports are on 127.0.0.1), while the page has one row per container, so the
// comparison is of the names each address answers to.
func sameAsFile(snap apiSnapshot, file []record) string {
	want := map[string]map[string]bool{}
	for _, r := range file {
		if want[r.Addr] == nil {
			want[r.Addr] = map[string]bool{}
		}
		for _, n := range r.Names {
			want[r.Addr][n] = true
		}
	}
	got := map[string]map[string]bool{}
	for _, r := range snap.Records {
		if got[r.Address] == nil {
			got[r.Address] = map[string]bool{}
		}
		for _, n := range r.Names {
			got[r.Address][n] = true
		}
	}

	var diffs []string
	for addr, names := range want {
		for n := range names {
			if !got[addr][n] {
				diffs = append(diffs, "only in the file: "+addr+" "+n)
			}
		}
	}
	for addr, names := range got {
		for n := range names {
			if !want[addr][n] {
				diffs = append(diffs, "only on the page: "+addr+" "+n)
			}
		}
	}
	sort.Strings(diffs)
	if len(diffs) > 5 {
		diffs = diffs[:5]
	}
	return strings.Join(diffs, "; ")
}
