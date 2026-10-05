package agent

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/manprint/docker-hoster-injector/internal/config"
)

// healthTimeout bounds the whole probe. Docker's own healthcheck timeout is
// longer, and a probe that hangs must report failure, not stall the check.
const healthTimeout = 3 * time.Second

// HealthCheck asks the running agent, over its own web endpoint, whether it is
// healthy. It is what the image's HEALTHCHECK runs: the image has no shell and
// no curl, so the binary is its own probe.
//
// With the web UI disabled there is nothing to ask, and the check passes: a
// container that cannot be probed is not thereby unhealthy.
func HealthCheck(ctx context.Context, cfg config.Config) error {
	if !cfg.Web.Enabled {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+probeAddr(cfg.Web.Addr)+"/healthz", nil)
	if err != nil {
		return fmt.Errorf("build the request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("ask the agent: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the agent reports %s", resp.Status)
	}
	return nil
}

// probeAddr turns the listen address into one that can be dialled: an empty or
// wildcard host means "this machine".
func probeAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	switch strings.Trim(host, "[]") {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
