// Command exportd runs the go-partner-export service: it builds the daily
// partner file, archives it, pushes it over SFTP and records the run.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Nguyenngoctruong274/go-partner-export/internal/blob"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/config"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/export"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/httpapi"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/pipeline"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/postgres"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/retry"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/sftp"
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

	store, signer, blobAPI, err := buildBlob(startCtx, cfg.Blob, logger)
	if err != nil {
		return err
	}
	sftpClient, err := buildSFTP(cfg.SFTP)
	if err != nil {
		return err
	}
	// A wrong password or a changed host key shows up at startup, not at
	// 01:00 when the report is due. Connectivity is only logged, so a
	// partner outage does not keep the API from serving.
	if err := sftpClient.Ping(startCtx); err != nil {
		logger.Warn("partner SFTP unreachable at startup", slog.Any("error", err))
	} else {
		logger.Info("partner SFTP reachable", slog.String("addr", cfg.SFTP.Addr()))
	}
	pipe, err := pipeline.New(postgres.NewTripStore(db), postgres.NewRunStore(db), store, signer, sftpClient, logger, pipeline.Options{
		Naming:       export.Naming{Prefix: cfg.Report.FilePrefix, Version: cfg.Report.FileVersion},
		Format:       export.Format{Delimiter: cfg.Report.CSVDelimiter},
		Location:     cfg.Report.Location(),
		BlobPrefix:   cfg.Blob.Prefix,
		RemoteDir:    cfg.SFTP.RemoteDir,
		SignedURLTTL: cfg.Blob.SignedURLTTL,
		Retry:        retry.Policy{Attempts: cfg.Retry.Attempts, Backoff: cfg.Retry.Backoff, Jitter: 0.2},
	})
	if err != nil {
		return err
	}
	_ = pipe // driven by the scheduler and the API on day 8

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
		Handler:           httpapi.NewHandler(logger, httpapi.Options{Ready: ready, Blob: blobAPI}),
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

// buildBlob wires the archive store and the link signer for the configured
// driver. With the fs driver, exportd serves the links itself.
func buildBlob(ctx context.Context, cfg config.Blob, logger *slog.Logger) (blob.Store, blob.Signer, *httpapi.Blob, error) {
	switch cfg.Driver {
	case config.BlobDriverGCS:
		store, err := blob.NewGCS(ctx, blob.GCSOptions{
			Bucket: cfg.Bucket, Endpoint: cfg.GCSEndpoint, CredentialsFile: cfg.GCSCredentialsFile,
		})
		if err != nil {
			return nil, nil, nil, err
		}
		signer, err := blob.NewGCSSignerFromFile(cfg.GCSSignerKeyFile, cfg.Bucket, cfg.GCSEndpoint)
		if err != nil {
			return nil, nil, nil, err
		}
		return store, signer, nil, nil
	default:
		store, err := blob.NewFS(cfg.FSRoot)
		if err != nil {
			return nil, nil, nil, err
		}
		secret := []byte(cfg.FSSigningSecret)
		if len(secret) == 0 {
			secret = make([]byte, 32)
			if _, err := rand.Read(secret); err != nil {
				return nil, nil, nil, fmt.Errorf("generate signing secret: %w", err)
			}
			logger.Warn("PX_FS_SIGNING_SECRET is empty; download links will stop working when this process restarts")
		}
		signer, err := blob.NewHMACSigner(cfg.PublicURL, secret)
		if err != nil {
			return nil, nil, nil, err
		}
		return store, signer, &httpapi.Blob{Store: store, Verifier: signer}, nil
	}
}

// buildSFTP reads the key file, if any, and prepares the client.
func buildSFTP(cfg config.SFTP) (*sftp.Client, error) {
	opts := sftp.Options{
		Addr: cfg.Addr(), User: cfg.User,
		KnownHostsFile: cfg.KnownHostsFile, InsecureSkipHostKey: cfg.InsecureSkipHostKey,
		Timeout: cfg.Timeout,
	}
	switch cfg.Auth {
	case config.SFTPAuthPassword:
		opts.Password = cfg.Password
	default:
		key, err := os.ReadFile(cfg.PrivateKeyFile) // #nosec G304 -- path comes from configuration
		if err != nil {
			return nil, fmt.Errorf("read sftp private key: %w", err)
		}
		opts.PrivateKeyPEM = key
	}
	return sftp.New(opts)
}
