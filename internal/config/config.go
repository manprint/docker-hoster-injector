// Package config loads and validates the runtime configuration of
// docker-hoster-injector from environment variables.
//
// Every value has a safe default, so running the container with no
// environment at all behaves as described in the README: serve "docker.local".
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// Environment variable names. They are exported so that tests, the web UI
// and the documentation never drift apart.
const (
	EnvDNSSuffix      = "DNS_SUFFIX"
	EnvHostsFile      = "HOSTS_FILE"
	EnvMountMode      = "HOSTS_MOUNT_MODE"
	EnvTargetMode     = "TARGET_MODE"
	EnvResyncInterval = "RESYNC_INTERVAL"
	EnvEventDebounce  = "EVENT_DEBOUNCE"
	EnvLogLevel       = "LOG_LEVEL"
	EnvLogFormat      = "LOG_FORMAT"
	EnvWebEnabled     = "WEB_ENABLED"
	EnvWebAddr        = "WEB_ADDR"
	EnvWebEvents      = "WEB_EVENTS"

	// Consumed by the Docker client.
	EnvDockerHost       = "DOCKER_HOST"
	EnvDockerAPIVersion = "DOCKER_API_VERSION"
)

// Defaults.
const (
	DefaultDNSSuffix      = "docker.local"
	DefaultHostsFile      = "/etc/hosts"
	DefaultResyncInterval = 30 * time.Second
	DefaultEventDebounce  = 250 * time.Millisecond
	DefaultWebAddr        = ":8080"

	// Bounds that reject absurd values which would make the agent either
	// hammer the Docker daemon or stop reacting to container events.
	MinResyncInterval = time.Second
	MaxResyncInterval = 24 * time.Hour
	MinEventDebounce  = 0
	MaxEventDebounce  = 30 * time.Second
)

// MountMode selects how the hosts file is made available inside the
// container, which in turn decides how atomically we can rewrite it.
type MountMode string

const (
	// MountModeFile mounts the single file /etc/hosts. Least privilege, but
	// rename(2) on a mount point returns EBUSY, so writes must be in-place.
	MountModeFile MountMode = "file"

	// MountModeDir mounts the whole directory holding the hosts file
	// (e.g. /etc:/host/etc). Allows a fully atomic rename-based rewrite.
	MountModeDir MountMode = "dir"
)

// TargetMode decides which addresses are published for a container name.
//
// The names under DNS_SUFFIX must behave like published ports, so that a
// container started with "-p 8080:80 nginx" is reachable as
// "nginx.docker.local:8080". Resolving the name to a host-side address is
// what makes the host port mapping apply.
type TargetMode string

const (
	// TargetModeBoth publishes the host-reachable address first, then the
	// container address. Default: satisfies the spec and still allows
	// direct access to the container's own ports.
	TargetModeBoth TargetMode = "both"

	// TargetModePublished publishes only host-reachable addresses, i.e. the
	// bind address of the published ports (127.0.0.1 when unspecified).
	TargetModePublished TargetMode = "published"

	// TargetModeContainerIP publishes only container addresses. Use it when
	// the records must resolve from other machines on the LAN.
	TargetModeContainerIP TargetMode = "container-ip"
)

// LogFormat selects the encoding of the structured logger.
type LogFormat string

const (
	// LogFormatJSON writes one JSON object per line, for log collectors.
	LogFormatJSON LogFormat = "json"
	// LogFormatText writes key=value lines, for a person reading a terminal.
	LogFormatText LogFormat = "text"
)

// Web groups the settings of the optional monitoring web server.
type Web struct {
	// Enabled turns the HTTP server on. It only exposes read-only endpoints.
	Enabled bool
	// Addr is the listen address inside the container, e.g. ":8080".
	Addr string
	// Events enables the Server-Sent Events push channel. When false the
	// browser falls back to periodic polling of /api/entries.
	Events bool
}

// Known reports whether the mode is one the package understands. An unknown
// mode must never be silently treated as the default, because the two
// supported modes differ in their write strategy.
func (m MountMode) Known() bool {
	switch m {
	case MountModeFile, MountModeDir:
		return true
	default:
		return false
	}
}

