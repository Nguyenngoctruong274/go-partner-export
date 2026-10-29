// Command exportd runs the go-partner-export service: it builds the daily
// partner file, archives it, pushes it over SFTP and records the run.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Nguyenngoctruong274/go-partner-export/internal/config"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/httpapi"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/postgres"
)

var errDraining = errors.New("shutting down")

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "exportd:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	// Log the shape of the deployment, never the secrets in it.
	logger.Info("configuration loaded",
		slog.String("report_timezone", cfg.Report.Timezone),
		slog.String("report_schedule_at", fmt.Sprintf("%02d:%02d", cfg.Report.ScheduleHour, cfg.Report.ScheduleMinute)),
		slog.String("file_pattern", cfg.Report.FilePrefix+"_"+cfg.Report.FileVersion+"_YYYYMMDD_NNN.csv"),
		slog.String("blob_driver", cfg.Blob.Driver),
		slog.String("blob_bucket", cfg.Blob.Bucket),
		slog.String("sftp_addr", cfg.SFTP.Addr()),
		slog.String("sftp_auth", cfg.SFTP.Auth),
		slog.Bool("sftp_host_key_pinned", !cfg.SFTP.InsecureSkipHostKey),
	)
	if cfg.SFTP.InsecureSkipHostKey {
		logger.Warn("SFTP host key verification is disabled; use PX_SFTP_KNOWN_HOSTS_FILE outside local development")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startCtx, cancelStart := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStart()
	db, err := postgres.Open(startCtx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer db.Close()
	// Instances starting together serialize on an advisory lock, so the
	// schema is migrated once and nobody serves before it is current.
	if err := postgres.Migrate(startCtx, db); err != nil {
		return err
	}
	logger.Info("database ready", slog.Int("max_conns", cfg.DBMaxConns))

	// Readiness turns false the moment shutdown starts, so load balancers
	// stop routing new requests while in-flight ones finish.
	var draining atomic.Bool
	ready := func(ctx context.Context) error {
		if draining.Load() {
			return errDraining
		}
		return db.PingContext(ctx)
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpapi.NewHandler(logger, httpapi.Options{Ready: ready}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		logger.Info("listening", slog.String("addr", cfg.HTTPAddr))
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
		close(errc)
	}()

	select {
	case err := <-errc:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutdown started", slog.Duration("drain", cfg.ShutdownDrain))
	draining.Store(true)
	time.Sleep(cfg.ShutdownDrain)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}
	logger.Info("http server stopped")
	return nil
}
