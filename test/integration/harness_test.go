//go:build integration

// Package integration holds the acceptance tests that run against a real
// Docker daemon and a real hosts file.
//
// They are written as Go tests behind the "integration" build tag rather than
// as a shell script, so that `make test-integration` and the CI matrix pick
// them up, so failures point at a Go stack instead of a line of shell, and so
// the same assertions cover Docker 25 through 29.
//
// The suite provisions a realistic estate of about a dozen containers covering
// every case that can change what gets published, then verifies the behaviour a
// user actually cares about: does the name resolve, does the right thing answer
// on the right port, does the operator's own configuration survive, and does
// the service come back after being killed.
package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Environment contract.
//
// The suite manages the real /etc/hosts. That is not laziness, it is the only
// way to test the product: resolution happens through nss-files, which reads
// that file, so asserting against a sandbox copy would prove nothing at all.
//
// The original content is backed up before the run and restored afterwards, and
// the suite refuses to start without a backup, so an interrupted run is still
// recoverable by hand from /tmp.
var (
	hostsPath = envOr("E2E_HOSTS_FILE", "/etc/hosts")
	// hostsBackup is where the operator's file is kept while the suite runs.
	hostsBackup = "/tmp/docker-hoster-injector.hosts.backup"
	// hostsRestored guards against a panic path restoring twice.
	hostsRestored bool

	agentBin   = envOr("E2E_AGENT_BIN", "./bin/docker-hoster-injector")
	dockerHost = envOr("DOCKER_HOST", "unix:///var/run/docker.sock")

	// webAddr is resolved at startup to a free port rather than fixed: a
	// leftover agent from an earlier run would otherwise answer /healthz and
	// the suite would silently test that process instead of its own.
	webAddr string
)

