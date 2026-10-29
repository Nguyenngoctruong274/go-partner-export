// Package config loads runtime configuration for exportd from the environment.
//
// Every variable is prefixed PX_. Secrets (database URL, SFTP password) are
// validated but never echoed back in error messages. Files holding keys are
// referenced by path, so they can be mounted from a Kubernetes Secret.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Blob drivers.
const (
	BlobDriverFS  = "fs"
	BlobDriverGCS = "gcs"
)

// SFTP authentication methods.
const (
	SFTPAuthKey      = "key"
	SFTPAuthPassword = "password"
)

// MaxSignedURLTTL is the longest validity Google Cloud Storage accepts for a
// V4 signed URL.
const MaxSignedURLTTL = 7 * 24 * time.Hour

// Config is the complete runtime configuration of the service.
type Config struct {
	HTTPAddr        string
	LogLevel        string
	ShutdownTimeout time.Duration
	// ShutdownDrain is how long to keep serving with /readyz failing before
	// closing the listener. 0 locally; a few seconds behind a load balancer.
	ShutdownDrain time.Duration

	// DatabaseURL points at the PostgreSQL database that holds the rows to
	// export and the run history, e.g.
	// postgres://partner:secret@localhost:5432/partner?sslmode=disable.
	DatabaseURL string
	DBMaxConns  int

	Report Report
	Blob   Blob
	SFTP   SFTP
	Retry  Retry
}

// Report describes the daily file: which day it covers and how it is named.
type Report struct {
	// Timezone is the IANA zone the report day is cut in. "Yesterday" is
	// computed in this zone, not in UTC, so a run at 01:00 local time
	// exports the full previous local day.
	Timezone string
	// ScheduleHour and ScheduleMinute are the local time of the daily run.
	ScheduleHour   int
	ScheduleMinute int
	// FilePrefix and FileVersion form the file name agreed with the partner:
	// <prefix>_<version>_<YYYYMMDD>_<seq>.csv.
	FilePrefix  string
	FileVersion string
	// CSVDelimiter separates fields; many partners ask for '|' instead of ','.
	CSVDelimiter rune
}

// Blob configures where a copy of every file is archived before it is
// pushed, and how download links to that copy are signed.
type Blob struct {
	// Driver is "fs" (a directory, for development and tests) or "gcs".
	Driver string
	// Bucket is the GCS bucket; Prefix is the folder inside it.
	Bucket string
	Prefix string
	// FSRoot is the directory used by the fs driver.
	FSRoot string
	// PublicURL is the scheme and host exportd is reachable at; links for
	// the fs driver are built under it.
	PublicURL string
	// FSSigningSecret signs fs-driver links (at least 16 bytes). Empty
	// means a random secret per process: links then die with a restart.
	FSSigningSecret string
	// GCSEndpoint overrides the storage API base URL, for an emulator.
	GCSEndpoint string
	// GCSCredentialsFile optionally points at a service-account JSON key for
	// the storage client. Empty means Application Default Credentials
	// (Workload Identity, GOOGLE_APPLICATION_CREDENTIALS, gcloud login).
	GCSCredentialsFile string
	// GCSSignerKeyFile is the service-account JSON key whose private key
	// signs download URLs. Signing needs the key material itself; ADC alone
	// cannot produce a V4 signature without an IAM signBlob round trip.
	GCSSignerKeyFile string
	// SignedURLTTL is how long a download link stays valid (at most 7 days).
	SignedURLTTL time.Duration
}

// SFTP configures the partner server the file is delivered to.
type SFTP struct {
	Host string
	Port int
	User string
	// Auth is "key" (PrivateKeyFile) or "password" (Password).
	Auth           string
	Password       string
	PrivateKeyFile string
	// KnownHostsFile pins the server's host key (OpenSSH known_hosts format,
	// e.g. from `ssh-keyscan -p 22 sftp.partner.example`). Required unless
	// InsecureSkipHostKey is set, which is for local stand-ins only.
	KnownHostsFile      string
	InsecureSkipHostKey bool
	// RemoteDir is the directory on the server the file is written into,
	// relative to the login directory. Missing path segments are created.
	RemoteDir string
	// Timeout bounds the TCP dial and the SSH handshake.
	Timeout time.Duration
}

// Retry bounds how often a failing stage (archive, push) is attempted.
type Retry struct {
	Attempts int
	Backoff  time.Duration
}

