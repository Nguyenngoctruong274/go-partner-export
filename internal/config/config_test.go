package config

import (
	"strings"
	"testing"
	"time"
)

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// minimal is the smallest environment that loads: a database, an SFTP
// target with key auth and a pinned host key.
func minimal() map[string]string {
	return map[string]string{
		"PX_DATABASE_URL":          "postgres://partner:partner@localhost:5432/partner",
		"PX_SFTP_HOST":             "sftp.partner.example",
		"PX_SFTP_USER":             "partner",
		"PX_SFTP_PRIVATE_KEY_FILE": "/etc/export/sftp_id_ed25519",
		"PX_SFTP_KNOWN_HOSTS_FILE": "/etc/export/known_hosts",
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := load(envFrom(minimal()))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		HTTPAddr:        ":8080",
		LogLevel:        "info",
		ShutdownTimeout: 15 * time.Second,
		DatabaseURL:     "postgres://partner:partner@localhost:5432/partner",
		DBMaxConns:      10,
		Report: Report{
			Timezone:       "Asia/Ho_Chi_Minh",
			ScheduleHour:   1,
			ScheduleMinute: 0,
			FilePrefix:     "PartnerAccrual",
			FileVersion:    "v1",
			CSVDelimiter:   '|',
		},
		Blob: Blob{
			Driver:       BlobDriverFS,
			Prefix:       "partner-export",
			FSRoot:       "./data/blob",
			SignedURLTTL: MaxSignedURLTTL,
		},
		SFTP: SFTP{
			Host:           "sftp.partner.example",
			Port:           22,
			User:           "partner",
			Auth:           SFTPAuthKey,
			PrivateKeyFile: "/etc/export/sftp_id_ed25519",
			KnownHostsFile: "/etc/export/known_hosts",
			RemoteDir:      "input",
			Timeout:        30 * time.Second,
		},
		Retry: Retry{Attempts: 3, Backoff: 2 * time.Second},
	}
	if cfg != want {
		t.Fatalf("got  %+v\nwant %+v", cfg, want)
	}
	if cfg.SFTP.Addr() != "sftp.partner.example:22" {
		t.Errorf("Addr() = %q", cfg.SFTP.Addr())
	}
	if cfg.Report.Location().String() != "Asia/Ho_Chi_Minh" {
		t.Errorf("Location() = %v", cfg.Report.Location())
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := load(envFrom(map[string]string{
		"PX_HTTP_ADDR":        "127.0.0.1:9090",
		"PX_LOG_LEVEL":        "debug",
		"PX_SHUTDOWN_TIMEOUT": "30s",
		"PX_SHUTDOWN_DRAIN":   "5s",
		"PX_DATABASE_URL":     "postgresql://partner:pw@db.internal:6432/partner?sslmode=require",
		"PX_DB_MAX_CONNS":     "25",

		"PX_REPORT_TIMEZONE":    "Asia/Singapore",
		"PX_REPORT_SCHEDULE_AT": "23:30",
		"PX_FILE_PREFIX":        "BeAffiliateAccrual",
		"PX_FILE_VERSION":       "v0",
		"PX_CSV_DELIMITER":      ";",

		"PX_BLOB_DRIVER":          "gcs",
		"PX_BLOB_BUCKET":          "acme-fin-prod",
		"PX_BLOB_PREFIX":          "partner-report-daily",
		"PX_GCS_ENDPOINT":         "http://localhost:4443",
		"PX_GCS_CREDENTIALS_FILE": "/var/run/secrets/gcs/client.json",
		"PX_GCS_SIGNER_KEY_FILE":  "/var/run/secrets/gcs/signer.json",
		"PX_SIGNED_URL_TTL":       "72h",

		"PX_SFTP_HOST":                   "sftpgw.partner.example",
		"PX_SFTP_PORT":                   "2222",
		"PX_SFTP_USER":                   "acme-prod",
		"PX_SFTP_AUTH":                   "password",
		"PX_SFTP_PASSWORD":               "hunter2",
		"PX_SFTP_KNOWN_HOSTS_FILE":       "/etc/export/known_hosts",
		"PX_SFTP_REMOTE_DIR":             "acme/input",
		"PX_SFTP_TIMEOUT":                "10s",
		"PX_SFTP_INSECURE_SKIP_HOST_KEY": "false",

		"PX_RETRY_ATTEMPTS": "5",
		"PX_RETRY_BACKOFF":  "500ms",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		HTTPAddr:        "127.0.0.1:9090",
		LogLevel:        "debug",
		ShutdownTimeout: 30 * time.Second,
		ShutdownDrain:   5 * time.Second,
		DatabaseURL:     "postgresql://partner:pw@db.internal:6432/partner?sslmode=require",
		DBMaxConns:      25,
		Report: Report{
			Timezone:       "Asia/Singapore",
			ScheduleHour:   23,
			ScheduleMinute: 30,
			FilePrefix:     "BeAffiliateAccrual",
			FileVersion:    "v0",
			CSVDelimiter:   ';',
		},
		Blob: Blob{
			Driver:             BlobDriverGCS,
			Bucket:             "acme-fin-prod",
			Prefix:             "partner-report-daily",
			FSRoot:             "./data/blob",
			GCSEndpoint:        "http://localhost:4443",
			GCSCredentialsFile: "/var/run/secrets/gcs/client.json",
			GCSSignerKeyFile:   "/var/run/secrets/gcs/signer.json",
			SignedURLTTL:       72 * time.Hour,
		},
		SFTP: SFTP{
			Host:           "sftpgw.partner.example",
			Port:           2222,
			User:           "acme-prod",
			Auth:           SFTPAuthPassword,
			Password:       "hunter2",
			KnownHostsFile: "/etc/export/known_hosts",
			RemoteDir:      "acme/input",
			Timeout:        10 * time.Second,
		},
		Retry: Retry{Attempts: 5, Backoff: 500 * time.Millisecond},
	}
	if cfg != want {
		t.Fatalf("got  %+v\nwant %+v", cfg, want)
	}
}

func TestLoadAllowsSkippingHostKeyLocally(t *testing.T) {
	env := minimal()
	delete(env, "PX_SFTP_KNOWN_HOSTS_FILE")
	env["PX_SFTP_INSECURE_SKIP_HOST_KEY"] = "true"
	cfg, err := load(envFrom(env))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SFTP.InsecureSkipHostKey || cfg.SFTP.KnownHostsFile != "" {
		t.Fatalf("unexpected sftp config %+v", cfg.SFTP)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	with := func(kv ...string) map[string]string {
		env := minimal()
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i+1] == "" {
				delete(env, kv[i])
			} else {
				env[kv[i]] = kv[i+1]
			}
		}
		return env
	}

	tests := []struct {
		name string
		env  map[string]string
		want []string // every substring must appear in the error
	}{
		{
			name: "missing database url",
			env:  with("PX_DATABASE_URL", ""),
			want: []string{"PX_DATABASE_URL: required"},
		},
		{
			name: "wrong database scheme",
			env:  with("PX_DATABASE_URL", "mysql://localhost:3306/partner"),
			want: []string{"PX_DATABASE_URL: must be"},
		},
		{
			name: "database password is never echoed",
			env:  with("PX_DATABASE_URL", "http://partner:hunter2@localhost/partner"),
			want: []string{"PX_DATABASE_URL: must be"},
		},
		{
			name: "service settings",
			env: with("PX_LOG_LEVEL", "loud", "PX_SHUTDOWN_TIMEOUT", "0s", "PX_SHUTDOWN_DRAIN", "-1s",
				"PX_DB_MAX_CONNS", "0"),
			want: []string{
				`PX_LOG_LEVEL: unsupported level "loud"`,
				"PX_SHUTDOWN_TIMEOUT: must be at least",
				"PX_SHUTDOWN_DRAIN: must be at least",
				"PX_DB_MAX_CONNS: must be an integer from 1 to 1000",
			},
		},
		{
			name: "unparsable duration",
			env:  with("PX_SHUTDOWN_TIMEOUT", "soon"),
			want: []string{"PX_SHUTDOWN_TIMEOUT: time: invalid duration"},
		},
		{
			name: "report settings",
			env: with("PX_REPORT_TIMEZONE", "Mars/Olympus", "PX_REPORT_SCHEDULE_AT", "1:00",
				"PX_FILE_PREFIX", "Partner Accrual", "PX_FILE_VERSION", "v1/2", "PX_CSV_DELIMITER", "||"),
			want: []string{
				`PX_REPORT_TIMEZONE: unknown IANA zone "Mars/Olympus"`,
				"PX_REPORT_SCHEDULE_AT: must be HH:MM",
				"PX_FILE_PREFIX: must match",
				"PX_FILE_VERSION: must match",
				"PX_CSV_DELIMITER: must be a single character",
			},
		},
		{
			name: "schedule out of range",
			env:  with("PX_REPORT_SCHEDULE_AT", "24:00"),
			want: []string{"PX_REPORT_SCHEDULE_AT: must be HH:MM"},
		},
		{
			name: "delimiter cannot be a quote",
			env:  with("PX_CSV_DELIMITER", `"`),
			want: []string{"PX_CSV_DELIMITER: cannot be a quote"},
		},
		{
			name: "gcs driver needs a bucket and a signer key",
			env:  with("PX_BLOB_DRIVER", "gcs"),
			want: []string{
				"PX_BLOB_BUCKET: required when PX_BLOB_DRIVER=gcs",
				"PX_GCS_SIGNER_KEY_FILE: required when PX_BLOB_DRIVER=gcs",
			},
		},
		{
			name: "unknown blob driver and absolute prefix",
			env:  with("PX_BLOB_DRIVER", "s3", "PX_BLOB_PREFIX", "/exports"),
			want: []string{`PX_BLOB_DRIVER: must be "fs" or "gcs"`, "PX_BLOB_PREFIX: must be a relative path"},
		},
		{
			name: "gcs endpoint must be a URL",
			env:  with("PX_GCS_ENDPOINT", "localhost:4443"),
			want: []string{"PX_GCS_ENDPOINT: must be an http(s) URL"},
		},
		{
			name: "signed url ttl beyond the GCS limit",
			env:  with("PX_SIGNED_URL_TTL", "200h"),
			want: []string{"PX_SIGNED_URL_TTL: must be between 1m0s and 168h0m0s"},
		},
		{
			name: "sftp target",
			env:  with("PX_SFTP_HOST", "", "PX_SFTP_USER", "", "PX_SFTP_PORT", "70000", "PX_SFTP_TIMEOUT", "10ms"),
			want: []string{
				"PX_SFTP_HOST: required",
				"PX_SFTP_USER: required",
				"PX_SFTP_PORT: must be an integer from 1 to 65535",
				"PX_SFTP_TIMEOUT: must be at least 1s",
			},
		},
		{
			name: "key auth without a key file",
			env:  with("PX_SFTP_PRIVATE_KEY_FILE", ""),
			want: []string{"PX_SFTP_PRIVATE_KEY_FILE: required when PX_SFTP_AUTH=key"},
		},
		{
			name: "password auth without a password",
			env:  with("PX_SFTP_AUTH", "password"),
			want: []string{"PX_SFTP_PASSWORD: required when PX_SFTP_AUTH=password"},
		},
		{
			name: "unknown auth method",
			env:  with("PX_SFTP_AUTH", "agent"),
			want: []string{`PX_SFTP_AUTH: must be "key" or "password"`},
		},
		{
			name: "host key must be pinned unless explicitly skipped",
			env:  with("PX_SFTP_KNOWN_HOSTS_FILE", ""),
			want: []string{"PX_SFTP_KNOWN_HOSTS_FILE: required unless PX_SFTP_INSECURE_SKIP_HOST_KEY=true"},
		},
		{
			name: "bad boolean and remote dir escaping the login directory",
			env:  with("PX_SFTP_INSECURE_SKIP_HOST_KEY", "yes please", "PX_SFTP_REMOTE_DIR", "../other"),
			want: []string{"PX_SFTP_INSECURE_SKIP_HOST_KEY: must be true or false", "PX_SFTP_REMOTE_DIR: must be a relative path"},
		},
		{
			name: "retry bounds",
			env:  with("PX_RETRY_ATTEMPTS", "11", "PX_RETRY_BACKOFF", "2m"),
			want: []string{"PX_RETRY_ATTEMPTS: must be an integer from 1 to 10", "PX_RETRY_BACKOFF: must be between 0s and 1m0s"},
		},
		{
			name: "every error is reported at once",
			env:  map[string]string{"PX_LOG_LEVEL": "loud"},
			want: []string{"PX_DATABASE_URL: required", "PX_SFTP_HOST: required", `PX_LOG_LEVEL: unsupported level "loud"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := load(envFrom(tt.env))
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not contain %q", err, w)
				}
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error %q leaks a secret", err)
			}
		})
	}
}

func TestParseClock(t *testing.T) {
	tests := []struct {
		in     string
		h, m   int
		wantOK bool
	}{
		{"00:00", 0, 0, true},
		{"23:59", 23, 59, true},
		{"01:05", 1, 5, true},
		{"1:05", 0, 0, false},
		{"01:5", 0, 0, false},
		{"24:00", 0, 0, false},
		{"12:60", 0, 0, false},
		{"12-30", 0, 0, false},
		{"ab:cd", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, tt := range tests {
		h, m, ok := parseClock(tt.in)
		if ok != tt.wantOK || h != tt.h || m != tt.m {
			t.Errorf("parseClock(%q) = %d, %d, %v; want %d, %d, %v", tt.in, h, m, ok, tt.h, tt.m, tt.wantOK)
		}
	}
}
