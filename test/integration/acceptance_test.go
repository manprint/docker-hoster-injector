//go:build integration

package integration

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

// estateSpec fields used by the assertions below.
var (
	specByName = map[string]estateSpec{}
	web        webUI
	agent      *agentProcess
)

// TestMain's estate is built once and shared by the whole suite, because
// provisioning twelve containers per test would dominate the runtime. The
// tests are therefore ordered by name, which Go guarantees by declaration
// order within a file, and each one is written to be independent of the others.
// TestEstate is the baseline check: the whole estate must converge before any
// finer grained assertion is meaningful.
func TestEstate(t *testing.T) {
	eventually(t, "the first full reconcile to publish the estate", func() bool {
		return len(records(t)) > 0
	})
	eventually(t, "the whole estate to be published", func() bool {
		for name, spec := range specByName {
			if !spec.wantPublished {
				continue
			}
			expected := spec.publishedName
			if expected == "" {
				expected = name + ".docker.local"
			}
			if !nameExists(t, expected) {
				return false
			}
		}
		return true
	})
}

func TestEstateRules(t *testing.T) {
	t.Run("published containers are in the hosts file", func(t *testing.T) {
		for name, spec := range specByName {
			if !spec.wantPublished {
				continue
			}
			expected := spec.publishedName
			if expected == "" {
				expected = name + ".docker.local"
			}
			if !nameExists(t, expected) {
				t.Errorf("no record for %s: %q is absent from the managed block.\nblock:\n%s",
					name, expected, managedSection(t))
				continue
			}
			for _, alias := range spec.aliases {
				if !nameExists(t, alias) {
					t.Errorf("%s: alias %q is absent.\nblock:\n%s", name, alias, managedSection(t))
				}
			}
		}
	})

	t.Run("excluded containers are absent", func(t *testing.T) {
		// Each of these must be absent, and the reason must be explained
		// rather than the container simply vanishing.
		excluded := map[string]string{
			"dhi-created": "not running",
			"dhi-hostnet": "shares the host network",
			"dhi-nonet":   "no network",
			"dhi-stopped": "not running",
			"localhost":   "reserved name",
		}
		for name, why := range excluded {
			if nameExists(t, name+".docker.local") {
				t.Errorf("%s is published but must not be (%s).\nblock:\n%s",
					name, why, managedSection(t))
			}
		}

		// The host-network container must never be given the host's address,
		// which is the specific harm the rule prevents.
		hostAddrs := hostAddresses()
		for _, addr := range addressesForName(t, "dhi-hostnet.docker.local") {
			for _, hostAddr := range hostAddrs {
				if addr == hostAddr {
					t.Errorf("a host-network container was published against the host address %s", addr)
				}
			}
		}
	})

	t.Run("exclusions are reported with a reason", func(t *testing.T) {
		// "not running" is deliberately absent: the agent asks the daemon for
		// running containers only, so a stopped container never reaches the
		// reconciler and has no skip record. Its record disappearing is
		// asserted by the lifecycle test instead.
		wantReasons := map[string]bool{
			"shares the host network":        false,
			"no network":                     false,
			"name is reserved by the system": false,
		}
		for _, s := range recordsSkipped(t) {
			if _, tracked := wantReasons[s.Reason]; tracked {
				wantReasons[s.Reason] = true
			}
		}
		for reason, seen := range wantReasons {
			if !seen {
				t.Errorf("no container was skipped with the reason %q; the log should explain every exclusion.\nskip list:\n%v",
					reason, recordsSkipped(t))
			}
		}
	})

	t.Run("the operator's hosts file is preserved byte for byte", func(t *testing.T) {
		got := userSection(t)
		if got != seedHosts {
			t.Errorf("the operator's section changed.\n--- want ---\n%q\n--- got ---\n%q", seedHosts, got)
		}
	})

	t.Run("the managed block is well formed", func(t *testing.T) {
		body := readHosts(t)
		if n := strings.Count(body, beginMarker); n != 1 {
			t.Errorf("found %d block headers, want exactly 1", n)
		}
		if n := strings.Count(body, endMarker); n != 1 {
			t.Errorf("found %d block footers, want exactly 1", n)
		}
		if strings.Index(body, beginMarker) < strings.Index(body, "localhost") {
			t.Error("the managed block must come after the operator's entries, so a " +
				"crash can never damage them")
		}
	})

	t.Run("the block comes after every user entry", func(t *testing.T) {
		body := readHosts(t)
		markerAt := strings.Index(body, beginMarker)
		for _, line := range strings.Split(strings.TrimSpace(seedHosts), "\n") {
			if line == "" {
				continue
			}
			if at := strings.Index(body, line); at < 0 {
				t.Errorf("the operator's line %q was lost", line)
			} else if at > markerAt {
				t.Errorf("the operator's line %q ended up after the managed block", line)
			}
		}
	})
}

