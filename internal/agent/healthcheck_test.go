package agent

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mint/docker-hoster-injector/internal/config"
)

func TestProbeAddr(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		":8080":          "127.0.0.1:8080",
		"0.0.0.0:9000":   "127.0.0.1:9000",
		"[::]:9000":      "127.0.0.1:9000",
		"127.0.0.1:8080": "127.0.0.1:8080",
		"10.1.2.3:80":    "10.1.2.3:80",
		"garbage":        "garbage",
	}
	for in, want := range cases {
		if got := probeAddr(in); got != want {
			t.Errorf("probeAddr(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHealthCheck(t *testing.T) {
	t.Parallel()

	serve := func(status int) config.Config {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/healthz" {
				http.NotFound(w, r)
				return
			}
			w.WriteHeader(status)
		}))
		t.Cleanup(srv.Close)
		return config.Config{Web: config.Web{Enabled: true, Addr: srv.Listener.Addr().String()}}
	}

	tests := []struct {
		name    string
		cfg     config.Config
		wantErr bool
	}{
		{"healthy", serve(http.StatusOK), false},
		{"degraded", serve(http.StatusServiceUnavailable), true},
		{"web disabled has nothing to ask", config.Config{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := HealthCheck(context.Background(), tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}

	t.Run("nobody listening", func(t *testing.T) {
		t.Parallel()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		cfg := config.Config{Web: config.Web{Enabled: true, Addr: addr}}
		if err := HealthCheck(context.Background(), cfg); err == nil {
			t.Fatal("a closed port reported healthy")
		}
	})
}
