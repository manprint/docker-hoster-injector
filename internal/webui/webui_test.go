package webui

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/manprint/docker-hoster-injector/internal/hostsfile"
	"github.com/manprint/docker-hoster-injector/internal/naming"
	"github.com/manprint/docker-hoster-injector/internal/reconcile"
)

func testConfig() Config {
	return Config{
		DNSSuffix:  "docker.local",
		HostsFile:  "/etc/hosts",
		TargetMode: "both",
		MountMode:  "file",
		Version:    "test",
		Source:     "testhost",
	}
}

// resultWith builds a reconcile result with two published containers and one
// skip, which is the smallest interesting shape.
func resultWith() reconcile.Result {
	return reconcile.Result{
		Entries: []reconcile.Entry{
			{
				Entry:       hostsfileEntry("127.0.0.1", "nginx.docker.local"),
				ContainerID: "c1", ContainerName: "nginx", State: "running",
			},
			{
				Entry:       hostsfileEntry("172.17.0.2", "nginx.docker.local"),
				ContainerID: "c1", ContainerName: "nginx", State: "running",
			},
			{
				Entry:       hostsfileEntry("172.18.0.3", "proj--web--1.docker.local", "proj_web_1.docker.local"),
				ContainerID: "c2", ContainerName: "proj_web_1", State: "running",
			},
		},
		Skipped: []reconcile.Skipped{
			{ContainerID: "c3", ContainerName: "hostapp", State: "running",
				Reason: reconcile.SkipNetworkHost},
		},
	}
}

func TestIndexRendersStateAndConfig(t *testing.T) {
	t.Parallel()

	s := New(testConfig(), nil)
	s.Publish(resultWith())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		"docker.local", "/etc/hosts", "both", "file", "test",
		"nginx.docker.local", "proj_web_1.docker.local", "hostapp",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not mention %q", want)
		}
	}
	// The stylesheet must be inlined: the service is often on an isolated
	// network where a linked file would not load.
	if !strings.Contains(body, "prefers-color-scheme") {
		t.Error("the stylesheet is not inlined")
	}
}

