//go:build integration

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The estate deliberately mixes every case that can change what gets
// published. Each entry states the expectation it encodes, so a failure names
// the rule that broke rather than just a name that is missing.
type estateSpec struct {
	name    string
	image   string
	ports   []string // -p arguments
	network string
	extra   []string
	host    bool // --network host
	none    bool // --network none
	noStart bool // create but do not run
	// cmd overrides the image's default command, which is how a server
	// listening on a non-standard port is provisioned.
	cmd []string

	// wantPublished is false when the container must NOT get a record.
	wantPublished bool
	// publishedName overrides the name the record is expected under.
	publishedName string
	// aliases are extra names expected in the block.
	aliases []string
	// publishedPort is a host port that must answer HTTP 200.
	publishedPort int
	// directPort is a container port that must answer HTTP 200, proving the
	// container address is published too.
	directPort int
}

const testNetwork = "dhi-e2e"

// estateNames is every container the suite may create, so teardown can remove
// them even when a test failed halfway.
func estateNames() []string {
	names := []string{
		"dhi-nginx", "dhi-nginx-hostport", "dhi-nginx-multi",
		"dhi-compose_web_1", "dhi-alias-svc",
		"dhi-noalias", "dhi-noports", "dhi-stopped", "dhi-created",
		"dhi-hostnet", "dhi-nonet", "dhi-weird_name", "localhost",
		"dhi-python", "dhi-busy", "dhi-lifecycle", "dhi-alias-added",
		// Legacy names from earlier revisions, so a stale container from an
		// interrupted run cannot pollute the estate.
		"dhi-slow", "dhi-alias-probe",
	}
	return names
}

