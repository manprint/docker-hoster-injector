// Command docker-hoster-injector keeps the host's /etc/hosts in sync with the
// running Docker containers, so that every container is reachable under
// <name>.<DNS_SUFFIX> from the host.
//
// It observes the Docker event stream for reactivity and reconciles fully on a
// timer for correctness, because events can be missed and a stream can drop.
// When it stops it removes its records again; "docker-hoster-injector clean"
// does the same after a forced kill, which no process can clean up after, and
// "docker-hoster-injector healthcheck" is what the image's HEALTHCHECK runs.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/manprint/docker-hoster-injector/internal/agent"
	"github.com/manprint/docker-hoster-injector/internal/config"
	"github.com/manprint/docker-hoster-injector/internal/logging"
)

// shutdownTimeout is how long a stop may take before the process gives up and
// exits. It stays under Docker's default stop grace period of 10 seconds, so
// the exit is ours and not the SIGKILL that would follow it.
const shutdownTimeout = 8 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "docker-hoster-injector: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "run"
	switch len(args) {
	case 0:
	case 1:
		command = args[0]
	default:
		return fmt.Errorf("usage: docker-hoster-injector [run|clean|healthcheck|version]")
	}

	if command == "version" {
		fmt.Println(version)
		return nil
	}
	if command != "run" && command != "clean" && command != "healthcheck" {
		return fmt.Errorf("unknown command %q (want run, clean, healthcheck or version)", command)
	}

	cfg, err := config.Load()
	if err != nil {
		// The logger does not exist yet, so this goes straight to stderr.
		// Reporting every bad field at once is the whole point of the joined
		// error: an operator should not have to restart to find the next one.
		return err
	}
	log := logging.New(cfg, logging.Options{Service: "docker-hoster-injector"})

	switch command {
	case "clean":
		return agent.Clean(cfg, log)
	case "healthcheck":
		return agent.HealthCheck(context.Background(), cfg)
	}

	// SIGHUP is a stop as well: it is what a closing terminal or a service
	// manager sends, and the default action would kill the process without
	// cleaning up. The handler stays installed until run returns, so a second
	// signal during cleanup is absorbed instead of cutting it short.
	ctx, stop := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	// A stop that does not complete is abandoned rather than left to be
	// SIGKILLed half way through a write.
	go func() {
		<-ctx.Done()
		time.Sleep(shutdownTimeout)
		fmt.Fprintln(os.Stderr, "docker-hoster-injector: shutdown timed out, exiting")
		os.Exit(1)
	}()

	log.Info("starting", "version", version, "config", cfg.String(), "pid", os.Getpid())

	a := agent.New(cfg, agent.Options{Version: version, Logger: log})
	if err := a.Run(ctx); err != nil {
		log.Error("the agent stopped with an error", "error", err)
		return err
	}
	log.Info("stopped cleanly")
	return nil
}

// version is overridden at build time with -X main.version=<semver>.
var version = "dev"
