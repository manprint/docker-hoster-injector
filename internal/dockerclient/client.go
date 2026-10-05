// Package dockerclient wraps the Docker Engine API behind a small, testable
// surface.
//
// The agent only needs three things from Docker: the list of running
// containers, a stream of lifecycle events, and the engine version. Isolating
// them behind an interface keeps the reconciler free of Docker types and makes
// it testable without a daemon.
package dockerclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

	"github.com/manprint/docker-hoster-injector/internal/version"
)

// ErrUnsupportedAPI reports an engine older than the oldest release this agent
// supports.
var ErrUnsupportedAPI = errors.New("unsupported Docker API version")

// MinAPIVersion is the oldest API this agent is tested against, which is Docker
// 19.03. It is checked against the engine's reported API version. The agent
// uses only the container list, the event stream and the engine version, all of
// which are older than that. Requests pinned to every version from 1.40 upward
// are exercised against a real daemon (see test/integration/apiversion_test.go),
// but no engine older than the 1.40 floor of the current daemon is available to
// the tests, so the floor is a verified lower bound and not a guess.
const MinAPIVersion = "1.40"

// maxInspectConcurrency bounds how many containers are inspected at once when
// recovering their network aliases. Too high and a busy host produces a burst
// of connections; too low and the resync gets slow. Eight is comfortable
// either way.
const maxInspectConcurrency = 8

// callTimeout bounds one unary Docker API call (ping, list, inspect). Without
// it a daemon that accepts the connection but never answers would block the
// only goroutine that rewrites the hosts file. It deliberately does not apply
// to the event stream, which is long lived by design.
const callTimeout = 15 * time.Second

// EngineInfo describes the daemon the agent is talking to.
type EngineInfo struct {
	// Version is the engine version, e.g. "29.8.2".
	Version string
	// APIVersion is the negotiated API version, e.g. "1.51".
	APIVersion string
	// OSType is "linux" on every platform this agent supports.
	OSType string
}

// Container is the subset of Docker's container summary the agent acts on.
//
// It is a plain struct rather than Docker's type so that the reconciler and
// its tests never depend on the Docker API's shape.
type Container struct {
	// ID is the container's full ID, used to break ties between competing
	// claims on a name.
	ID string
	// Name is the primary name, without Docker's leading slash.
	Name string
	// State is "running", "paused", "exited", ...
	State string
	// Created is the creation timestamp, used as the collision priority.
	Created time.Time
	// Networks maps a network name to the addresses of this container on it.
	Networks []Network
	// NetworkMode is "host", "none", "bridge" or a user defined network name.
	NetworkMode string
	// PublishedPorts describes the host side port bindings.
	PublishedPorts []Port
}

// Network is one network a container is attached to.
type Network struct {
	// Name is the network name, for example "bridge" or "myproject_default".
	Name string
	// IPv4 and IPv6 are the container addresses on this network. Either may
	// be empty, for example a network with IPv6 disabled.
	IPv4 string
	IPv6 string
	// Aliases are the extra DNS names set on this endpoint, which is how
	// Compose service names and "network-alias" values reach a container.
	Aliases []string
	// DNSNames is what the engine derives for the endpoint, including the
	// service name and the container short ID.
	DNSNames []string
}

// Port is one published port.
type Port struct {
	// HostIP is the bind address. Empty means "all interfaces", which the
	// agent treats as reachable on loopback.
	HostIP string
	// HostPort is the port on the host, as an integer. Empty when the
	// container only exposes the port without publishing it.
	HostPort int
	// ContainerPort is the port inside the container.
	ContainerPort int
	// Protocol is "tcp" or "udp".
	Protocol string
}

// Published reports whether the port is reachable from the host.
//
// The check is strictly positive rather than "not zero": a zero port means the
// container only exposes it, and a negative value would mean the conversion
// from Docker's unsigned integer went wrong. Neither is a published port, and
// treating either as one would publish an address that cannot work.
func (p Port) Published() bool { return p.HostPort > 0 }

// Event is a container lifecycle event, reduced to the fields the agent uses.
//
// Docker's own event type is not exposed past this package, so that the rest
// of the agent never depends on the Engine API's shape.
type Event struct {
	// Type is the object class, always "container" for this stream.
	Type string
	// Action is what happened, for example "start", "die" or "destroy".
	Action string
	// ContainerID is the ID from the event actor.
	ContainerID string
	// ContainerName is the name attribute, when the daemon provides it.
	ContainerName string
}

// API is the surface the agent needs from Docker. It is defined here, in the
// consuming package's sibling, so that a fake can be written in tests without
// depending on a daemon.
type API interface {
	// Info returns the engine version, after checking the API floor.
	Info(ctx context.Context) (EngineInfo, error)
	// ListRunning returns the containers currently in the running state.
	ListRunning(ctx context.Context) ([]Container, error)
	// Events streams lifecycle events until ctx is cancelled. Both channels
	// are closed when the stream ends; callers are expected to reconnect and
	// reconcile, because whatever happened during the gap was never delivered.
	Events(ctx context.Context) (<-chan Event, <-chan error, error)
	// Close releases the underlying connections.
	Close() error
}