// hostsWritable reports whether the suite may manage the real hosts file.
//
// A container usually cannot, because /etc/hosts is a mount point owned by
// Docker. In that case the file level assertions still work and are worth
// running, but the resolution and HTTP assertions are skipped and say why,
// rather than failing for a reason that has nothing to do with the code.
func hostsWritable() bool {
	dir := filepathJoin(hostsPath[:strings.LastIndex(hostsPath, "/")])
	probe := filepathJoin(dir, ".dhi-write-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		return false
	}
	_ = os.Remove(probe)
	return true
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// waitTimeout bounds how long the agent is given to converge after a change.
// It is generous because a CI runner is slow and the point of these tests is
// reliability, not speed.
const waitTimeout = 60 * time.Second

// Container ports the provisioned servers listen on. They must match what the
// images actually serve, since a mismatch would look like a publishing bug.
const (
	portNginx  = 80
	portPython = 8000
	portBusy   = 8080
)

// pollInterval is how often the convergence checks re-read the state.
const pollInterval = 150 * time.Millisecond

// Markers delimiting the managed block, duplicated here on purpose: the
// acceptance tests must verify the real file format independently of the
// package that writes it, otherwise a change to both would agree with itself.
const (
	beginMarker = "# BEGIN docker-hoster-injector (managed, do not edit)"
	endMarker   = "# END docker-hoster-injector"
)

// seedHosts is a realistic operator file. Its bytes must survive every test,
// which is what makes it a useful control.
const seedHosts = "127.0.0.1\tlocalhost\n" +
	"127.0.1.1\tmy-workstation\n" +
	"::1     ip6-localhost ip6-loopback\n" +
	"fe00::0 ip6-localnet\n" +
	"ff02::1 ip6-allnodes\n" +
	"ff02::2 ip6-allrouters\n" +
	"192.168.1.10   nas.home\n" +
	"192.168.1.11   printer.lan   # a comment of mine\n" +
	"10.0.0.5       build-server\n"

// The estate and the agent are built once in TestMain rather than inside a
// test. Sharing mutable state between tests would otherwise require a
// dependency order that Go does not offer, and parallel tests would race the
// setup they need.
func TestMain(m *testing.M) {
	code := 1
	_ = code

	// The crash suite re-executes this binary as a helper child to get a
	// process it can SIGKILL. That child must not provision an estate of its
	// own, so the setup is skipped for it.
	if os.Getenv("GO_WANT_HELPER_PROCESS") == "1" {
		os.Exit(m.Run())
	}

	if err := setup(); err != nil {
		fmt.Fprintf(os.Stderr, "integration setup failed: %v\n", err)
		teardown()
		os.Exit(1)
	}

	for _, s := range buildEstate() {
		specByName[s.name] = s
	}
	web = webUI{base: webAddr}
	agent = startAgent(agentLogPath())

	code = m.Run()

	agent.stop()
	// A graceful exit hands the real hosts file back to the operator: nothing
	// of the agent may remain in it, and every other byte must be the seed.
	if got, err := os.ReadFile(hostsPath); err != nil || string(got) != seedHosts {
		fmt.Fprintf(os.Stderr, "FAIL: %s after a graceful stop is not the operator's file (err=%v):\n%s\n",
			hostsPath, err, got)
		code = 1
	}
	teardown()
	os.Exit(code)
}

// agentLogPath is where the agent's output is captured for assertions.
func agentLogPath() string { return "/tmp/docker-hoster-injector.e2e.log" }

func setup() error {
	webAddr = envOr("E2E_WEB_ADDR", "127.0.0.1:"+strconv.Itoa(freePort()))

	if err := locateAgentBinary(); err != nil {
		return err
	}

	original, err := os.ReadFile(hostsPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", hostsPath, err)
	}
	if err := os.WriteFile(hostsBackup, original, 0o644); err != nil {
		return fmt.Errorf("back up %s to %s: %w", hostsPath, hostsBackup, err)
	}
	if !hostsWritable() {
		return fmt.Errorf("%s is not writable; run the suite with sudo so that name "+
			"resolution can actually be verified", hostsPath)
	}
	// Start from a controlled file so the assertions about preservation have a
	// known starting point.
	if err := os.WriteFile(hostsPath, []byte(seedHosts), 0o644); err != nil {
		return fmt.Errorf("seed %s: %w", hostsPath, err)
	}

	if _, err := dockerVersion(); err != nil {
		return err
	}
	pullImage("nginx:alpine")
	pullImage("python:3-alpine")
	pullImage("busybox:latest")
	return nil
}

func teardown() {
	// The agent is killed before anything else. A suite that dies mid-run
	// would otherwise leave an agent holding /etc/hosts and a listening socket,
	// which makes the next run flaky in ways that are very hard to diagnose.
	if agent != nil {
		_ = agent.killNow()
	}
	for _, c := range estateNames() {
		_ = runQuiet(30*time.Second, "docker", "rm", "-f", "-v", c)
	}
	removeNetwork(testNetwork)

	// Restoring the operator's hosts file is the last thing, and it happens
	// exactly once even if teardown is reached twice.
	if hostsRestored {
		return
	}
	hostsRestored = true
	if data, err := os.ReadFile(hostsBackup); err == nil {
		if err := os.WriteFile(hostsPath, data, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "CRITICAL: could not restore %s from %s: %v\n",
				hostsPath, hostsBackup, err)
			return
		}
		fmt.Fprintf(os.Stderr, "restored %s from %s\n", hostsPath, hostsBackup)
	} else {
		fmt.Fprintf(os.Stderr, "CRITICAL: no backup at %s, %s was left modified\n",
			hostsBackup, hostsPath)
	}
}

func filepathJoin(parts ...string) string {
	return strings.Join(parts, "/")
}

// --- hosts file inspection ----------------------------------------------

func readHosts(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(hostsPath)
	if err != nil {
		t.Fatalf("read the hosts file: %v", err)
	}
	return string(data)
}

// userSection returns the file with the managed block and its explanatory
// comment removed, which must equal the seed byte for byte.
func userSection(t *testing.T) string {
	t.Helper()
	var out []string
	inBlock := false
	for _, line := range strings.Split(readHosts(t), "\n") {
		switch strings.TrimSpace(line) {
		case beginMarker:
			inBlock = true
			continue
		case endMarker:
			inBlock = false
			continue
		}
		if inBlock {
			continue
		}
		out = append(out, line)
	}
	// The renderer writes the operator's lines exactly as read and appends the
	// block after them, so what is left must be the seed byte for byte.
	return strings.Join(out, "\n")
}

