// Package webui serves a read-only monitoring page.
//
// The service has no authentication by design: it is meant for an internal
// network, and it only exposes GET endpoints that return container names,
// addresses and ports. It must therefore never expose a mutating route, and it
// defaults to being bound by the operator to loopback.
//
// Updates are pushed over Server-Sent Events rather than polled, because the
// agent already knows the moment the state changes and polling would either be
// slower or busier for no gain.
package webui

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mint/docker-hoster-injector/internal/reconcile"
)

// Snapshot is the read model the page renders.
//
// It is deliberately a plain struct with JSON tags rather than the internal
// result type: the API must not leak internal representation into a contract
// that browsers depend on.
type Snapshot struct {
	// GeneratedAt is when the state was computed.
	GeneratedAt time.Time `json:"generated_at"`
	// Records is one entry per published container address.
	Records []Record `json:"records"`
	// Skipped is one entry per container that is deliberately not published.
	Skipped []SkippedContainer `json:"skipped"`
	// Summary holds the counters shown in the page header.
	Summary Summary `json:"summary"`
}

// Record is one published address and the names that resolve to it.
type Record struct {
	Address string   `json:"address"`
	Names   []string `json:"names"`
	// Container is the name of the container this record came from.
	Container string `json:"container"`
	// ContainerID identifies it unambiguously.
	ContainerID string `json:"container_id"`
	// State is the Docker state at the time of the reconcile.
	State string `json:"state"`
}

// SkippedContainer explains why a container has no record.
type SkippedContainer struct {
	Container   string `json:"container"`
	ContainerID string `json:"container_id"`
	State       string `json:"state"`
	Reason      string `json:"reason"`
}

// Summary holds the aggregate figures shown in the header.
type Summary struct {
	Containers int `json:"containers"`
	Records    int `json:"records"`
	Names      int `json:"names"`
	Skipped    int `json:"skipped"`
}

// Config carries the static facts about the deployment, shown in the page
// header and stored so a client can tell one agent from another.
type Config struct {
	// DNSSuffix is the configured base domain.
	DNSSuffix string
	// HostsFile is the path being managed.
	HostsFile string
	// TargetMode, MountMode and Version are echoed for troubleshooting.
	TargetMode string
	MountMode  string
	Version    string
	// Source is the host the agent runs on, useful when several agents are
	// published from different machines.
	Source string
}

// Server is the monitoring HTTP server.
type Server struct {
	cfg    Config
	log    *slog.Logger
	hub    *hub
	server *http.Server

	// mu guards lastResult's replacement and the stats provider.
	mu sync.RWMutex

	// lastResult holds the most recent snapshot, so a client that connects
	// between updates still sees the current state immediately.
	lastResult atomic.Pointer[Snapshot]

	// stats, when set, supplies the applier's counters.
	stats func() (writes uint64, lastErr error, lastApply time.Time)
}

// New builds a Server.
func New(cfg Config, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		cfg: cfg,
		log: log,
		hub: newHub(),
	}
}

// SetStatsProvider connects the health endpoint to the applier's counters. It
// is a method rather than a constructor argument so that the wiring order in
// main stays simple.
func (s *Server) SetStatsProvider(fn func() (writes uint64, lastErr error, lastApply time.Time)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats = fn
}

// Publish records a new state and wakes every connected client.
func (s *Server) Publish(res reconcile.Result) {
	snap := s.buildSnapshot(res)
	s.lastResult.Store(&snap)
	s.hub.publish(snap)
}

// current returns the most recent snapshot, or an empty one.
func (s *Server) current() Snapshot {
	if v := s.lastResult.Load(); v != nil {
		return *v
	}
	return s.buildSnapshot(reconcile.Result{})
}

// Handler returns the routed handler.
//
// Only GET and HEAD are routed. Anything else gets a 405 and a clear message,
// because a monitoring page that accepted writes would need authentication.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("GET /api/entries", s.handleEntries)
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("GET /api/events", s.handleEvents)

	// Registered without a method so the response can explain the API rather
	// than Go's bare "method not allowed".
	mux.HandleFunc("/", s.handleRoot)

	return s.withLogging(mux)
}

// handleMetrics exposes counters in the Prometheus text format.
//
// It is read-only and derived from the same state the page shows, so a scrape
// and a browser can never disagree.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	snap := s.current()

	s.mu.RLock()
	stats := s.stats
	s.mu.RUnlock()

	var (
		writes    uint64
		lastErr   error
		lastApply time.Time
	)
	if stats != nil {
		writes, lastErr, lastApply = stats()
	}

	var b strings.Builder
	gauge := func(name, help string, value int) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, value)
	}

	gauge("dhi_containers", "Containers that are published.", snap.Summary.Containers)
	gauge("dhi_records", "Published host file records.", snap.Summary.Records)
	gauge("dhi_names", "Published host names.", snap.Summary.Names)
	gauge("dhi_skipped", "Containers deliberately not published.", snap.Summary.Skipped)
	gauge("dhi_sse_clients", "Connected event stream clients.", s.hub.count())

	if writes > 0 {
		fmt.Fprintf(&b, "# HELP dhi_hosts_writes_total Successful writes of the hosts file.\n"+
			"# TYPE dhi_hosts_writes_total counter\ndhi_hosts_writes_total %d\n", writes)
	}
	if !lastApply.IsZero() {
		fmt.Fprintf(&b, "# HELP dhi_last_apply_timestamp_seconds Unix time of the last successful apply.\n"+
			"# TYPE dhi_last_apply_timestamp_seconds gauge\ndhi_last_apply_timestamp_seconds %d\n",
			lastApply.Unix())
	}
	// A failed apply is a gauge rather than a counter because the condition is
	// current state, not something that accumulates.
	errValue := 0
	if lastErr != nil {
		errValue = 1
	}
	gauge("dhi_last_apply_failed", "1 when the most recent apply failed.", errValue)

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(b.String()))
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context, addr string) error {
	s.server = &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		// No write timeout: the SSE stream is long lived by design. The read
		// timeout still applies, which is what bounds a slow client.
		IdleTimeout: 120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.log.Info("web UI listening", "addr", addr)
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	// Shutdown closes the SSE connections by ending their responses, which is
	// what unblocks the clients.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.server.Shutdown(shutdownCtx)
}
