// Command bottero is a WhatsApp bot built on whatsmeow. It pairs with a
// dedicated number, keeps the session on disk, logs every event whatsmeow
// delivers and answers keywords with fixed replies.
//
// Usage:
//
//	bottero pair -phone 549XXXXXXXXXX   link the number using a pairing code
//	bottero run                          connect, log events and answer until stopped
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/ivanvallejoss/bottero/internal/config"
	"github.com/ivanvallejoss/bottero/internal/dedup"
	"github.com/ivanvallejoss/bottero/internal/eventlog"
	"github.com/ivanvallejoss/bottero/internal/session"
)

const (
	defaultDataDir = "/var/lib/bottero"
	defaultConfig  = "/etc/bottero/config.json"
)

// Exit codes. The systemd unit restarts the service on failure, except on
// exitSessionEnded and exitConfig (RestartPreventExitStatus=3 4): without a
// valid session, after WhatsApp ended it, or with a broken configuration,
// restarting does not help.
const (
	exitError        = 1
	exitUsage        = 2
	exitSessionEnded = 3
	exitConfig       = 4
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if len(os.Args) < 2 {
		usage()
		os.Exit(exitUsage)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "pair":
		err = pairCmd(ctx, logger, os.Args[2:])
	case "run":
		err = runCmd(ctx, logger, os.Args[2:])
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(exitUsage)
	}

	if err != nil {
		logger.Error("exiting with error", "component", "bottero", "err", err)
		stop()
		switch {
		case errors.Is(err, session.ErrSessionEnded), errors.Is(err, session.ErrNotPaired):
			os.Exit(exitSessionEnded)
		case errors.Is(err, config.ErrInvalid):
			os.Exit(exitConfig)
		default:
			os.Exit(exitError)
		}
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Usage:
  bottero pair -phone 549XXXXXXXXXX [-data-dir DIR]
  bottero run [-config FILE] [-data-dir DIR]

Commands:
  pair   link the dedicated number as a companion device using a pairing code
  run    connect with the stored session, log every event and answer keywords
`)
}

func pairCmd(ctx context.Context, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	phone := fs.String("phone", "", "dedicated number in international format, digits only (Argentina: 549 + area code + number)")
	dataDir := fs.String("data-dir", defaultDataDir, "directory that holds the SQLite databases")
	_ = fs.Parse(args)

	if *phone == "" {
		return errors.New("-phone is required")
	}

	waLogger := eventlog.WhatsmeowLogger(logger)
	container, err := session.OpenStore(ctx, *dataDir, waLogger.Sub("Database"))
	if err != nil {
		return err
	}
	defer container.Close()

	err = session.Pair(ctx, container, *phone, waLogger, func(code string) {
		fmt.Printf("\nPairing code: %s\n", code)
		fmt.Println("On the phone: Linked devices > Link a device > Link with phone number instead.")
		fmt.Println("The code is valid for about two minutes.")
		fmt.Println()
	})
	if err != nil {
		return err
	}

	logger.Info("paired; the session is stored. Start the bot with 'bottero run' (the first connection uploads encryption keys and can take a while)", "component", "bottero")
	return nil
}

func runCmd(ctx context.Context, logger *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("config", defaultConfig, "configuration file (keywords, allowed people, enabled groups)")
	dataDir := fs.String("data-dir", defaultDataDir, "directory that holds the SQLite databases")
	_ = fs.Parse(args)

	logger.Info("starting", "component", "bottero", "data_dir", *dataDir, "config", *configPath, "whatsmeow", whatsmeowVersion())

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	keywords, people, groups := cfg.Summary()
	logger.Info("configuration loaded", "component", "bottero", "keywords", keywords, "allowed_people", people, "enabled_groups", groups)

	answered, err := dedup.Open(ctx, *dataDir)
	if err != nil {
		return err
	}
	defer answered.Close()

	waLogger := eventlog.WhatsmeowLogger(logger)
	container, err := session.OpenStore(ctx, *dataDir, waLogger.Sub("Database"))
	if err != nil {
		return err
	}
	defer container.Close()

	return session.Run(ctx, container, waLogger, logger, cfg, answered)
}

// whatsmeowVersion reports the whatsmeow module version compiled into the
// binary, so the log shows which pinned commit is running on the homelab.
func whatsmeowVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	for _, dep := range info.Deps {
		if dep.Path == "go.mau.fi/whatsmeow" {
			return dep.Version
		}
	}
	return "unknown"
}