// Client is the real implementation backed by the Docker Engine API.
type Client struct {
	api client.APIClient
	log *slog.Logger
}

// Compile-time proof that the real client satisfies the interface, so a
// signature change upstream is caught here rather than at wiring time.
var _ API = (*Client)(nil)

// Options configures New.
type Options struct {
	// Host overrides DOCKER_HOST. Empty means "read the environment", which
	// is what makes rootless Docker work with no configuration.
	Host string
	// APIVersion pins the API version. Empty means negotiate automatically.
	APIVersion string
	// Timeout bounds a single API call. Zero means the client default.
	Timeout time.Duration
	// Logger receives diagnostics such as a failed alias lookup. Nil discards
	// them.
	Logger *slog.Logger
}

// New builds a client and verifies the daemon is usable.
func New(ctx context.Context, opts Options) (*Client, error) {
	// Version negotiation is the client's default: one binary works against
	// Docker 19.03 through 29 and beyond because the client downgrades to whatever
	// the daemon supports. It is switched off only by pinning a version below.
	var apiOpts []client.Opt
	switch {
	case opts.Host != "":
		apiOpts = append(apiOpts, client.WithHost(opts.Host))
	default:
		apiOpts = append(apiOpts, client.WithHostFromEnv())
	}
	if opts.APIVersion != "" {
		apiOpts = append(apiOpts, client.WithAPIVersion(opts.APIVersion))
	}
	if opts.Timeout > 0 {
		apiOpts = append(apiOpts, client.WithTimeout(opts.Timeout))
	}

	api, err := client.New(apiOpts...)
	if err != nil {
		return nil, fmt.Errorf("create the Docker client: %w", err)
	}

	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Client{api: api, log: log}
	// Fail fast and loudly at startup rather than on the first event.
	if _, err := c.Info(ctx); err != nil {
		_ = api.Close()
		return nil, err
	}
	return c, nil
}

// Info returns the engine version, having checked the API floor.
func (c *Client) Info(ctx context.Context) (EngineInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	// Ping is used rather than ServerVersion because it does not depend on the
	// negotiated version exposing every field, so it works against the oldest
	// supported engine.
	ping, err := c.api.Ping(ctx, client.PingOptions{})
	if err != nil {
		return EngineInfo{}, fmt.Errorf("reach the Docker daemon (is the socket mounted?): %w", err)
	}

	info := EngineInfo{
		APIVersion: ping.APIVersion,
		OSType:     ping.OSType,
	}

	// Enforce the floor. Failing here with a clear message is far kinder than
	// letting the agent run against an engine whose API does not provide the
	// endpoints it relies on, and failing much later with a 404.
	if ping.APIVersion != "" && !version.AtLeast(ping.APIVersion, MinAPIVersion) {
		return info, fmt.Errorf("%w: the daemon speaks API %s but at least %s (Docker 19.03) is required; "+
			"upgrade Docker", ErrUnsupportedAPI, ping.APIVersion, MinAPIVersion)
	}

	ver, err := c.api.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		// Not fatal: the API version is what gates compatibility, and a
		// daemon that refuses ServerVersion still serves the endpoints used.
		return info, nil
	}
	info.Version = ver.Version
	return info, nil
}

// ListRunning returns the running containers, including their network aliases.
//
// The list endpoint is used for the bulk of the data, then each container is
// inspected to recover its aliases. That second step is not optional: Docker's
// /containers/json returns null for both "Aliases" and "DNSNames", and only
// /containers/{id}/json carries them. Skipping it would silently drop every
// Compose service name, which is a headline feature.
//
// Inspections run concurrently with a bounded worker pool, so a host with many
// containers still costs a bounded number of concurrent round trips rather
// than a serial chain.
func (c *Client) ListRunning(ctx context.Context) ([]Container, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	res, err := c.api.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("status", "running"),
	})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	out := make([]Container, len(res.Items))
	for i, item := range res.Items {
		out[i] = convert(item)
	}

	c.fillAliases(ctx, out)
	return out, nil
}

// fillAliases populates each container's network aliases via inspect.
//
// Aliases are attached to the network they belong to: an alias set on one
// network says nothing about the others.
func (c *Client) fillAliases(ctx context.Context, containers []Container) {
	if len(containers) == 0 {
		return
	}

	workers := len(containers)
	if workers > maxInspectConcurrency {
		workers = maxInspectConcurrency
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, workers)

	for i := range containers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			byNetwork, err := c.aliasesOf(ctx, containers[i].ID)
			if err != nil {
				// Not fatal: the container is still published under its own
				// name, only the aliases are missing. Losing an alias is a
				// smaller problem than losing the record entirely, but it is
				// worth a warning because the symptom (a missing Compose
				// service name) is otherwise hard to explain.
				c.log.Warn("could not read the network aliases",
					"container", shortID(containers[i].ID), "error", err)
				return
			}
			for j := range containers[i].Networks {
				if names, ok := byNetwork[containers[i].Networks[j].Name]; ok {
					containers[i].Networks[j].Aliases = names
				}
			}
		}(i)
	}

	wg.Wait()
}