func TestIndexIsSelfContained(t *testing.T) {
	t.Parallel()

	s := New(testConfig(), nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	// No external references at all: no CDN, no fonts, no absolute URLs.
	for _, marker := range []string{"http://", "https://", "//cdn", "@import"} {
		if strings.Contains(rec.Body.String(), marker) {
			t.Errorf("the page references %q, which would fail on an isolated network", marker)
		}
	}
}

// An empty result must still be a valid document: a browser rendering [] must
// not throw.
func TestIndexWithNoContainers(t *testing.T) {
	t.Parallel()

	s := New(testConfig(), nil)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// The embedded snapshot must not contain a null array, or the page's
	// first render throws before the event stream arrives.
	body := rec.Body.String()
	start := strings.Index(body, "const SNAPSHOT = ")
	if start < 0 {
		t.Fatal("the page does not embed an initial snapshot")
	}
	snapshot := body[start+len("const SNAPSHOT = "):]
	snapshot = snapshot[:strings.IndexByte(snapshot, '\n')]
	if strings.Contains(snapshot, "null") {
		t.Errorf("the embedded snapshot contains null: %s", snapshot)
	}
}

func TestEntriesJSON(t *testing.T) {
	t.Parallel()

	s := New(testConfig(), nil)
	s.Publish(resultWith())

	req := httptest.NewRequest(http.MethodGet, "/api/entries", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var snap Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatalf("the response is not valid JSON: %v\n%s", err, rec.Body.String())
	}
	if len(snap.Records) != 3 {
		t.Errorf("got %d records, want 3", len(snap.Records))
	}
	if snap.Summary.Containers != 2 {
		t.Errorf("summary.containers = %d, want 2", snap.Summary.Containers)
	}
	if snap.Summary.Skipped != 1 {
		t.Errorf("summary.skipped = %d, want 1", snap.Summary.Skipped)
	}
	if len(snap.Skipped) != 1 || snap.Skipped[0].Container != "hostapp" {
		t.Errorf("skipped = %+v, want hostapp", snap.Skipped)
	}
	if snap.GeneratedAt.IsZero() {
		t.Error("generated_at is zero")
	}
}

// A JSON consumer must receive [] and never null, or the first thing it does
// on a fresh agent throws.
func TestEntriesJSONNeverNull(t *testing.T) {
	t.Parallel()

	s := New(testConfig(), nil) // nothing published

	req := httptest.NewRequest(http.MethodGet, "/api/entries", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, `"records": null`) || strings.Contains(body, `"skipped": null`) {
		t.Errorf("the API returns null for an empty list:\n%s", body)
	}
	if !strings.Contains(body, `"records": []`) {
		t.Errorf("expected an empty array:\n%s", body)
	}
}

func TestEntriesIsStableForTheSameState(t *testing.T) {
	t.Parallel()

	// The output must be byte-identical for identical state, or the page
	// would flicker and any client diffing snapshots would see noise.
	s := New(testConfig(), nil)
	s.Publish(resultWith())

	get := func() string {
		req := httptest.NewRequest(http.MethodGet, "/api/entries", nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		// The timestamp legitimately changes; everything else must not.
		return scrub(rec.Body.String())
	}

	first := get()
	for i := 0; i < 10; i++ {
		if got := get(); got != first {
			t.Fatalf("iteration %d differs:\n%s\n---\n%s", i, first, got)
		}
	}
}

// scrub removes the only field that is expected to change between snapshots.
func scrub(s string) string {
	start := strings.Index(s, `"generated_at"`)
	if start < 0 {
		return s
	}
	end := strings.Index(s[start:], ",")
	if end < 0 {
		return s
	}
	return s[:start] + `"generated_at": "X"` + s[start+end:]
}

func TestConfigEndpoint(t *testing.T) {
	t.Parallel()

	s := New(testConfig(), nil)
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["dns_suffix"] != "docker.local" {
		t.Errorf("dns_suffix = %q", got["dns_suffix"])
	}
	if got["version"] != "test" {
		t.Errorf("version = %q", got["version"])
	}
}

func TestHealth(t *testing.T) {
	t.Parallel()

	s := New(testConfig(), nil)

	t.Run("healthy", func(t *testing.T) {
		s.SetStatsProvider(func() (uint64, time.Time, error) {
			return 3, time.Now(), nil
		})
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
			t.Errorf("body = %s, want status ok", rec.Body.String())
		}
	})

	// A degraded agent must be visible to a health check, otherwise an
	// operator has to read the logs to notice a broken hosts file.
	t.Run("degraded on a failed apply", func(t *testing.T) {
		s.SetStatsProvider(func() (uint64, time.Time, error) {
			return 1, time.Time{}, errTest
		})
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)

		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "degraded") {
			t.Errorf("body = %s, want degraded", rec.Body.String())
		}
	})
}

var errTest = &testError{}

type testError struct{}

func (*testError) Error() string { return "boom" }

func TestMetrics(t *testing.T) {
	t.Parallel()

	s := New(testConfig(), nil)
	s.Publish(resultWith())
	s.SetStatsProvider(func() (uint64, time.Time, error) {
		return 5, time.Unix(1700000000, 0), nil
	})

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, want := range []string{
		"dhi_containers 2", "dhi_records 3", "dhi_skipped 1",
		"dhi_hosts_writes_total 5", "dhi_last_apply_failed 0",
		"dhi_last_apply_timestamp_seconds 1700000000",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}
	// The fail gauge must flip, it is the whole point of the endpoint.
	s.SetStatsProvider(func() (uint64, time.Time, error) {
		return 5, time.Time{}, errTest
	})
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "dhi_last_apply_failed 1") {
		t.Errorf("the failure gauge did not flip:\n%s", rec.Body.String())
	}
}

// The service has no authentication, which is only defensible while it is
// incapable of changing anything.
func TestReadOnly(t *testing.T) {
	t.Parallel()

	s := New(testConfig(), nil)
	h := s.Handler()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			for _, path := range []string{"/", "/api/entries", "/api/config", "/metrics"} {
				req := httptest.NewRequest(method, path, nil)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusMethodNotAllowed {
					t.Errorf("%s %s = %d, want 405", method, path, rec.Code)
				}
			}
		})
	}
}