// buildEstate provisions the containers.
//
// It takes no *testing.T because it runs from TestMain, before any test
// exists. A failure aborts the whole suite with a message, which is the right
// outcome: without the estate there is nothing to test.
func buildEstate() []estateSpec {

	// Every container gets its own ports. Distinct values are guaranteed
	// because the kernel may legitimately hand out the same free port twice,
	// and a duplicate would make the second container fail to bind for a
	// reason unrelated to the code under test.
	p := uniquePorts(7)
	nginxPort, hostPortPort, multiPortA := p[0], p[1], p[2]
	multiPortB, composePort, pyPort := p[3], p[4], p[5]
	// busyPort is the host side of the busybox mapping; the container listens
	// on portBusy, which is a fixed port of the image.
	busyPort := p[6]

	specs := []estateSpec{
		{
			// The canonical case from the specification.
			name: "dhi-nginx", image: "nginx:alpine",
			ports:         []string{fmt.Sprintf("%d:%d", nginxPort, portNginx)},
			publishedName: "dhi-nginx.docker.local",
			publishedPort: nginxPort, directPort: portNginx,
			wantPublished: true,
		},
		{
			// Published only on loopback: the name must resolve to 127.0.0.1
			// and nothing else host-side.
			name: "dhi-nginx-hostport", image: "nginx:alpine",
			ports:         []string{fmt.Sprintf("127.0.0.1:%d:%d", hostPortPort, portNginx)},
			publishedName: "dhi-nginx-hostport.docker.local",
			publishedPort: hostPortPort, directPort: portNginx,
			wantPublished: true,
		},
		{
			// Two published ports: both must answer under the same name.
			name: "dhi-nginx-multi", image: "nginx:alpine",
			ports: []string{
				fmt.Sprintf("%d:%d", multiPortA, portNginx),
				fmt.Sprintf("%d:%d", multiPortB, portNginx),
			},
			publishedName: "dhi-nginx-multi.docker.local",
			publishedPort: multiPortA, directPort: portNginx,
			wantPublished: true,
		},
		{
			// A Compose-shaped name with underscores: both the raw and the
			// sanitised form must be published.
			name: "dhi-compose_web_1", image: "nginx:alpine",
			network:       testNetwork,
			ports:         []string{fmt.Sprintf("%d:%d", composePort, portNginx)},
			publishedName: "dhi-compose_web_1.docker.local",
			aliases:       []string{"dhi-compose--web--1.docker.local"},
			publishedPort: composePort, directPort: portNginx,
			wantPublished: true,
		},
		{
			// A network alias, which is how a Compose service name reaches a
			// container.
			name: "dhi-alias-svc", image: "nginx:alpine",
			network:       testNetwork,
			extra:         []string{"--network-alias", "svc", "--network-alias", "frontend"},
			publishedName: "dhi-alias-svc.docker.local",
			aliases:       []string{"svc.docker.local", "frontend.docker.local"},
			wantPublished: true,
		},
		{
			// No published ports: reachable only on the container address.
			name: "dhi-noalias", image: "nginx:alpine",
			publishedName: "dhi-noalias.docker.local",
			directPort:    portNginx,
			wantPublished: true,
		},
		{
			// Exposes a port but publishes nothing.
			name: "dhi-noports", image: "nginx:alpine",
			extra:         []string{"-P"},
			publishedName: "dhi-noports.docker.local",
			wantPublished: true,
		},
		{
			// Created but never started: no address, no record.
			name: "dhi-created", image: "nginx:alpine",
			noStart:       true,
			wantPublished: false,
		},
		{
			// Shares the host network: publishing it would map every host
			// service onto the container's name.
			name: "dhi-hostnet", image: "nginx:alpine",
			host:          true,
			wantPublished: false,
		},
		{
			// No network at all.
			name: "dhi-nonet", image: "nginx:alpine",
			none:          true,
			wantPublished: false,
		},
		{
			// A name with characters that are illegal in a host name.
			name: "dhi-weird_name", image: "nginx:alpine",
			publishedName: "dhi-weird_name.docker.local",
			aliases:       []string{"dhi-weird--name.docker.local"},
			wantPublished: true,
		},
		{
			// A genuinely reserved name. Docker accepts it, and publishing it
			// would shadow the host's own resolution of localhost, which is
			// exactly the harm the rule exists to prevent.
			name: "localhost", image: "nginx:alpine",
			wantPublished: false,
		},
		{
			// A server on a port that is not 80, to prove the mapping is not
			// accidentally coupled to the usual web port.
			name: "dhi-python", image: "python:3-alpine",
			// The image has an entrypoint, so the interpreter is named
			// explicitly rather than relying on the default command.
			cmd: []string{
				"python", "-m", "http.server", strconv.Itoa(portPython),
			},
			ports:         []string{fmt.Sprintf("%d:%d", pyPort, portPython)},
			publishedName: "dhi-python.docker.local",
			publishedPort: pyPort, directPort: portPython,
			wantPublished: true,
		},
		{
			// busybox httpd on another non-standard port. The port it listens
			// on inside the container is not the host port, and conflating the
			// two makes the mapping point at nothing. It serves a real index so
			// that a 200 proves the request reached the container.
			name: "dhi-busy", image: "busybox:latest",
			cmd: []string{
				"sh", "-c",
				"echo ok > /tmp/index.html && exec httpd -f -p " +
					strconv.Itoa(portBusy) + " -h /tmp",
			},
			ports:         []string{fmt.Sprintf("%d:%d", busyPort, portBusy)},
			publishedName: "dhi-busy.docker.local",
			publishedPort: busyPort,
			directPort:    portBusy,
			wantPublished: true,
		},
	}

	// Tear down anything left from a previous run.
	for _, n := range estateNames() {
		_ = runQuiet(30*time.Second, "docker", "rm", "-f", "-v", n)
	}
	removeNetwork(testNetwork)
	createNetwork(testNetwork)

	// Create and start them one at a time, so a failure names the container
	// that caused it.
	for _, s := range specs {
		args := []string{"create", "--name", s.name}
		switch {
		case s.host:
			args = append(args, "--network", "host")
		case s.none:
			args = append(args, "--network", "none")
		case s.network != "":
			args = append(args, "--network", s.network)
		}
		for _, p := range s.ports {
			args = append(args, "-p", p)
		}
		args = append(args, s.extra...)
		args = append(args, s.image)
		args = append(args, s.cmd...)
		if out, err := run(60*time.Second, "docker", args...); err != nil {
			panic(fmt.Sprintf("create %s: %v: %s", s.name, err, strings.TrimSpace(out)))
		}
		if !s.noStart {
			if out, err := run(60*time.Second, "docker", "start", s.name); err != nil {
				panic(fmt.Sprintf("start %s: %v: %s", s.name, err, strings.TrimSpace(out)))
			}
		}
	}

	// A stopped container: created, started once, then stopped.
	stopped := estateSpec{name: "dhi-stopped", image: "nginx:alpine"}
	if out, err := run(60*time.Second, "docker", "run", "-d", "--name", stopped.name, stopped.image); err != nil {
		panic(fmt.Sprintf("create the stopped container: %v: %s", err, strings.TrimSpace(out)))
	}
	_ = runQuiet(30*time.Second, "docker", "stop", stopped.name)
	specs = append(specs, stopped)

	// Wait for every container to settle, so the first reconcile sees a stable
	// world rather than a half-created estate.
	for _, s := range specs {
		if s.noStart || s.name == "dhi-stopped" {
			continue
		}
		waitContainerRunning(s.name)
	}

	return specs
}

// removeNetwork deletes a network, tolerating one that does not exist.
//
// Docker removes a network asynchronously, so the caller must treat the call as
// requested rather than completed. createNetwork therefore retries.
func removeNetwork(name string) {
	_ = runQuiet(20*time.Second, "docker", "network", "rm", name)
}

// createNetwork creates a network, retrying while a previous removal settles.
func createNetwork(name string) {
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		if out, err := run(30*time.Second, "docker", "network", "create", name); err == nil {
			return
		} else {
			lastErr = fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
		}
		removeNetwork(name)
		time.Sleep(300 * time.Millisecond)
	}
	panic(fmt.Sprintf("create the test network %s after retries: %v", name, lastErr))
}

func waitContainerRunning(name string) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if containerRunning(name) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	panic(fmt.Sprintf("container %s never reached the running state (state=%q)",
		name, containerState(name)))
}

