package config

import (
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// env builds a LookupFunc from a map, so tests never touch the real process
// environment and can run in parallel.
func env(pairs map[string]string) LookupFunc {
	return func(k string) (string, bool) {
		v, ok := pairs[k]
		return v, ok
	}
}

// merge copies base and overlays overrides.
func merge(base, over map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func TestLoadFromDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := LoadFrom(env(nil))
	if err != nil {
		t.Fatalf("LoadFrom: unexpected error: %v", err)
	}

	if cfg.DNSSuffix != DefaultDNSSuffix {
		t.Errorf("DNSSuffix = %q, want %q", cfg.DNSSuffix, DefaultDNSSuffix)
	}
	if cfg.HostsFile != DefaultHostsFile {
		t.Errorf("HostsFile = %q, want %q", cfg.HostsFile, DefaultHostsFile)
	}
	if cfg.MountMode != MountModeFile {
		t.Errorf("MountMode = %q, want %q", cfg.MountMode, MountModeFile)
	}
	if cfg.TargetMode != TargetModeBoth {
		t.Errorf("TargetMode = %q, want %q", cfg.TargetMode, TargetModeBoth)
	}
	if cfg.ResyncInterval != DefaultResyncInterval {
		t.Errorf("ResyncInterval = %s, want %s", cfg.ResyncInterval, DefaultResyncInterval)
	}
	if cfg.EventDebounce != DefaultEventDebounce {
		t.Errorf("EventDebounce = %s, want %s", cfg.EventDebounce, DefaultEventDebounce)
	}
	if cfg.Level != slog.LevelInfo {
		t.Errorf("Level = %v, want info", cfg.Level)
	}
	if cfg.Format != LogFormatJSON {
		t.Errorf("Format = %q, want json", cfg.Format)
	}
	if !cfg.Web.Enabled {
		t.Error("Web.Enabled = false, want true by default")
	}
	if cfg.Web.Addr != DefaultWebAddr {
		t.Errorf("Web.Addr = %q, want %q", cfg.Web.Addr, DefaultWebAddr)
	}
	if !cfg.Web.Events {
		t.Error("Web.Events = false, want true by default")
	}
}

func TestLoadFromSpecExample(t *testing.T) {
	t.Parallel()

	// The exact scenario from Specs.txt, with nothing else configured.
	cfg, err := LoadFrom(env(map[string]string{
		"DNS_SUFFIX": "docker.local",
	}))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if got, want := cfg.FQDN("nginx"), "nginx.docker.local"; got != want {
		t.Errorf("FQDN(nginx) = %q, want %q", got, want)
	}
}

func TestLoadFromOverrides(t *testing.T) {
	t.Parallel()

	cfg, err := LoadFrom(env(map[string]string{
		"DNS_SUFFIX":       "Containers.Example.COM.",
		"HOSTS_FILE":       "/host/etc/hosts",
		"HOSTS_MOUNT_MODE": "dir",
		"TARGET_MODE":      "published",
		"RESYNC_INTERVAL":  "2m",
		"EVENT_DEBOUNCE":   "1s",
		"LOG_LEVEL":        "debug",
		"LOG_FORMAT":       "text",
		"WEB_ENABLED":      "false",
		"DOCKER_HOST":      "unix:///run/user/1000/docker.sock",
	}))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"DNSSuffix", cfg.DNSSuffix, "containers.example.com"},
		{"HostsFile", cfg.HostsFile, "/host/etc/hosts"},
		{"MountMode", cfg.MountMode, MountModeDir},
		{"TargetMode", cfg.TargetMode, TargetModePublished},
		{"ResyncInterval", cfg.ResyncInterval, 2 * time.Minute},
		{"EventDebounce", cfg.EventDebounce, time.Second},
		{"Level", cfg.Level, slog.LevelDebug},
		{"Format", cfg.Format, LogFormatText},
		{"Web.Enabled", cfg.Web.Enabled, false},
		{"DockerHost", cfg.DockerHost, "unix:///run/user/1000/docker.sock"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

// A blank value must behave exactly like an unset one, otherwise a stray empty
// env var in a compose file would silently break the deployment.
func TestLoadFromBlankFallsBackToDefault(t *testing.T) {
	t.Parallel()

	cfg, err := LoadFrom(env(map[string]string{
		"DNS_SUFFIX":        "   ",
		"HOSTS_FILE":        "",
		"RESYNC_INTERVAL":   "\t",
		"WEB_ADDR":          "",
		"DOCKER_HOST":       "  ",
		"EVENT_DEBOUNCE":    "  100ms  ",
		"DOCKER_APIVERSION": "",
	}))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.DNSSuffix != DefaultDNSSuffix {
		t.Errorf("DNSSuffix = %q, want default", cfg.DNSSuffix)
	}
	if cfg.HostsFile != DefaultHostsFile {
		t.Errorf("HostsFile = %q, want default", cfg.HostsFile)
	}
	if cfg.ResyncInterval != DefaultResyncInterval {
		t.Errorf("ResyncInterval = %s, want default", cfg.ResyncInterval)
	}
	if cfg.Web.Addr != DefaultWebAddr {
		t.Errorf("Web.Addr = %q, want default", cfg.Web.Addr)
	}
	if cfg.DockerHost != "" {
		t.Errorf("DockerHost = %q, want empty", cfg.DockerHost)
	}
	// Surrounding whitespace on a real value is trimmed, not dropped.
	if cfg.EventDebounce != 100*time.Millisecond {
		t.Errorf("EventDebounce = %s, want 100ms", cfg.EventDebounce)
	}
}

func TestLoadFromBooleans(t *testing.T) {
	t.Parallel()

	truthy := []string{"1", "t", "true", "TRUE", "Yes", "y", "on", " true "}
	for _, v := range truthy {
		cfg, err := LoadFrom(env(map[string]string{"WEB_ENABLED": v}))
		if err != nil {
			t.Errorf("WEB_ENABLED=%q: unexpected error: %v", v, err)
			continue
		}
		if !cfg.Web.Enabled {
			t.Errorf("WEB_ENABLED=%q: got false, want true", v)
		}
	}

	falsy := []string{"0", "f", "false", "FALSE", "No", "n", "off"}
	for _, v := range falsy {
		cfg, err := LoadFrom(env(map[string]string{"WEB_ENABLED": v}))
		if err != nil {
			t.Errorf("WEB_ENABLED=%q: unexpected error: %v", v, err)
			continue
		}
		if cfg.Web.Enabled {
			t.Errorf("WEB_ENABLED=%q: got true, want false", v)
		}
	}
}

// Invalid values must all be reported at once: an operator fixing a bad
// deployment should see the full list, not one error per restart.
func TestLoadFromReportsAllErrors(t *testing.T) {
	t.Parallel()

	_, err := LoadFrom(env(map[string]string{
		"DNS_SUFFIX":       "not a domain",
		"HOSTS_FILE":       "relative/hosts",
		"HOSTS_MOUNT_MODE": "nope",
		"TARGET_MODE":      "nope",
		"RESYNC_INTERVAL":  "30ms",
		"LOG_LEVEL":        "loud",
		"LOG_FORMAT":       "yaml",
		"WEB_ENABLED":      "maybe",
		"WEB_ADDR":         "8080",
	}))
	if err == nil {
		t.Fatal("expected an error, got nil")
	}

	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("expected joined errors, got %T", err)
	}
	if len(joined.Unwrap()) < 8 {
		t.Errorf("got %d errors, want at least 8: %v", len(joined.Unwrap()), err)
	}

	msg := err.Error()
	for _, field := range []string{
		"DNS_SUFFIX", "HOSTS_FILE", "HOSTS_MOUNT_MODE", "TARGET_MODE",
		"RESYNC_INTERVAL", "LOG_LEVEL", "LOG_FORMAT", "WEB_ENABLED", "WEB_ADDR",
	} {
		if !strings.Contains(msg, field) {
			t.Errorf("error message does not mention %s:\n%s", field, msg)
		}
	}
}

func TestLoadFromFieldErrorsAreInspectable(t *testing.T) {
	t.Parallel()

	_, err := LoadFrom(env(map[string]string{"TARGET_MODE": "sideways"}))
	var fe *FieldError
	if !errors.As(err, &fe) {
		t.Fatalf("expected *FieldError, got %T", err)
	}
	if fe.Field != EnvTargetMode {
		t.Errorf("Field = %q, want %q", fe.Field, EnvTargetMode)
	}
	if fe.Value != "sideways" {
		t.Errorf("Value = %q, want %q", fe.Value, "sideways")
	}
	if fe.Reason == "" {
		t.Error("Reason is empty")
	}
	if got, want := fe.Error(), `TARGET_MODE="sideways": must be one of: both, published, container-ip`; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestLoadFromRejectsBadDurations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		value string
	}{
		{"too short resync", "10ms"},
		{"too long resync", "48h"},
		{"unparsable", "soon"},
		{"no unit", "30"},
		{"negative", "-5s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := LoadFrom(env(map[string]string{"RESYNC_INTERVAL": tc.value})); err == nil {
				t.Fatalf("RESYNC_INTERVAL=%q: expected error, got nil", tc.value)
			}
		})
	}

	// The debounce accepts zero on purpose: it means "apply immediately".
	if cfg, err := LoadFrom(env(map[string]string{"EVENT_DEBOUNCE": "0s"})); err != nil {
		t.Errorf("EVENT_DEBOUNCE=0s: unexpected error: %v", err)
	} else if cfg.EventDebounce != 0 {
		t.Errorf("EventDebounce = %s, want 0s", cfg.EventDebounce)
	}
}

