// Command labview serves a console wall for a QEMU lab.
//
// One binary, one inventory directory, no database. See
// docs/design/labview.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/maglo/qemu-lab-manager/labview/internal/activity"
	"github.com/maglo/qemu-lab-manager/labview/internal/api"
	"github.com/maglo/qemu-lab-manager/labview/internal/config"
	"github.com/maglo/qemu-lab-manager/labview/internal/host"
	"github.com/maglo/qemu-lab-manager/labview/internal/inventory"
	"github.com/maglo/qemu-lab-manager/labview/internal/lease"
	"github.com/maglo/qemu-lab-manager/labview/internal/serial"
	"github.com/maglo/qemu-lab-manager/labview/internal/web"
)

// version is overridable at build time: -ldflags "-X main.version=..."
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "labview: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg := config.Default()

	fs := flag.NewFlagSet("labview", flag.ContinueOnError)
	config.Bind(fs, &cfg)
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "labview %s -- a console wall for a QEMU lab\n\nUsage:\n", version)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	level := slog.LevelInfo
	if cfg.Verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	// Signals first, so an inventory that takes a moment to load is still
	// interruptible.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The inventory must load before anything else: starting with no
	// machines because the operator fat-fingered a file would look like a
	// working but empty lab.
	watcher, err := inventory.NewWatcher(cfg.InventoryDir, cfg.InventoryRescan, log)
	if err != nil {
		return fmt.Errorf("inventory %s: %w", cfg.InventoryDir, err)
	}
	log.Info("inventory loaded",
		"dir", cfg.InventoryDir, "machines", watcher.Current().Len())

	hosts, err := newHostAccess(cfg, log)
	if err != nil {
		return err
	}
	defer hosts.Close()

	leases := lease.NewManager(lease.Options{
		IdleTimeout: cfg.LeaseIdle,
		WarnBefore:  cfg.LeaseWarn,
	})

	brokers := serial.NewManager(ctx, serial.ManagerConfig{
		RingBytes:       cfg.RingBytes,
		SubscriberQueue: cfg.SubscriberQueue,
		BackoffMin:      cfg.BackoffMin,
		BackoffMax:      cfg.BackoffMax,
		DialTimeout:     cfg.DialTimeout,
		Transcripts: serial.TranscriptPolicy{
			Dir:           cfg.TranscriptDir,
			MaxFiles:      cfg.TranscriptMaxFiles,
			MaxAge:        cfg.TranscriptMaxAge,
			MaxTotalBytes: cfg.TranscriptMaxBytes,
			MaxFileBytes:  cfg.TranscriptMaxRunSize,
		},
		Log: log,
	})

	ui, err := web.Handler()
	if err != nil {
		return fmt.Errorf("ui: %w", err)
	}

	srv := api.New(api.Deps{
		Config:    cfg,
		Inventory: watcher,
		Brokers:   brokers,
		Leases:    leases,
		Hosts:     hosts,
		Activity:  activity.New(cfg.ActivityCapacity, log),
		Log:       log,
		UI:        ui,
	})

	go watcher.Run(ctx)
	go leases.Run(ctx, cfg.LeaseSweep)

	// Brokers start eagerly, at process start, so a VM that reboots while
	// nobody is watching is still captured (design section 11).
	go brokers.Run(ctx, watcher)

	httpSrv := &http.Server{
		Addr:    cfg.Listen,
		Handler: srv,
		// No WriteTimeout: a websocket lives as long as the browser wants
		// it, and a wall tile is a long lived view-only session by design.
		// ReadHeaderTimeout still bounds a client that opens a connection
		// and says nothing.
		ReadHeaderTimeout: cfg.ReadTimeout,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ErrorLog:          nil,
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.Listen, err)
	}

	log.Info("labview listening",
		"address", ln.Addr().String(),
		"version", version,
		"machines", watcher.Current().Len(),
		"host_access", string(cfg.HostAccess),
		"tile_mode", string(cfg.TileMode),
		"recordings", cfg.TranscriptDir,
	)
	if cfg.TranscriptDir == "" {
		log.Warn("serial capture is disabled; no recordings will be written")
	}

	serveErr := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	// Stop accepting, let websockets finish, then stop the brokers so every
	// transcript is flushed and closed before the process exits.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown was not clean", "error", err)
	}
	brokers.Close()

	return nil
}

// newHostAccess builds the host access implementation the configuration asks
// for.
//
// The fake is never a fallback: it is selected explicitly, because a wall
// silently showing invented machine details would be worse than one that says
// it cannot reach the host.
func newHostAccess(cfg config.Config, log *slog.Logger) (host.Access, error) {
	switch cfg.HostAccess {
	case config.HostLocal:
		return host.NewLocal(host.LocalOptions{Log: log}), nil
	case config.HostFake:
		log.Warn("host access is faked; details and power operations are invented")
		return host.NewFake(), nil
	case config.HostNone:
		return host.NewUnavailable(), nil
	}
	return nil, fmt.Errorf("unknown host access mode %q", cfg.HostAccess)
}