// --- agent lifecycle -----------------------------------------------------

// agentProcess controls a running agent.
type agentProcess struct {
	cmd *exec.Cmd
	log string
}

// startAgent runs the agent against the sandbox hosts file and the web UI.
func startAgent(logPath string) *agentProcess {
	logFile, err := os.Create(logPath)
	if err != nil {
		panic(fmt.Sprintf("create the agent log: %v", err))
	}

	cmd := exec.Command(agentBin)
	// The agent reads its configuration from the environment, not from argv.
	cmd.Env = append(os.Environ(), agentEnv()...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		panic(fmt.Sprintf("start the agent: %v", err))
	}

	p := &agentProcess{cmd: cmd, log: logPath}

	// Wait for the first apply so tests never race the startup reconcile.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		// Check the process first. Without this, a stale agent holding the
		// port would answer /healthz and the suite would go on testing
		// somebody else's binary, which is a maddening failure to diagnose.
		if p.cmd.ProcessState != nil {
			panic("the agent exited during startup:\n" + p.dump())
		}
		if httpGet("http://"+webAddr+"/healthz") == 200 {
			// Confirm it is our agent and not a leftover.
			body, _ := httpBody("http://" + webAddr + "/api/config")
			if !strings.Contains(body, hostsPath) {
				panic("another service is answering on " + webAddr +
					"; its hosts_file is not the sandbox:\n" + body)
			}
			return p
		}
		time.Sleep(200 * time.Millisecond)
	}
	panic("the agent never became ready:\n" + p.dump())
}

// dump returns the tail of the agent's log, for startup failures.
func (p *agentProcess) dump() string {
	data, err := os.ReadFile(p.log)
	if err != nil {
		return "(no log: " + err.Error() + ")"
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return strings.Join(lines, "\n")
}

// agentEnv is the environment the agent under test runs with.
func agentEnv() []string {
	return []string{
		"HOSTS_FILE=" + hostsPath,
		"HOSTS_MOUNT_MODE=file",
		"DNS_SUFFIX=docker.local",
		"TARGET_MODE=both",
		"RESYNC_INTERVAL=5s",
		"EVENT_DEBOUNCE=100ms",
		"LOG_FORMAT=json",
		"LOG_LEVEL=debug",
		"WEB_ENABLED=true",
		"WEB_ADDR=" + webAddr,
		"DOCKER_HOST=" + dockerHost,
	}
}

// kill terminates the agent with SIGKILL, simulating a hard crash.
// killNow terminates the agent immediately. It is idempotent, so it is safe to
// call both from a test cleanup and from teardown.
func (p *agentProcess) killNow() error {
	if p.cmd.Process == nil {
		return nil
	}
	err := p.cmd.Process.Kill()
	_, _ = p.cmd.Process.Wait()
	p.cmd.Process = nil
	return err
}

func (p *agentProcess) kill(t *testing.T) {
	t.Helper()
	if p.cmd.Process == nil {
		return
	}
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the agent: %v", err)
	}
	_ = p.cmd.Wait()
}

// stop asks the agent to shut down cleanly.
func (p *agentProcess) stop() {
	if p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Signal(os.Interrupt)
	done := make(chan struct{})
	go func() { _, _ = p.cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = p.cmd.Process.Kill()
	}
}

// alive reports whether the process is still running.
func (p *agentProcess) alive() bool {
	if p.cmd.Process == nil {
		return false
	}
	return p.cmd.ProcessState == nil
}

// waitExit blocks until the process exits or the timeout expires, returning
// whether it exited.
func (p *agentProcess) waitExit(d time.Duration) bool {
	done := make(chan struct{})
	go func() { _, _ = p.cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// logContains reports whether the agent log mentions something.
func (p *agentProcess) logContains(t *testing.T, needle string) bool {
	t.Helper()
	data, err := os.ReadFile(p.log)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), needle)
}

// webUI queries the monitoring API.
type webUI struct{ base string }

func (w webUI) url(path string) string { return "http://" + w.base + path }

func (w webUI) entries(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if code := jsonGet(w.url("/api/entries"), &out); code != 200 {
		t.Fatalf("GET /api/entries returned %d", code)
	}
	return out
}

func (w webUI) config(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	if code := jsonGet(w.url("/api/config"), &out); code != 200 {
		t.Fatalf("GET /api/config returned %d", code)
	}
	return out
}

func (w webUI) health(t *testing.T) (map[string]any, int) {
	t.Helper()
	var out map[string]any
	code := jsonGet(w.url("/healthz"), &out)
	return out, code
}

func (w webUI) metrics(t *testing.T) string {
	t.Helper()
	body, code := httpBody(w.url("/metrics"))
	if code != 200 {
		t.Fatalf("GET /metrics returned %d", code)
	}
	return body
}

func (w webUI) index(t *testing.T) (string, int) {
	t.Helper()
	return httpBody(w.url("/"))
}

func (w webUI) indexStatus(t *testing.T) int {
	t.Helper()
	_, code := httpBody(w.url("/"))
	return code
}