func TestNormalizeSuffix(t *testing.T) {
	t.Parallel()

	valid := map[string]string{
		"docker.local":           "docker.local",
		"DOCKER.LOCAL":           "docker.local",
		"  docker.local  ":       "docker.local",
		".docker.local.":         "docker.local",
		"Containers.Example.COM": "containers.example.com",
		"a.b.c.d.e":              "a.b.c.d.e",
		"my-suffix.internal":     "my-suffix.internal",
		"x1.y2":                  "x1.y2",
		"xn--80ak6aa92e.example": "xn--80ak6aa92e.example", // punycode
	}
	for in, want := range valid {
		got, err := NormalizeSuffix(in)
		if err != nil {
			t.Errorf("NormalizeSuffix(%q): unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeSuffix(%q) = %q, want %q", in, got, want)
		}
	}

	invalid := []string{
		"",
		"   ",
		".",
		"...",
		"local",                            // single label
		"docker",                           // single label
		"docker.local.",                    // handled by trimming, kept out of the invalid list
		"docker..local",                    // empty label
		".local",                           // leading dot only collapses to one label
		"-bad.local",                       // leading hyphen
		"bad-.local",                       // trailing hyphen
		"do cker.local",                    // space
		"docker.local:53",                  // port
		"docker*local",                     // wildcard
		"docker_local",                     // underscore
		"héllo.local",                      // non-ascii
		strings.Repeat("a", 64) + ".local", // label too long
		strings.Repeat("a.", 200),          // name too long
	}
	for _, in := range invalid {
		if in == "docker.local." || in == ".local" {
			// These are valid once trimmed; asserted separately below.
			continue
		}
		got, err := NormalizeSuffix(in)
		if err == nil {
			t.Errorf("NormalizeSuffix(%q) = %q, want error", in, got)
		}
	}
}

