// Command bottero is a WhatsApp bot built on whatsmeow. In this stage it is an
// exploration tool: it pairs with a dedicated number, keeps the session on
// disk and logs every event whatsmeow delivers.
//
// Usage:
//
//	bottero pair -phone 549XXXXXXXXXX   link the number using a pairing code
//	bottero run                          connect and log events until stopped
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

	"github.com/ivanvallejoss/bottero/internal/eventlog"
	"github.com/ivanvallejoss/bottero/internal/session"
)

const defaultDataDir = "/var/lib/bottero"

// Exit codes. The systemd unit restarts the service on failure, except on
// exitSessionEnded (RestartPreventExitStatus=3): without a valid session, or
// after WhatsApp ended it, restarting only hits the server again.
const (
	exitError        = 1
	exitUsage        = 2
	exitSessionEnded = 3
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
		if errors.Is(err, session.ErrSessionEnded) || errors.Is(err, session.ErrNotPaired) {
			os.Exit(exitSessionEnded)
		}
		os.Exit(exitError)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `Usage:
  bottero pair -phone 549XXXXXXXXXX [-data-dir DIR]
  bottero run [-data-dir DIR]

Commands:
  pair   link the dedicated number as a companion device using a pairing code
  run    connect with the stored session and log every whatsmeow event
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
	dataDir := fs.String("data-dir", defaultDataDir, "directory that holds the SQLite databases")
	_ = fs.Parse(args)

	logger.Info("starting", "component", "bottero", "data_dir", *dataDir, "whatsmeow", whatsmeowVersion())

	waLogger := eventlog.WhatsmeowLogger(logger)
	container, err := session.OpenStore(ctx, *dataDir, waLogger.Sub("Database"))
	if err != nil {
		return err
	}
	defer container.Close()

	return session.Run(ctx, container, waLogger, logger)
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