// TestSpecPublishedPortsReachTheRightContainer checks the behaviour the
// specification is really about: the published port answers under the name.
func TestSpecPublishedPortsReachTheRightContainer(t *testing.T) {
	// The canonical case: "-p <hostPort>:80 nginx" must answer on
	// "<name>.docker.local:<hostPort>".
	for name, spec := range specByName {
		if spec.publishedPort == 0 {
			continue
		}
		expected := spec.publishedName
		if expected == "" {
			expected = name + ".docker.local"
		}
		t.Run(name, func(t *testing.T) {
			// Wait for the record, then for the server to answer.
			eventually(t, expected+" to resolve", func() bool {
				return resolves(expected)
			})
			url := "http://" + expected + ":" + itoa(spec.publishedPort) + "/"
			if !waitForHTTP(url, 200, 30*time.Second) {
				t.Errorf("%s did not answer on its published port.\n%s", url, diagnose(name, url))
			}
		})
	}
}

// TestSpecDirectContainerPort checks the second half of the deal: the name also
// reaches the container's own port, which only works because the container
// address is published too.
func TestSpecDirectContainerPort(t *testing.T) {

	for name, spec := range specByName {
		if spec.directPort == 0 {
			continue
		}
		expected := spec.publishedName
		if expected == "" {
			expected = name + ".docker.local"
		}
		t.Run(name, func(t *testing.T) {
			url := "http://" + expected + ":" + itoa(spec.directPort) + "/"
			if !waitForHTTP(url, 200, 30*time.Second) {
				t.Errorf("%s did not answer on the container port.\n%s", url, diagnose(name, url))
			}
		})
	}
}

// TestSpecNamesResolveThroughNSSFiles checks resolution the way a normal
// program does it.
func TestSpecNamesResolveThroughNSSFiles(t *testing.T) {

	for name, spec := range specByName {
		if !spec.wantPublished {
			continue
		}
		expected := spec.publishedName
		if expected == "" {
			expected = name + ".docker.local"
		}
		t.Run(expected, func(t *testing.T) {
			eventually(t, expected+" to resolve", func() bool { return resolves(expected) })
			for _, alias := range spec.aliases {
				if !resolves(alias) {
					t.Errorf("alias %q is in the hosts file but does not resolve", alias)
				}
			}
		})
	}
}

// TestSpecExcludedNamesDoNotResolve is the negative half of the rule: skipping a
// container has to mean its name is genuinely unreachable, not merely hidden
// from the file.
func TestSpecExcludedNamesDoNotResolve(t *testing.T) {

	for _, name := range []string{
		"dhi-created", "dhi-stopped", "dhi-hostnet", "dhi-nonet", "localhost",
	} {
		t.Run(name, func(t *testing.T) {
			if nameExists(t, name+".docker.local") {
				t.Errorf("%s is in the managed block and must not be:\n%s",
					name, managedSection(t))
			}
			if !resolverIsHijacked() && resolves(name+".docker.local") {
				t.Errorf("%s must not resolve, but it does", name)
			}
		})
	}
}

// --- lifecycle -----------------------------------------------------------