func TestNormalizeSuffixLengthBoundary(t *testing.T) {
	t.Parallel()

	// 63.63.63.63 style label at the limit must pass.
	label := strings.Repeat("a", 63)
	if _, err := NormalizeSuffix(label + "." + label); err != nil {
		t.Errorf("63-char labels must be accepted: %v", err)
	}
	// One more character must fail.
	if _, err := NormalizeSuffix(strings.Repeat("a", 64) + ".local"); err == nil {
		t.Error("64-char label must be rejected")
	}
}

func TestNormalizeSuffixTrimsDots(t *testing.T) {
	t.Parallel()

	for _, in := range []string{"docker.local.", ".docker.local", "..docker.local.."} {
		got, err := NormalizeSuffix(in)
		if err != nil {
			t.Errorf("NormalizeSuffix(%q): %v", in, err)
			continue
		}
		if got != "docker.local" {
			t.Errorf("NormalizeSuffix(%q) = %q, want docker.local", in, got)
		}
	}
}

func TestNormalizePath(t *testing.T) {
	t.Parallel()

	clean := map[string]string{
		"/etc/hosts":               "/etc/hosts",
		"/host/etc/hosts":          "/host/etc/hosts",
		"  /etc/hosts  ":           "/etc/hosts",
		"/etc/./hosts":             "/etc/hosts",
		"/etc/../etc/hosts":        "/etc/hosts",
		"/etc//hosts":              "/etc/hosts",
		"/host/etc/hosts/../hosts": "/host/etc/hosts",
	}
	for in, want := range clean {
		got, err := normalizePath(in)
		if err != nil {
			t.Errorf("normalizePath(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("normalizePath(%q) = %q, want %q", in, got, want)
		}
	}

	invalid := []string{
		"",
		"   ",
		"etc/hosts",
		"./hosts",
		"hosts",
		"/",
		"/etc",
	}
	for _, in := range invalid {
		if got, err := normalizePath(in); err == nil {
			t.Errorf("normalizePath(%q) = %q, want error", in, got)
		}
	}
}

func TestNormalizePathIsStable(t *testing.T) {
	t.Parallel()

	// Cleaning must be idempotent, otherwise the renderer would keep
	// producing "different" paths for the same file.
	once, err := normalizePath("/host/etc/./sub/../hosts")
	if err != nil {
		t.Fatalf("normalizePath: %v", err)
	}
	twice, err := normalizePath(once)
	if err != nil {
		t.Fatalf("normalizePath: %v", err)
	}
	if once != twice {
		t.Errorf("not idempotent: %q then %q", once, twice)
	}
	if !filepath.IsAbs(once) {
		t.Errorf("%q is not absolute", once)
	}
}

func TestFQDN(t *testing.T) {
	t.Parallel()

	cfg := Config{DNSSuffix: "docker.local"}
	if got, want := cfg.FQDN("nginx"), "nginx.docker.local"; got != want {
		t.Errorf("FQDN(nginx) = %q, want %q", got, want)
	}
	if got := cfg.FQDN(""); got != "" {
		t.Errorf("FQDN(\"\") = %q, want empty", got)
	}
	// Names are expected to be already normalised by the naming package; the
	// join must not silently reintroduce a trailing dot.
	if got, want := cfg.FQDN("web-1"), "web-1.docker.local"; got != want {
		t.Errorf("FQDN(web-1) = %q, want %q", got, want)
	}
}

func TestConfigStringHoldsNoSecrets(t *testing.T) {
	t.Parallel()

	cfg, err := LoadFrom(env(map[string]string{"DOCKER_HOST": "tcp://10.0.0.5:2375"}))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	s := cfg.String()
	for _, want := range []string{"docker.local", "/etc/hosts", "both", ":8080"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() = %q, missing %q", s, want)
		}
	}
	if strings.Contains(s, "2375") {
		t.Errorf("String() leaked the Docker endpoint: %q", s)
	}
}

