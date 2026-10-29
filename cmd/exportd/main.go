// Command exportd runs the go-partner-export service: it builds the daily
// partner file, archives it, pushes it over SFTP and records the run.
//
// Without flags it serves the HTTP API and triggers the export daily at
// PX_REPORT_SCHEDULE_AT. With --once it exports one day and exits, which
// is the shape a Kubernetes CronJob wants.
package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"flag"
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
	"github.com/Nguyenngoctruong274/go-partner-export/internal/scheduler"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/sftp"
)

var errDraining = errors.New("shutting down")

func main() {
	once := flag.Bool("once", false, "export one day and exit (for CronJobs)")
	date := flag.String("date", "", "with --once: the day to export as YYYY-MM-DD; default yesterday in the report zone")
	repush := flag.Bool("repush", false, "with --once: re-send the archived file of --date instead of rebuilding it")
	flag.Parse()

	if err := run(*once, *date, *repush); err != nil {
		fmt.Fprintln(os.Stderr, "exportd:", err)
		os.Exit(1)
	}
}

// deps is everything built from the configuration.
type deps struct {
	cfg    config.Config
	logger *slog.Logger
	db     *sql.DB
	pipe   *pipeline.Pipeline
	runs   *postgres.RunStore
	blob   *httpapi.Blob
}

func run(once bool, date string, repush bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	logConfig(logger, cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	startCtx, cancelStart := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStart()
	d, err := build(startCtx, cfg, logger)
	if err != nil {
		return err
	}
	defer d.db.Close()

	if once {
		return runOnce(ctx, d, date, repush)
	}
	return serve(ctx, d)
}

// runOnce exports a single day in the foreground. The exit code tells the
// CronJob whether to count a failure.
func runOnce(ctx context.Context, d *deps, date string, repush bool) error {
	loc := d.cfg.Report.Location()
	day := export.ReportDay(time.Now(), loc)
	if date != "" {
		var err error
		if day, err = export.ParseDay(date); err != nil {
			return err
		}
	}
	if today := export.DayOf(time.Now(), loc); !day.Before(today) {
		return fmt.Errorf("export %s: the day is not over yet in %s", day, loc)
	}
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	var (
		r   export.Run
		err error
	)
	if repush {
		r, err = d.pipe.Repush(runCtx, day)
	} else {
		r, err = d.pipe.Run(runCtx, day)
	}
	if err != nil {
		return fmt.Errorf("export %s: %w", day, err)
	}
	d.logger.Info("done", slog.String("day", day.String()), slog.Int("rows", r.Rows), slog.String("remote", r.RemotePath))
	return nil
}

// serve runs the API and the daily scheduler until a signal arrives.
func serve(ctx context.Context, d *deps) error {
	cfg, logger := d.cfg, d.logger

	sched, err := scheduler.New(d.pipe, logger, scheduler.Options{
		Location: cfg.Report.Location(), Hour: cfg.Report.ScheduleHour, Minute: cfg.Report.ScheduleMinute,
	})
	if err != nil {
		return err
	}
	schedCtx, stopSched := context.WithCancel(ctx)
	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		_ = sched.Run(schedCtx)
	}()
	defer func() {
		stopSched()
		<-schedDone
		logger.Info("scheduler stopped")
	}()

	// Readiness turns false the moment shutdown starts, so load balancers
	// stop routing new requests while in-flight ones finish.
	var draining atomic.Bool
	ready := func(ctx context.Context) error {
		if draining.Load() {
			return errDraining
		}
		return d.db.PingContext(ctx)
	}
	handler := httpapi.NewHandler(logger, httpapi.Options{
		Ready: ready,
		Blob:  d.blob,
		Exports: &httpapi.ExportOptions{
			Exports:  exportsAdapter{pipe: d.pipe, runs: d.runs},
			Location: cfg.Report.Location(),
		},
	})
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
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
	// Exports started over HTTP run on their own context; give them the
	// rest of the shutdown budget before the database handle closes.
	if err := handler.Drain(shutdownCtx); err != nil {
		logger.Warn("exports still running at shutdown", slog.Any("error", err))
	}
	return nil
}

// build wires every dependency from the configuration.
func build(ctx context.Context, cfg config.Config, logger *slog.Logger) (*deps, error) {
	db, err := postgres.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return nil, err
	}
	d, err := buildWithDB(ctx, cfg, logger, db)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return d, nil
}

func buildWithDB(ctx context.Context, cfg config.Config, logger *slog.Logger, db *sql.DB) (*deps, error) {
	// Instances starting together serialize on an advisory lock, so the
	// schema is migrated once and nobody serves before it is current.
	if err := postgres.Migrate(ctx, db); err != nil {
		return nil, err
	}
	logger.Info("database ready", slog.Int("max_conns", cfg.DBMaxConns))

	store, signer, blobAPI, err := buildBlob(ctx, cfg.Blob, logger)
	if err != nil {
		return nil, err
	}
	sftpClient, err := buildSFTP(cfg.SFTP)
	if err != nil {
		return nil, err
	}
	// A wrong password or a changed host key shows up at startup, not at
	// 01:00 when the report is due. Connectivity is only logged, so a
	// partner outage does not keep the API from serving.
	if err := sftpClient.Ping(ctx); err != nil {
		logger.Warn("partner SFTP unreachable at startup", slog.Any("error", err))
	} else {
		logger.Info("partner SFTP reachable", slog.String("addr", cfg.SFTP.Addr()))
	}

	runs := postgres.NewRunStore(db)
	pipe, err := pipeline.New(postgres.NewTripStore(db), runs, store, signer, sftpClient, logger, pipeline.Options{
		Naming:       export.Naming{Prefix: cfg.Report.FilePrefix, Version: cfg.Report.FileVersion},
		Format:       export.Format{Delimiter: cfg.Report.CSVDelimiter},
		Location:     cfg.Report.Location(),
		BlobPrefix:   cfg.Blob.Prefix,
		RemoteDir:    cfg.SFTP.RemoteDir,
		SignedURLTTL: cfg.Blob.SignedURLTTL,
		Retry:        retry.Policy{Attempts: cfg.Retry.Attempts, Backoff: cfg.Retry.Backoff, Jitter: 0.2},
	})
	if err != nil {
		return nil, err
	}
	return &deps{cfg: cfg, logger: logger, db: db, pipe: pipe, runs: runs, blob: blobAPI}, nil
}

func logConfig(logger *slog.Logger, cfg config.Config) {
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
}

// exportsAdapter joins the pipeline and the run store into what the API needs.
type exportsAdapter struct {
	pipe *pipeline.Pipeline
	runs *postgres.RunStore
}

func (e exportsAdapter) Run(ctx context.Context, day export.Day) (export.Run, error) {
	return e.pipe.Run(ctx, day)
}

func (e exportsAdapter) Repush(ctx context.Context, day export.Day) (export.Run, error) {
	return e.pipe.Repush(ctx, day)
}

func (e exportsAdapter) Get(ctx context.Context, day export.Day) (export.Run, error) {
	return e.runs.Get(ctx, day)
}

func (e exportsAdapter) List(ctx context.Context, state export.State, before export.Day, limit int) ([]export.Run, error) {
	return e.runs.List(ctx, postgres.ListFilter{State: state, Before: before, Limit: limit})
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