func TestUnknownRoute(t *testing.T) {
	t.Parallel()

	s := New(testConfig(), nil)
	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// The first event must carry the current state, or a page would sit empty
// until something changed.
func TestEventsStreamSendsInitialSnapshot(t *testing.T) {
	t.Parallel()

	s := New(testConfig(), nil)
	s.Publish(resultWith())

	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := contextWithTimeout(5 * time.Second)
	defer cancel()

	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}

	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	got := string(buf[:n])

	if !strings.Contains(got, "event: snapshot") {
		t.Errorf("first message = %q, want a snapshot event", got)
	}
	if !strings.Contains(got, "nginx.docker.local") {
		t.Errorf("the first snapshot does not carry the current state: %q", got)
	}
}

func TestEventsStreamPushesUpdates(t *testing.T) {
	t.Parallel()

	s := New(testConfig(), nil)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	ctx, cancel := contextWithTimeout(10 * time.Second)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Drain the first event, then publish and expect a push.
	buf := make([]byte, 4096)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		s.Publish(resultWith())
	}()

	// The slice must keep its full length: Read into a zero-length slice
	// returns immediately without consuming anything.
	for i := 0; i < 5; i++ {
		n, err := resp.Body.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(buf[:n]), "nginx.docker.local") {
			return
		}
	}
	t.Error("the stream never pushed an update")
}

// A publishing client that is not reading must never block the agent.
func TestPublishDoesNotBlockOnASlowClient(t *testing.T) {
	t.Parallel()

	s := New(testConfig(), nil)
	updates, unsubscribe := s.hub.subscribe()
	defer unsubscribe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			s.Publish(resultWith())
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("publishing blocked on a client that never reads")
	}
	_ = updates
}

func TestHubUnsubscribeIsIdempotent(t *testing.T) {
	t.Parallel()

	h := newHub()
	_, unsub := h.subscribe()
	unsub()
	unsub() // must not panic on a closed channel

	if got := h.count(); got != 0 {
		t.Errorf("count = %d after unsubscribing, want 0", got)
	}
}

func TestSnapshotGroupsByContainer(t *testing.T) {
	t.Parallel()

	s := New(testConfig(), nil)
	snap := s.buildSnapshot(resultWith())

	// Two addresses for one container must share the same name list, so the
	// page agrees with the file about what resolves where.
	byContainer := map[string][]Record{}
	for _, r := range snap.Records {
		byContainer[r.ContainerID] = append(byContainer[r.ContainerID], r)
	}

	if len(byContainer["c1"]) != 2 {
		t.Errorf("nginx has %d records, want 2", len(byContainer["c1"]))
	}
	if len(byContainer["c2"]) != 1 {
		t.Errorf("proj_web_1 has %d records, want 1", len(byContainer["c2"]))
	}

	// The names must be sorted so the table does not reshuffle.
	got := byContainer["c2"][0].Names
	want := []string{"proj--web--1.docker.local", "proj_web_1.docker.local"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("names = %q, want %q", got, want)
	}
}

// hostsfileEntry builds an entry without importing the package everywhere.
func hostsfileEntry(addr string, names ...string) hostsfile.Entry {
	return hostsfile.Entry{IP: net.ParseIP(addr), Names: names}
}

// contextWithTimeout keeps the imports of the stream tests tidy.
func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// A container with two addresses lists the same names against each of them, but
// those names are one set, not two.
func TestSummaryCountsNamesOncePerContainer(t *testing.T) {
	t.Parallel()

	s := New(testConfig(), nil)
	snap := s.buildSnapshot(resultWith())

	// nginx: 1 name on 2 addresses; proj_web_1: 2 names on 1 address.
	if snap.Summary.Names != 3 {
		t.Errorf("summary.names = %d, want 3", snap.Summary.Names)
	}
	if snap.Summary.Records != 3 {
		t.Errorf("summary.records = %d, want 3", snap.Summary.Records)
	}
}

