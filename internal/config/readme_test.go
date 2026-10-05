package config_test

import (
	"os"
	"strings"
	"testing"

	"github.com/mint/docker-hoster-injector/internal/config"
)

// TestDefaultsMatchReadme guards the README table against drift. Documentation
// that contradicts the code is worse than no documentation.
func TestDefaultsMatchReadme(t *testing.T) {
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Skipf("README not readable: %v", err)
	}
	doc := string(raw)

	cfg, err := config.LoadFrom(func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]string{
		config.EnvDNSSuffix:      cfg.DNSSuffix,
		config.EnvHostsFile:      cfg.HostsFile,
		config.EnvMountMode:      string(cfg.MountMode),
		config.EnvTargetMode:     string(cfg.TargetMode),
		config.EnvResyncInterval: cfg.ResyncInterval.String(),
		config.EnvEventDebounce:  cfg.EventDebounce.String(),
		config.EnvWebAddr:        cfg.Web.Addr,
	}
	for k, v := range map[string]string{
		config.EnvLogLevel:   "info",
		config.EnvLogFormat:  "json",
		config.EnvWebEnabled: "true",
		config.EnvWebEvents:  "true",
	} {
		want[k] = v
	}

	for env, def := range want {
		row := "| `" + env + "` | `" + def + "` |"
		if !strings.Contains(doc, row) {
			t.Errorf("README does not document %s=%s (expected the row %q)", env, def, row)
		}
	}
}