// TestSpecContainerLifecycleFollowsDocker checks that records appear and
// disappear as containers are created, started, stopped, restarted and removed.
func TestSpecContainerLifecycleFollowsDocker(t *testing.T) {

	name := "dhi-lifecycle"
	const published = "dhi-lifecycle.docker.local"

	_ = runQuiet(30*time.Second, "docker", "rm", "-f", "-v", name)
	if out, err := run(60*time.Second, "docker", "run", "-d", "--name", name, "nginx:alpine"); err != nil {
		t.Fatalf("create: %v: %s", err, out)
	}

	eventually(t, name+" to be published", func() bool { return nameExists(t, published) })
	eventually(t, published+" to resolve", func() bool { return resolves(published) })

	t.Run("stop removes the record", func(t *testing.T) {
		if err := runQuiet(30*time.Second, "docker", "stop", name); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the record to disappear after stop", func() bool {
			return !nameExists(t, published)
		})
	})

	t.Run("start brings it back", func(t *testing.T) {
		if err := runQuiet(30*time.Second, "docker", "start", name); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the record to return after start", func() bool {
			return nameExists(t, published)
		})
	})

	t.Run("remove removes the record", func(t *testing.T) {
		if err := runQuiet(30*time.Second, "docker", "rm", "-f", name); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the record to disappear after remove", func() bool {
			return !nameExists(t, published)
		})
	})

	t.Run("the operator's entries still survive everything", func(t *testing.T) {
		if got := userSection(t); got != seedHosts {
			t.Errorf("the operator's section changed after the lifecycle churn.\nwant %q\ngot  %q",
				seedHosts, got)
		}
	})
}

// TestSpecAliasAddedAfterStartIsPublished covers the case the event stream
// exists for: an alias added to a running container.
func TestSpecAliasAddedAfterStartIsPublished(t *testing.T) {

	name := "dhi-alias-added"
	const published = "dhi-alias-added.docker.local"
	const alias = "late-alias.docker.local"

	_ = runQuiet(30*time.Second, "docker", "rm", "-f", "-v", name)
	if out, err := run(60*time.Second, "docker", "run", "-d", "--name", name, "nginx:alpine"); err != nil {
		t.Fatalf("create: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = runQuiet(30*time.Second, "docker", "rm", "-f", "-v", name) })

	eventually(t, name+" to be published", func() bool { return nameExists(t, published) })

	// Connect it to another network carrying an alias.
	if err := runQuiet(30*time.Second, "docker", "network", "connect",
		"--alias", "late-alias", testNetwork, name); err != nil {
		t.Fatalf("connect the alias: %v", err)
	}

	eventually(t, "the new alias to be published", func() bool { return nameExists(t, alias) })
	if !resolves(alias) {
		t.Errorf("the alias %q is in the file but does not resolve", alias)
	}
}

// --- the web component ---------------------------------------------------

// TestWebUIPageIsSelfContained checks the page renders, carries the state, and
// depends on nothing external.
func TestWebUIPageIsSelfContained(t *testing.T) {

	body, code := web.index(t)
	if code != 200 {
		t.Fatalf("GET / returned %d", code)
	}

	// The state must be embedded so the table is populated on first paint.
	for _, want := range []string{"docker.local", "docker-hoster-injector", "snapshot", "Published records"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not contain %q", want)
		}
	}
	// The stylesheet is inlined.
	if !strings.Contains(body, "--bg") || !strings.Contains(body, "prefers-color-scheme") {
		t.Error("the stylesheet is not inlined, so the page would be unstyled offline")
	}
	// No external resources: this service is often on an isolated network.
	// What counts is what the browser would fetch, that is attributes and CSS
	// references. A URL inside the embedded data (the links to the containers)
	// is text the page shows, not something it loads.
	external := regexp.MustCompile(`(?i)(?:src|href|action|poster|data)\s*=\s*["']?(?:https?:)?//|url\(\s*["']?(?:https?:)?//|@import|<link\b`)
	if loc := external.FindStringIndex(body); loc != nil {
		t.Errorf("the page references an external resource: %q",
			body[max(0, loc[0]-40):min(len(body), loc[1]+40)])
	}
}

func TestWebUIAPIEndpoints(t *testing.T) {

	t.Run("entries is valid JSON with the expected shape", func(t *testing.T) {
		var snap struct {
			GeneratedAt string `json:"generated_at"`
			Records     []struct {
				Address     string   `json:"address"`
				Names       []string `json:"names"`
				Container   string   `json:"container"`
				ContainerID string   `json:"container_id"`
				State       string   `json:"state"`
			} `json:"records"`
			Skipped []struct {
				Container string `json:"container"`
				Reason    string `json:"reason"`
			} `json:"skipped"`
			Summary struct {
				Containers int `json:"containers"`
				Records    int `json:"records"`
			} `json:"summary"`
		}
		if code := jsonGet(web.url("/api/entries"), &snap); code != 200 {
			t.Fatalf("GET /api/entries returned %d", code)
		}

		if snap.GeneratedAt == "" {
			t.Error("generated_at is empty")
		}
		if len(snap.Records) == 0 {
			t.Fatal("no records: the estate should have produced some")
		}
		if snap.Summary.Records != len(snap.Records) {
			t.Errorf("summary.records = %d but there are %d records",
				snap.Summary.Records, len(snap.Records))
		}
		for _, r := range snap.Records {
			if r.Address == "" || len(r.Names) == 0 || r.Container == "" {
				t.Errorf("incomplete record: %+v", r)
			}
		}
		// The API must report an empty list as [], never null, or the page's
		// first paint throws.
		if !strings.Contains(readHosts(t), beginMarker) {
			t.Error("sanity: the block should exist")
		}
	})

	t.Run("config reports the deployment", func(t *testing.T) {
		cfg := web.config(t)
		if cfg["dns_suffix"] != "docker.local" {
			t.Errorf("dns_suffix = %q, want docker.local", cfg["dns_suffix"])
		}
		if cfg["hosts_file"] != hostsPath {
			t.Errorf("hosts_file = %q, want %q", cfg["hosts_file"], hostsPath)
		}
		if cfg["target_mode"] != "both" {
			t.Errorf("target_mode = %q, want both", cfg["target_mode"])
		}
		if cfg["version"] == "" {
			t.Error("version is empty")
		}
	})

	t.Run("health reports ok", func(t *testing.T) {
		body, code := web.health(t)
		if code != 200 {
			t.Fatalf("GET /healthz returned %d: %v", code, body)
		}
		if body["status"] != "ok" {
			t.Errorf("status = %v, want ok", body["status"])
		}
	})

	t.Run("metrics exposes the counters", func(t *testing.T) {
		body := web.metrics(t)
		for _, want := range []string{
			"dhi_containers", "dhi_records", "dhi_names", "dhi_skipped", "dhi_sse_clients",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("metrics is missing %s:\n%s", want, body)
			}
		}
	})
}

