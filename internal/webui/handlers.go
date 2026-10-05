package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// handleIndex serves the monitoring page.
func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	page, err := renderIndex(s.cfg, s.current())
	if err != nil {
		s.log.Error("render the page", "error", err)
		http.Error(w, "the page could not be rendered", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write([]byte(page)); err != nil {
		s.clientGone("index", err)
	}
}

// handleEntries serves the JSON read model.
//
// This is also the fallback for browsers where the event stream does not work,
// so it must be complete on its own.
func (s *Server) handleEntries(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, s.current())
}

// handleConfig serves the static configuration of the deployment.
func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, map[string]string{
		"dns_suffix":  s.cfg.DNSSuffix,
		"hosts_file":  s.cfg.HostsFile,
		"target_mode": s.cfg.TargetMode,
		"mount_mode":  s.cfg.MountMode,
		"version":     s.cfg.Version,
		"source":      s.cfg.Source,
	})
}

// handleHealth reports whether the agent is functioning.
//
// It is a liveness check with a light readiness signal: the process answering
// at all is the signal, and a failed last apply is surfaced so an operator can
// see a degraded agent without reading the logs.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	stats := s.stats
	s.mu.RUnlock()

	body := map[string]any{
		"status":  "ok",
		"records": s.current().Summary.Records,
	}
	status := http.StatusOK

	if stats != nil {
		writes, lastApply, lastErr := stats()
		body["writes"] = writes
		if !lastApply.IsZero() {
			body["last_apply"] = lastApply.UTC().Format(time.RFC3339)
		}
		if lastErr != nil {
			body["status"] = "degraded"
			body["last_error"] = lastErr.Error()
			status = http.StatusServiceUnavailable
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		s.clientGone("healthz", err)
	}
}

// handleEvents streams updates over Server-Sent Events.
//
// Each message is a complete snapshot rather than a diff. That is a deliberate
// trade: the payload is small, and a client can therefore reconnect at any time
// and be correct without replaying history it might have missed.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported by this server", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	// Proxies that buffer would defeat the purpose of streaming.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	updates, unsubscribe := s.hub.subscribe()
	defer unsubscribe()

	// The first message carries the current state so a client never shows an
	// empty table while waiting for a change.
	if err := writeEvent(w, "snapshot", s.current()); err != nil {
		return
	}
	flusher.Flush()

	// A periodic comment keeps the connection alive through intermediaries that
	// close idle connections, and gives the client a liveness signal.
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case snap, ok := <-updates:
			if !ok {
				return
			}
			if err := writeEvent(w, "snapshot", snap); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// handleRoot explains the API for anything that is not a known route.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w,
			"This service is read-only and answers GET requests only. "+
				"Available routes: /, /api/entries, /api/config, /api/events, /healthz",
			http.StatusMethodNotAllowed)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		s.clientGone("json", err)
	}
}

// clientGone records a response that could not be written. Once the status is
// sent there is nothing to change for the client, and the usual cause is the
// client having closed the connection, so it is a debug line and not an error.
func (s *Server) clientGone(what string, err error) {
	s.log.Debug("could not write the response", "what", what, "error", err)
}

func writeEvent(w http.ResponseWriter, name string, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, payload)
	return err
}

// withLogging records each request at debug level.
//
// Deliberately not the default logger's Info level: an operator watching a
// dashboard would otherwise drown in access logs, and the log is meant to
// report state changes.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		s.log.Debug("request",
			"method", r.Method,
			"path", r.URL.Path,
			"duration", time.Since(start))
	})
}