// record is one parsed line of the managed block.
type record struct {
	Addr  string
	Names []string
}

// records parses the managed block.
func records(t *testing.T) []record {
	t.Helper()
	var out []record
	inBlock := false
	for _, line := range strings.Split(readHosts(t), "\n") {
		switch strings.TrimSpace(line) {
		case beginMarker:
			inBlock = true
			continue
		case endMarker:
			inBlock = false
			continue
		}
		if !inBlock {
			continue
		}
		body := line
		if i := strings.IndexByte(body, '#'); i >= 0 {
			body = body[:i]
		}
		fields := strings.Fields(body)
		if len(fields) < 2 {
			if strings.TrimSpace(body) == "" {
				continue
			}
			t.Errorf("malformed line in the managed block: %q", line)
			continue
		}
		out = append(out, record{Addr: fields[0], Names: fields[1:]})
	}
	return out
}

// addressesForName returns the addresses a name is published under, sorted.
func addressesForName(t *testing.T, name string) []string {
	t.Helper()
	var out []string
	for _, r := range records(t) {
		for _, n := range r.Names {
			if n == name {
				out = append(out, r.Addr)
			}
		}
	}
	sort.Strings(out)
	return out
}

func nameExists(t *testing.T, name string) bool {
	t.Helper()
	return len(addressesForName(t, name)) > 0
}

// allPublishedNames returns every name in the managed block.
func allPublishedNames(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, r := range records(t) {
		out = append(out, r.Names...)
	}
	sort.Strings(out)
	return out
}

// --- resolution and HTTP -------------------------------------------------

// resolve returns the addresses a name resolves to through nss-files.
//
// getent ahosts is used rather than getent hosts because the latter falls
// through to DNS when the name is absent, and an upstream resolver that
// hijacks NXDOMAIN into 127.0.0.1 would then report a false positive.
func resolve(name string) ([]string, error) {
	out, err := run(10*time.Second, "getent", "ahosts", name)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	var addrs []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if _, dup := seen[fields[0]]; dup {
			continue
		}
		seen[fields[0]] = struct{}{}
		addrs = append(addrs, fields[0])
	}
	sort.Strings(addrs)
	return addrs, nil
}

func resolves(name string) bool {
	addrs, err := resolve(name)
	return err == nil && len(addrs) > 0
}

func doesNotResolve(name string) bool { return !resolves(name) }

// resolverHijacked reports whether the host's DNS server answers unknown names
// with an address instead of NXDOMAIN.
//
// Several ISPs do exactly that, to catch typos. When it is happening, "the name
// still resolves" proves nothing: the answer comes from DNS, not from the hosts
// file the agent manages. The tests detect this and fall back to inspecting the
// file, which is the authoritative signal anyway.
var hijackChecked bool
var hijacked bool

func resolverIsHijacked() bool {
	if !hijackChecked {
		hijackChecked = true
		// A name that cannot exist, checked against a domain the agent never
		// publishes.
		probe := "dhi-nonexistent-probe-" + strconv.Itoa(os.Getpid()) + ".invalid"
		_, err := resolve(probe)
		hijacked = err == nil
		if hijacked {
			fmt.Fprintln(os.Stderr,
				"note: this host's DNS answers unknown names with an address; "+
					"resolution is not used as a negative signal, the hosts file is")
		}
	}
	return hijacked
}

// httpGet returns the status code for a URL, or 0 when the request fails.
func httpGet(rawURL string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// httpBody returns the response body for a URL.
func httpBody(rawURL string) (string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", 0
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", resp.StatusCode
	}
	return string(raw), resp.StatusCode
}

func jsonGet(rawURL string, v any) int {
	body, code := httpBody(rawURL)
	if code != http.StatusOK {
		return code
	}
	if err := json.Unmarshal([]byte(body), v); err != nil {
		return 0
	}
	return code
}