// Load reads configuration from environment variables, applying defaults
// for optional values. It reports every invalid value at once so that a
// misconfigured deployment fails fast with a complete error message.
func Load() (Config, error) {
	return load(os.Getenv)
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func load(getenv func(string) string) (Config, error) {
	cfg := Config{
		HTTPAddr:        valueOr(getenv("PX_HTTP_ADDR"), ":8080"),
		LogLevel:        valueOr(getenv("PX_LOG_LEVEL"), "info"),
		ShutdownTimeout: 15 * time.Second,
		DatabaseURL:     getenv("PX_DATABASE_URL"),
		DBMaxConns:      10,
		Report: Report{
			Timezone:       valueOr(getenv("PX_REPORT_TIMEZONE"), "Asia/Ho_Chi_Minh"),
			ScheduleHour:   1,
			ScheduleMinute: 0,
			FilePrefix:     valueOr(getenv("PX_FILE_PREFIX"), "PartnerAccrual"),
			FileVersion:    valueOr(getenv("PX_FILE_VERSION"), "v1"),
			CSVDelimiter:   '|',
		},
		Blob: Blob{
			Driver:             valueOr(getenv("PX_BLOB_DRIVER"), BlobDriverFS),
			Bucket:             getenv("PX_BLOB_BUCKET"),
			Prefix:             valueOr(getenv("PX_BLOB_PREFIX"), "partner-export"),
			FSRoot:             valueOr(getenv("PX_BLOB_FS_ROOT"), "./data/blob"),
			PublicURL:          valueOr(getenv("PX_PUBLIC_URL"), "http://localhost:8080"),
			FSSigningSecret:    getenv("PX_FS_SIGNING_SECRET"),
			GCSEndpoint:        getenv("PX_GCS_ENDPOINT"),
			GCSCredentialsFile: getenv("PX_GCS_CREDENTIALS_FILE"),
			GCSSignerKeyFile:   getenv("PX_GCS_SIGNER_KEY_FILE"),
			SignedURLTTL:       MaxSignedURLTTL,
		},
		SFTP: SFTP{
			Host:           getenv("PX_SFTP_HOST"),
			Port:           22,
			User:           getenv("PX_SFTP_USER"),
			Auth:           valueOr(getenv("PX_SFTP_AUTH"), SFTPAuthKey),
			Password:       getenv("PX_SFTP_PASSWORD"),
			PrivateKeyFile: getenv("PX_SFTP_PRIVATE_KEY_FILE"),
			KnownHostsFile: getenv("PX_SFTP_KNOWN_HOSTS_FILE"),
			RemoteDir:      valueOr(getenv("PX_SFTP_REMOTE_DIR"), "input"),
			Timeout:        30 * time.Second,
		},
		Retry: Retry{Attempts: 3, Backoff: 2 * time.Second},
	}

	p := parser{getenv: getenv}

	// Service.
	cfg.ShutdownTimeout = p.duration("PX_SHUTDOWN_TIMEOUT", cfg.ShutdownTimeout, time.Nanosecond, 0)
	cfg.ShutdownDrain = p.duration("PX_SHUTDOWN_DRAIN", cfg.ShutdownDrain, 0, 0)
	cfg.DBMaxConns = p.integer("PX_DB_MAX_CONNS", cfg.DBMaxConns, 1, 1000)
	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		p.errorf("PX_LOG_LEVEL: unsupported level %s", strconv.Quote(cfg.LogLevel))
	}
	if cfg.DatabaseURL == "" {
		p.errorf("PX_DATABASE_URL: required")
	} else if u, err := url.Parse(cfg.DatabaseURL); err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		// The URL holds a password, so it is never echoed back.
		p.errorf("PX_DATABASE_URL: must be a postgres:// or postgresql:// URL with a host")
	}

	// Report.
	if _, err := time.LoadLocation(cfg.Report.Timezone); err != nil {
		p.errorf("PX_REPORT_TIMEZONE: unknown IANA zone %s", strconv.Quote(cfg.Report.Timezone))
	}
	if raw := getenv("PX_REPORT_SCHEDULE_AT"); raw != "" {
		h, m, ok := parseClock(raw)
		if !ok {
			p.errorf("PX_REPORT_SCHEDULE_AT: must be HH:MM, got %s", strconv.Quote(raw))
		} else {
			cfg.Report.ScheduleHour, cfg.Report.ScheduleMinute = h, m
		}
	}
	if !nameRe.MatchString(cfg.Report.FilePrefix) {
		p.errorf("PX_FILE_PREFIX: must match %s", nameRe)
	}
	if !nameRe.MatchString(cfg.Report.FileVersion) {
		p.errorf("PX_FILE_VERSION: must match %s", nameRe)
	}
	if raw := getenv("PX_CSV_DELIMITER"); raw != "" {
		r := []rune(raw)
		switch {
		case len(r) != 1:
			p.errorf("PX_CSV_DELIMITER: must be a single character")
		case r[0] == '"' || r[0] == '\n' || r[0] == '\r':
			p.errorf("PX_CSV_DELIMITER: cannot be a quote or a line break")
		default:
			cfg.Report.CSVDelimiter = r[0]
		}
	}

	// Blob.
	cfg.Blob.SignedURLTTL = p.duration("PX_SIGNED_URL_TTL", cfg.Blob.SignedURLTTL, time.Minute, MaxSignedURLTTL)
	switch cfg.Blob.Driver {
	case BlobDriverFS:
		if u, err := url.Parse(cfg.Blob.PublicURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			p.errorf("PX_PUBLIC_URL: must be an http(s) URL")
		}
		if n := len(cfg.Blob.FSSigningSecret); n > 0 && n < 16 {
			p.errorf("PX_FS_SIGNING_SECRET: must be at least 16 bytes")
		}
	case BlobDriverGCS:
		if cfg.Blob.Bucket == "" {
			p.errorf("PX_BLOB_BUCKET: required when PX_BLOB_DRIVER=gcs")
		}
		if cfg.Blob.GCSSignerKeyFile == "" {
			p.errorf("PX_GCS_SIGNER_KEY_FILE: required when PX_BLOB_DRIVER=gcs")
		}
	default:
		p.errorf("PX_BLOB_DRIVER: must be %q or %q", BlobDriverFS, BlobDriverGCS)
	}
	if raw := cfg.Blob.GCSEndpoint; raw != "" {
		if u, err := url.Parse(raw); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			p.errorf("PX_GCS_ENDPOINT: must be an http(s) URL")
		}
	}
	if strings.HasPrefix(cfg.Blob.Prefix, "/") || strings.Contains(cfg.Blob.Prefix, "..") {
		p.errorf("PX_BLOB_PREFIX: must be a relative path without '..'")
	}

	// SFTP.
	if cfg.SFTP.Host == "" {
		p.errorf("PX_SFTP_HOST: required")
	}
	if cfg.SFTP.User == "" {
		p.errorf("PX_SFTP_USER: required")
	}
	cfg.SFTP.Port = p.integer("PX_SFTP_PORT", cfg.SFTP.Port, 1, 65535)
	cfg.SFTP.Timeout = p.duration("PX_SFTP_TIMEOUT", cfg.SFTP.Timeout, time.Second, 0)
	switch cfg.SFTP.Auth {
	case SFTPAuthKey:
		if cfg.SFTP.PrivateKeyFile == "" {
			p.errorf("PX_SFTP_PRIVATE_KEY_FILE: required when PX_SFTP_AUTH=key")
		}
	case SFTPAuthPassword:
		if cfg.SFTP.Password == "" {
			p.errorf("PX_SFTP_PASSWORD: required when PX_SFTP_AUTH=password")
		}
	default:
		p.errorf("PX_SFTP_AUTH: must be %q or %q", SFTPAuthKey, SFTPAuthPassword)
	}
	cfg.SFTP.InsecureSkipHostKey = p.boolean("PX_SFTP_INSECURE_SKIP_HOST_KEY", false)
	if cfg.SFTP.KnownHostsFile == "" && !cfg.SFTP.InsecureSkipHostKey {
		p.errorf("PX_SFTP_KNOWN_HOSTS_FILE: required unless PX_SFTP_INSECURE_SKIP_HOST_KEY=true")
	}
	if strings.HasPrefix(cfg.SFTP.RemoteDir, "/") || strings.Contains(cfg.SFTP.RemoteDir, "..") {
		p.errorf("PX_SFTP_REMOTE_DIR: must be a relative path without '..'")
	}

	// Retry.
	cfg.Retry.Attempts = p.integer("PX_RETRY_ATTEMPTS", cfg.Retry.Attempts, 1, 10)
	cfg.Retry.Backoff = p.duration("PX_RETRY_BACKOFF", cfg.Retry.Backoff, 0, time.Minute)

	if err := errors.Join(p.errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

// Location returns the report zone. It is valid once Load succeeded.
func (r Report) Location() *time.Location {
	loc, err := time.LoadLocation(r.Timezone)
	if err != nil {
		panic("config: unvalidated timezone " + r.Timezone)
	}
	return loc
}

// Addr returns host:port of the SFTP server.
func (s SFTP) Addr() string {
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}

// parser accumulates every error so that all of them are reported together.
type parser struct {
	getenv func(string) string
	errs   []error
}

func (p *parser) errorf(format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf(format, args...))
}