// Rows follow the order of the hosts file: the resolver tries a name's
// addresses in file order, so the page must not re-sort them.
func TestSnapshotKeepsTheAddressOrder(t *testing.T) {
	t.Parallel()

	res := reconcile.Result{Entries: []reconcile.Entry{
		{Entry: hostsfileEntry("127.0.0.1", "a.docker.local"), ContainerID: "c1", ContainerName: "a", State: "running"},
		{Entry: hostsfileEntry("10.0.0.5", "a.docker.local"), ContainerID: "c1", ContainerName: "a", State: "running"},
	}}
	snap := New(testConfig(), nil).buildSnapshot(res)

	if len(snap.Records) != 2 || snap.Records[0].Address != "127.0.0.1" || snap.Records[1].Address != "10.0.0.5" {
		t.Errorf("records = %+v, want 127.0.0.1 before 10.0.0.5", snap.Records)
	}
}

// A container that lost every one of its names is in neither the records nor
// the skipped list, so the conflict row is the only place it can be seen. The
// counter must match the rows.
func TestConflictsAreListedAndCounted(t *testing.T) {
	t.Parallel()

	res := resultWith()
	res.Conflicts = []naming.Conflict{{
		Name:   "nginx.docker.local",
		Winner: naming.Claim{Name: "nginx.docker.local", Owner: "c1"},
		Losers: []naming.Claim{{Name: "nginx.docker.local", Owner: "c9"}},
	}}
	snap := New(testConfig(), nil).buildSnapshot(res)

	if snap.Summary.Skipped != len(snap.Skipped) {
		t.Errorf("summary.skipped = %d but %d rows are listed", snap.Summary.Skipped, len(snap.Skipped))
	}
	var found *SkippedContainer
	for i := range snap.Skipped {
		if snap.Skipped[i].State == "conflict" {
			found = &snap.Skipped[i]
		}
	}
	if found == nil {
		t.Fatalf("no conflict row in %+v", snap.Skipped)
	}
	if !strings.Contains(found.Reason, "2 containers") || !strings.Contains(found.Reason, "nginx") {
		t.Errorf("reason = %q, want it to name the contest and who kept the name", found.Reason)
	}
}

func TestDedupeSortedReallySorts(t *testing.T) {
	t.Parallel()

	got := dedupeSorted([]string{"b", "a", "b", "c", "a"})
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("dedupeSorted = %v, want [a b c]", got)
	}
}

// The counter exists from the start, at zero, so that rate() and absence
// alerts behave.
func TestMetricsExposeTheWriteCounterAtZero(t *testing.T) {
	t.Parallel()

	s := New(testConfig(), nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if !strings.Contains(rec.Body.String(), "dhi_hosts_writes_total 0") {
		t.Errorf("metrics lack the counter at zero:\n%s", rec.Body.String())
	}
}

func TestResponsesCarrySecurityHeaders(t *testing.T) {
	t.Parallel()

	s := New(testConfig(), nil)
	for _, path := range []string{"/", "/api/entries", "/healthz", "/nope"} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: Content-Security-Policy = %q", path, csp)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: missing X-Content-Type-Options", path)
		}
	}
}

// WEB_EVENTS=false removes the push channel and tells the page to poll.
func TestDisableEventsRemovesTheStreamAndSwitchesThePageToPolling(t *testing.T) {
	t.Parallel()

	cfg := testConfig()
	cfg.DisableEvents = true
	s := New(cfg, nil)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/events", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("/api/events status = %d, want 404 when events are disabled", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(rec.Body.String(), "const EVENTS = false;") {
		t.Error("the page was not told to poll")
	}

	rec = httptest.NewRecorder()
	New(testConfig(), nil).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(rec.Body.String(), "const EVENTS = true;") {
		t.Error("the page was not told to use the event stream")
	}
}

// Stopping the agent must not wait out the grace period because a browser has
// the page open: the event streams are ended, not abandoned.
func TestServeStopsPromptlyWithAnOpenEventStream(t *testing.T) {
	t.Parallel()

	s := New(testConfig(), nil)
	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v", err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("shutdown took %v with one open stream", d)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Serve did not return after the context was cancelled")
	}
}

func TestListenReportsATakenPortImmediately(t *testing.T) {
	t.Parallel()

	ln, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	if _, err := Listen(ln.Addr().String()); err == nil {
		t.Error("binding an address that is taken did not fail")
	}
}