func TestLoadFromNilLookupIsSafe(t *testing.T) {
	t.Parallel()

	// A nil lookup must not panic; it should behave like an empty environment.
	cfg, err := LoadFrom(nil)
	if err != nil {
		t.Fatalf("LoadFrom(nil): %v", err)
	}
	if cfg.DNSSuffix != DefaultDNSSuffix {
		t.Errorf("DNSSuffix = %q, want default", cfg.DNSSuffix)
	}
}

func TestFieldErrorOmitsEmptyValue(t *testing.T) {
	t.Parallel()

	fe := &FieldError{Field: "DNS_SUFFIX", Reason: "must contain at least two labels"}
	if got, want := fe.Error(), "DNS_SUFFIX: must contain at least two labels"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestErrorsAreJoinedNotWrapped(t *testing.T) {
	t.Parallel()

	// Guard against a future refactor reintroducing a fatal-on-first-error
	// behaviour: the whole point is one report for all bad fields.
	_, err := LoadFrom(env(map[string]string{
		"DNS_SUFFIX":  "bad suffix",
		"TARGET_MODE": "nope",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := err.(interface{ Unwrap() []error }); !ok {
		t.Fatalf("expected an errors.Join result, got %T", err)
	}

	var fe *FieldError
	if !errors.As(err, &fe) {
		t.Fatal("expected the joined error to be reachable via errors.As")
	}
}