// duration parses name as a Go duration within [min, max]; max 0 means no
// upper bound.
func (p *parser) duration(name string, def, minimum, maximum time.Duration) time.Duration {
	raw := p.getenv(name)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		p.errorf("%s: %w", name, err)
		return def
	}
	if d < minimum || (maximum > 0 && d > maximum) {
		if maximum > 0 {
			p.errorf("%s: must be between %s and %s", name, minimum, maximum)
		} else {
			p.errorf("%s: must be at least %s", name, minimum)
		}
		return def
	}
	return d
}

func (p *parser) integer(name string, def, minimum, maximum int) int {
	raw := p.getenv(name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < minimum || n > maximum {
		p.errorf("%s: must be an integer from %d to %d", name, minimum, maximum)
		return def
	}
	return n
}

func (p *parser) boolean(name string, def bool) bool {
	raw := p.getenv(name)
	if raw == "" {
		return def
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		p.errorf("%s: must be true or false", name)
		return def
	}
	return b
}

// parseClock parses "HH:MM" on a 24-hour clock.
func parseClock(s string) (hour, minute int, ok bool) {
	hh, mm, found := strings.Cut(s, ":")
	if !found || len(hh) != 2 || len(mm) != 2 {
		return 0, 0, false
	}
	h, err1 := strconv.Atoi(hh)
	m, err2 := strconv.Atoi(mm)
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

func valueOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