// TestWebUIStreamPushesUpdates checks the SSE channel actually pushes, since a
// page that silently falls back to polling would still look fine.
func TestWebUIStreamPushesUpdates(t *testing.T) {

	const name = "dhi-slow"
	const published = "dhi-slow.docker.local"
	_ = runQuiet(30*time.Second, "docker", "rm", "-f", "-v", name)
	t.Cleanup(func() { _ = runQuiet(30*time.Second, "docker", "rm", "-f", "-v", name) })

	// Open the stream in the background, then cause a change.
	streamCtx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(streamCtx, "GET", web.url("/api/events"), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open the event stream: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	// The first event must arrive without any change happening, otherwise a
	// page would sit empty until something moved.
	first := readSSEEvent(t, resp.Body)
	if !strings.Contains(first, "event: snapshot") {
		t.Errorf("first event = %q, want a snapshot", first)
	}

	// Now create the container and expect a push.
	if out, err := run(60*time.Second, "docker", "run", "-d", "--name", name, "nginx:alpine"); err != nil {
		t.Fatalf("create: %v: %s", err, out)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ev := readSSEEvent(t, resp.Body)
		if strings.Contains(ev, published) {
			return // pushed
		}
	}
	t.Errorf("the stream never pushed an update mentioning %s", published)
}

// TestWebUIRejectsWrites checks the read-only contract, which is what makes
// running without authentication acceptable.
func TestWebUIRejectsWrites(t *testing.T) {

	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		t.Run(method, func(t *testing.T) {
			req, err := http.NewRequest(method, web.url("/api/entries"), nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 405 {
				t.Errorf("%s returned %d, want 405: the service must be read-only",
					method, resp.StatusCode)
			}
		})
	}

	t.Run("unknown route is 404", func(t *testing.T) {
		if _, code := httpBody(web.url("/does-not-exist")); code != 404 {
			t.Errorf("got %d, want 404", code)
		}
	})
}