// aliasesOf returns, for each network endpoint of a container, the union of its
// aliases and DNS names.
func (c *Client) aliasesOf(ctx context.Context, id string) (map[string][]string, error) {
	detail, err := c.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("inspect container %s: %w", shortID(id), err)
	}
	networks := detail.Container.NetworkSettings
	if networks == nil {
		return nil, nil
	}

	out := make(map[string][]string, len(networks.Networks))
	for name, ep := range networks.Networks {
		if ep == nil {
			continue
		}
		var names []string
		names = append(names, ep.Aliases...)
		names = append(names, ep.DNSNames...)
		out[name] = dedupeSorted(names)
	}
	return out, nil
}

// dedupeSorted removes duplicates while keeping the order, and drops blanks.
func dedupeSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// shortID trims an ID for log messages.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// Events streams container lifecycle events.
//
// Docker's channels are translated into the agent's own Event type here, so
// that no other package needs to import the Engine API.
func (c *Client) Events(ctx context.Context) (<-chan Event, <-chan error, error) {
	res := c.api.Events(ctx, client.EventsListOptions{
		Filters: make(client.Filters).Add("type", string(events.ContainerEventType)),
	})
	if res.Err == nil || res.Messages == nil {
		return nil, nil, errors.New("the Docker daemon returned no event stream")
	}

	out := make(chan Event)
	errOut := make(chan error, 1)

	go func() {
		defer close(out)
		defer close(errOut)

		messages := res.Messages
		for {
			select {
			case <-ctx.Done():
				return

			case msg, ok := <-messages:
				if !ok {
					// Docker's client never closes this channel today, but
					// nothing in its contract forbids it. A nil channel blocks
					// forever, so the loop keeps waiting for the error below
					// instead of spinning on a closed one.
					messages = nil
					continue
				}
				select {
				case out <- Event{
					Type:          string(msg.Type),
					Action:        string(msg.Action),
					ContainerID:   msg.Actor.ID,
					ContainerName: msg.Actor.Attributes["name"],
				}:
				case <-ctx.Done():
					// The consumer stopped reading. Abandon the stream instead
					// of blocking forever on a send nobody will receive.
					return
				}

			case err, ok := <-res.Err:
				// Docker reports the end of the stream, whatever the cause, on
				// this channel and then closes it. It is the only reliable end
				// of stream signal, because the message channel stays open.
				// The error is relayed so the caller can log why; closing both
				// output channels tells it to reconnect and reconcile.
				if ok && err != nil {
					errOut <- err
				}
				return
			}
		}
	}()

	return out, errOut, nil
}

// Close releases the client's connections.
func (c *Client) Close() error {
	if c.api == nil {
		return nil
	}
	return c.api.Close()
}

// convert maps Docker's summary onto the agent's own type.
func convert(s container.Summary) Container {
	c := Container{
		ID:      s.ID,
		State:   string(s.State),
		Created: time.Unix(s.Created, 0).UTC(),
	}
	for _, n := range s.Names {
		// Docker prefixes container names with a slash.
		if name := trimLeadingSlash(n); name != "" {
			c.Name = name
			break
		}
	}
	c.NetworkMode = s.HostConfig.NetworkMode

	if s.NetworkSettings != nil {
		// The map has no defined iteration order, so it is sorted to keep the
		// published records stable across reconciles.
		for _, name := range sortedKeys(s.NetworkSettings.Networks) {
			ep := s.NetworkSettings.Networks[name]
			if ep == nil {
				continue
			}
			n := Network{
				Name:     name,
				Aliases:  ep.Aliases,
				DNSNames: ep.DNSNames,
			}
			if ep.IPAddress.IsValid() {
				n.IPv4 = ep.IPAddress.String()
			}
			// The IPv6 address of a dual stack endpoint has its own field.
			if ep.GlobalIPv6Address.IsValid() {
				n.IPv6 = ep.GlobalIPv6Address.String()
			}
			// Defensive: an IPv6 address reported in IPAddress is still an IPv6
			// address, while an IPv4-mapped one is IPv4 in disguise.
			if addr := ep.IPAddress; addr.Is6() && !addr.Is4In6() {
				if n.IPv6 == "" {
					n.IPv6 = addr.String()
				}
				n.IPv4 = ""
			}
			c.Networks = append(c.Networks, n)
		}
	}

	for _, p := range s.Ports {
		c.PublishedPorts = append(c.PublishedPorts, convertPort(p))
	}
	return c
}

func convertPort(p container.PortSummary) Port {
	out := Port{
		HostPort:      int(p.PublicPort),
		ContainerPort: int(p.PrivatePort),
		Protocol:      string(p.Type),
	}
	if p.IP.IsValid() {
		out.HostIP = p.IP.String()
	}
	return out
}

func trimLeadingSlash(s string) string {
	for len(s) > 0 && s[0] == '/' {
		s = s[1:]
	}
	return s
}