// Config is the fully validated configuration.
type Config struct {
	// DNSSuffix is the normalised base domain: lowercase, no trailing dot,
	// at least two labels.
	DNSSuffix string

	// HostsFile is the absolute, cleaned path of the hosts file to manage.
	HostsFile string

	// MountMode mirrors how the hosts file is mounted into the container.
	MountMode MountMode

	// TargetMode selects which addresses are published per name.
	TargetMode TargetMode

	// ResyncInterval is the period of the full reconciliation that repairs
	// whatever the Docker event stream missed.
	ResyncInterval time.Duration

	// EventDebounce coalesces bursts of Docker events into one rewrite.
	EventDebounce time.Duration

	// Level and Format configure the structured logger.
	Level  slog.Level
	Format LogFormat

	// Web configures the monitoring HTTP server.
	Web Web

	// DockerHost and DockerAPIVersion are passed to the Docker client.
	// Empty means "let the client decide", which is what makes rootless
	// Docker work without extra configuration.
	DockerHost       string
	DockerAPIVersion string
}

// FieldError reports a single invalid configuration field. Errors are joined
// so that every problem surfaces at once instead of one per run.
type FieldError struct {
	Field  string
	Value  string
	Reason string
}

func (e *FieldError) Error() string {
	if e.Value == "" {
		return fmt.Sprintf("%s: %s", e.Field, e.Reason)
	}
	return fmt.Sprintf("%s=%q: %s", e.Field, e.Value, e.Reason)
}

// LookupFunc mirrors os.LookupEnv and is injectable for tests.
type LookupFunc func(key string) (string, bool)

// Load reads the configuration from the process environment.
func Load() (Config, error) {
	return LoadFrom(os.LookupEnv)
}

// LoadFrom builds a Config from an arbitrary environment lookup function.
// Empty, blank and unset values fall back to the documented defaults.
func LoadFrom(lookup LookupFunc) (Config, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	env := environ{lookup: lookup}

	cfg := Config{
		DockerHost:       env.text(EnvDockerHost),
		DockerAPIVersion: env.text(EnvDockerAPIVersion),
	}

	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	// --- DNS_SUFFIX -----------------------------------------------------
	suffix, err := NormalizeSuffix(env.textOr(EnvDNSSuffix, DefaultDNSSuffix))
	if err != nil {
		add(&FieldError{EnvDNSSuffix, suffix, err.Error()})
	} else {
		cfg.DNSSuffix = suffix
	}

	// --- HOSTS_FILE -----------------------------------------------------
	path, err := normalizePath(env.textOr(EnvHostsFile, DefaultHostsFile))
	if err != nil {
		add(&FieldError{EnvHostsFile, path, err.Error()})
	} else {
		cfg.HostsFile = path
	}

	// --- HOSTS_MOUNT_MODE -----------------------------------------------
	switch mode := MountMode(env.textOr(EnvMountMode, string(MountModeFile))); mode {
	case MountModeFile, MountModeDir:
		cfg.MountMode = mode
	default:
		add(&FieldError{EnvMountMode, string(mode), "must be one of: file, dir"})
	}

	// --- TARGET_MODE ----------------------------------------------------
	switch mode := TargetMode(env.textOr(EnvTargetMode, string(TargetModeBoth))); mode {
	case TargetModeBoth, TargetModePublished, TargetModeContainerIP:
		cfg.TargetMode = mode
	default:
		add(&FieldError{EnvTargetMode, string(mode), "must be one of: both, published, container-ip"})
	}

	// --- Durations ------------------------------------------------------
	if cfg.ResyncInterval, err = env.duration(EnvResyncInterval, MinResyncInterval, MaxResyncInterval); err != nil {
		add(err)
	}
	if cfg.EventDebounce, err = env.duration(EnvEventDebounce, MinEventDebounce, MaxEventDebounce); err != nil {
		add(err)
	}

	// --- Logger ---------------------------------------------------------
	if cfg.Level, err = env.logLevel(); err != nil {
		add(err)
	}
	switch format := LogFormat(env.textOr(EnvLogFormat, string(LogFormatJSON))); format {
	case LogFormatJSON, LogFormatText:
		cfg.Format = format
	default:
		add(&FieldError{EnvLogFormat, string(format), "must be one of: json, text"})
	}

	// --- Web ------------------------------------------------------------
	// Parsed even when disabled, so that a typo is reported at startup
	// rather than the day someone flips WEB_ENABLED to true.
	if cfg.Web.Enabled, err = env.boolean(EnvWebEnabled, true); err != nil {
		add(err)
	}
	if cfg.Web.Events, err = env.boolean(EnvWebEvents, true); err != nil {
		add(err)
	}
	if cfg.Web.Addr, err = env.listenAddr(EnvWebAddr); err != nil {
		add(err)
	}

	return cfg, errors.Join(errs...)
}

