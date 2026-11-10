// Command qwatrix is a Matrix homeserver: one binary, one SQLite file, no external
// services.
//
// It reads its configuration from the environment (QWATRIX_*) and then from flags, so a
// container can be configured with the environment and a developer with flags, and the two
// never disagree silently.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/enimatek-nl/qwatrix/internal/app"
	"github.com/enimatek-nl/qwatrix/internal/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "qwatrix: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Default()

	// The environment first, so an explicit flag always wins over an inherited variable.
	if err := cfg.ApplyEnv(os.LookupEnv); err != nil {
		return err
	}

	// Flags are bound *after* the environment is applied, which makes each flag's default
	// the value the environment supplied. A flag the operator did not pass therefore never
	// overwrites an inherited variable, and the precedence order (default < env < flag)
	// needs no special casing anywhere else.
	flags := flag.NewFlagSet("qwatrix", flag.ContinueOnError)
	flags.Usage = func() {
		_, _ = fmt.Fprintf(flags.Output(), "qwatrix %s — a Matrix homeserver\n\n", cfg.Version)
		_, _ = fmt.Fprintf(flags.Output(), "Usage: qwatrix [flags]\n\n")
		flags.PrintDefaults()
	}
	cfg.ApplyFlags(flags)
	if err := flags.Parse(os.Args[1:]); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}

	setupLogging(cfg.LogLevel)

	instance, err := app.New(cfg)
	if err != nil {
		return err
	}
	defer func() {
		if err := instance.Close(); err != nil {
			slog.Error("closing the homeserver", "error", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("starting qwatrix",
		"version", cfg.Version,
		"server_name", cfg.ServerName,
		"data_dir", cfg.DataDir,
		"db", cfg.DatabaseFile(),
		"registration", cfg.EnableRegistration,
		"shared_secret_registration", cfg.RegistrationSecret != "",
		"default_room_version", cfg.DefaultRoomVersion,
	)

	if err := instance.Run(ctx); err != nil {
		return err
	}
	slog.Info("stopped")
	return nil
}

// setupLogging installs the process-wide logger at the requested level. Text output is
// deliberate: a homeserver's logs are read by a human in a terminal far more often than
// they are parsed.
func setupLogging(level string) {
	parsed := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		parsed = slog.LevelDebug
	case "warn", "warning":
		parsed = slog.LevelWarn
	case "error":
		parsed = slog.LevelError
	case "", "info":
	default:
		slog.Warn("unknown log level, using info", "log_level", level)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parsed})))
}