// waitForHTTP reports whether a URL returns the wanted status within d.
func waitForHTTP(url string, want int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if httpGet(url) == want {
			return true
		}
		time.Sleep(pollInterval)
	}
	return false
}

// diagnose explains a failed request, so a failure is actionable without
// re-running anything.
func diagnose(containerName, url string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  url:        %s\n", url)
	fmt.Fprintf(&b, "  status:     %d\n", httpGet(url))
	fmt.Fprintf(&b, "  resolves:   %v\n", resolves(hostOf(url)))

	addrs, err := resolve(hostOf(url))
	fmt.Fprintf(&b, "  nss:        %v (err=%v)\n", addrs, err)

	if out, err := run(10*time.Second, "docker", "inspect",
		"-f", "{{.State.Status}} {{.State.ExitCode}} {{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}",
		containerName); err == nil {
		fmt.Fprintf(&b, "  container:  %s\n", strings.TrimSpace(out))
	} else {
		fmt.Fprintf(&b, "  container:  <not found: %v>\n", err)
	}
	if out, err := run(10*time.Second, "docker", "logs", "--tail", "5", containerName); err == nil {
		fmt.Fprintf(&b, "  logs:\n    %s\n", strings.TrimSpace(strings.ReplaceAll(out, "\n", "\n    ")))
	}
	return b.String()
}

// hostOf extracts the host from a URL.
func hostOf(rawURL string) string {
	rest := strings.TrimPrefix(rawURL, "http://")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	if h, _, err := net.SplitHostPort(rest); err == nil {
		return h
	}
	return rest
}

// --- convergence ---------------------------------------------------------

// eventually polls cond until it holds or the timeout expires. The last failure
// description is reported so the message explains what stayed wrong.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	var last string
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(pollInterval)
	}
	if last != "" {
		t.Fatalf("timed out after %s waiting for %s: %s", waitTimeout, what, last)
	}
	t.Fatalf("timed out after %s waiting for %s", waitTimeout, what)
}

// consistently checks that cond keeps holding for the given duration, used to
// prove that a steady state does not drift or rewrite the file.
func consistently(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !cond() {
			t.Fatalf("%s stopped holding within %s", what, d)
		}
		time.Sleep(pollInterval)
	}
}

// --- docker helpers ------------------------------------------------------

func dockerVersion() (string, error) {
	out, err := run(30*time.Second, "docker", "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return "", fmt.Errorf("no usable Docker daemon (DOCKER_HOST=%s): %w: %s",
			dockerHost, err, strings.TrimSpace(out))
	}
	return strings.TrimSpace(out), nil
}

func run(timeout time.Duration, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "DOCKER_HOST="+dockerHost)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func runQuiet(timeout time.Duration, name string, args ...string) error {
	_, err := run(timeout, name, args...)
	return err
}

func pullImage(image string) {
	if _, err := run(10*time.Second, "docker", "image", "inspect", image); err == nil {
		return
	}
	if err := runQuiet(180*time.Second, "docker", "pull", image); err != nil {
		fmt.Fprintf(os.Stderr, "note: could not pull %s; related checks will be skipped\n", image)
	}
}

func imageAvailable(image string) bool {
	_, err := run(10*time.Second, "docker", "image", "inspect", image)
	return err == nil
}

func containerRunning(name string) bool {
	out, err := run(10*time.Second, "docker", "inspect", "-f", "{{.State.Running}}", name)
	return err == nil && strings.TrimSpace(out) == "true"
}