// FQDN joins name and the configured suffix, e.g. "nginx" + "docker.local".
func (c Config) FQDN(name string) string {
	if name == "" {
		return ""
	}
	return name + "." + c.DNSSuffix
}

// String renders the configuration for the startup log. It holds no secrets,
// so it is safe to log verbatim.
func (c Config) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "dns_suffix=%s hosts_file=%s mount_mode=%s target_mode=%s resync=%s debounce=%s",
		c.DNSSuffix, c.HostsFile, c.MountMode, c.TargetMode, c.ResyncInterval, c.EventDebounce)
	if c.Web.Enabled {
		fmt.Fprintf(&b, " web=%s events=%t", c.Web.Addr, c.Web.Events)
	} else {
		b.WriteString(" web=disabled")
	}
	return b.String()
}

// --- environ: typed accessors with validation ---------------------------

type environ struct {
	lookup LookupFunc
}

// raw returns the trimmed value and whether it was set to a non-blank
// string. Treating a blank value as unset is what lets a compose file carry
// "DNS_SUFFIX: ${DNS_SUFFIX:-}" without breaking the deployment.
func (e environ) raw(key string) (string, bool) {
	v, ok := e.lookup(key)
	if !ok {
		return "", false
	}
	v = strings.TrimSpace(v)
	return v, v != ""
}

func (e environ) text(key string) string {
	v, _ := e.raw(key)
	return v
}

func (e environ) textOr(key, def string) string {
	if v, ok := e.raw(key); ok {
		return v
	}
	return def
}

func (e environ) duration(key string, min, max time.Duration) (time.Duration, error) {
	def := min
	switch key {
	case EnvResyncInterval:
		def = DefaultResyncInterval
	case EnvEventDebounce:
		def = DefaultEventDebounce
	}
	raw := e.textOr(key, def.String())
	d, err := time.ParseDuration(raw)
	if err != nil {
		return def, &FieldError{key, raw, "not a valid duration, expected e.g. 30s, 250ms or 5m"}
	}
	if d < min || d > max {
		return def, &FieldError{key, raw, fmt.Sprintf("must be between %s and %s", min, max)}
	}
	return d, nil
}

func (e environ) boolean(key string, def bool) (bool, error) {
	raw, ok := e.raw(key)
	if !ok {
		return def, nil
	}
	switch strings.ToLower(raw) {
	case "1", "t", "true", "yes", "y", "on":
		return true, nil
	case "0", "f", "false", "no", "n", "off":
		return false, nil
	default:
		return def, &FieldError{key, raw, "must be a boolean: true or false"}
	}
}

func (e environ) logLevel() (slog.Level, error) {
	raw := e.textOr(EnvLogLevel, "info")
	switch strings.ToLower(raw) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, &FieldError{EnvLogLevel, raw,
			"must be one of: debug, info, warn, error"}
	}
}

func (e environ) listenAddr(key string) (string, error) {
	raw := e.textOr(key, DefaultWebAddr)
	// Accept ":8080", "0.0.0.0:8080", "127.0.0.1:8080" and "[::1]:8080".
	if !strings.Contains(raw, ":") {
		return DefaultWebAddr, &FieldError{key, raw, "missing port, expected host:port such as \":8080\""}
	}
	return raw, nil
}