func containerState(name string) string {
	out, err := run(10*time.Second, "docker", "inspect", "-f", "{{.State.Status}}", name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// freePort reserves a port on the loopback interface so the estate does not
// collide with whatever the developer is already running.
// freePort reserves a port on the loopback interface so the estate does not
// collide with whatever the developer is already running.
//
// The socket is closed before returning, so two calls can hand out the same
// number. Callers must therefore treat each result as a distinct request and
// re-check for a collision, which the estate builder does by using one port per
// container and verifying the estate actually starts.
func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(fmt.Sprintf("reserve a port: %v", err))
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// uniquePorts returns n distinct ports. Relying on freePort alone is not
// enough: the kernel is free to hand the same number twice in a row, and a
// duplicate would make the second container fail to bind for a reason that has
// nothing to do with the code under test.
func uniquePorts(n int) []int {
	seen := map[int]struct{}{}
	out := make([]int, 0, n)
	for len(out) < n {
		p := freePort()
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

func itoa(i int) string { return strconv.Itoa(i) }

// hostAddresses returns the host's own addresses, used to assert that a
// host-network container is never mapped onto one of them.
func hostAddresses() []string {
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			switch v := a.(type) {
			case *net.IPNet:
				out = append(out, v.IP.String())
			case *net.IPAddr:
				out = append(out, v.IP.String())
			}
		}
	}
	return out
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// managedSection returns the body of the managed block, without its markers.
func managedSection(t *testing.T) string {
	t.Helper()
	var out []string
	in := false
	for _, line := range strings.Split(readHosts(t), "\n") {
		switch strings.TrimSpace(line) {
		case beginMarker:
			in = true
			continue
		case endMarker:
			return strings.Join(out, "\n")
		}
		if in {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// recordsSkipped returns the containers the agent reports as not published.
func recordsSkipped(t *testing.T) []webRecord {
	t.Helper()
	var out []webRecord
	in := false
	for _, line := range strings.Split(readHosts(t), "\n") {
		_ = line
		_ = in
		break
	}
	// The skip list is not in the hosts file: it is reported by the web API,
	// which is the only place it exists.
	var snap struct {
		Skipped []webRecord `json:"skipped"`
	}
	if code := jsonGet("http://"+webAddr+"/api/entries", &snap); code != 200 {
		t.Fatalf("GET /api/entries returned %d", code)
	}
	out = append(out, snap.Skipped...)
	return out
}

// webRecord mirrors the skipped-container shape of the API.
type webRecord struct {
	Container string `json:"container"`
	Reason    string `json:"reason"`
	State     string `json:"state"`
}

// readSSEEvent reads one Server-Sent Event, blocking until it arrives or the
// caller's context ends.
func readSSEEvent(t *testing.T, body io.Reader) string {
	t.Helper()

	type result struct {
		s   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var sb strings.Builder
		br := bufio.NewReader(body)
		for {
			line, err := br.ReadString('\n')
			sb.WriteString(line)
			if err != nil {
				ch <- result{sb.String(), err}
				return
			}
			// An event ends with a blank line.
			if line == "\n" && sb.Len() > 0 {
				ch <- result{sb.String(), nil}
				return
			}
		}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("read the event stream: %v (partial: %q)", r.err, r.s)
		}
		return r.s
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for an event")
		return ""
	}
}

// locateAgentBinary resolves the agent binary.
//
// Go runs a test with the package directory as the working directory, so a path
// relative to the repository root does not resolve. E2E_AGENT_BIN overrides the
// search; otherwise the repository root is found by walking up to go.mod, and
// as a last resort the file is looked for relative to the package.
func locateAgentBinary() error {
	if agentBin != "./bin/docker-hoster-injector" && agentBin != "" {
		if _, err := os.Stat(agentBin); err != nil {
			return fmt.Errorf("E2E_AGENT_BIN=%s does not exist", agentBin)
		}
		return nil
	}

	root, err := repoRoot()
	if err != nil {
		return err
	}
	candidate := filepathJoin(root, "bin", "docker-hoster-injector")
	if _, err := os.Stat(candidate); err != nil {
		return fmt.Errorf("the agent binary is missing at %s: run 'make build' first", candidate)
	}
	agentBin = candidate
	return nil
}

// repoRoot walks up from the working directory until it finds go.mod.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}
	for {
		if _, err := os.Stat(filepathJoin(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := dir
		for len(parent) > 1 && parent[len(parent)-1] == '/' {
			parent = parent[:len(parent)-1]
		}
		next := parent[:strings.LastIndex(parent, "/")+1]
		if next == "" || next == dir {
			return "", fmt.Errorf("could not find go.mod above the working directory; set E2E_AGENT_BIN")
		}
		dir = strings.TrimSuffix(next, "/")
	}
}
