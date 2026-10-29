package pipeline

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Nguyenngoctruong274/go-partner-export/internal/blob"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/export"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/postgres"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/retry"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/sftp"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/sftptest"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/testdb"
)

// TestEndToEnd runs the real stores: PostgreSQL (skipped without
// PX_TEST_DATABASE_URL), the fs archive and an in-process SFTP server.
func TestEndToEnd(t *testing.T) {
	db := testdb.New(t, postgres.Migrate)
	ctx := context.Background()
	trips := postgres.NewTripStore(db)
	runs := postgres.NewRunStore(db)

	srv := sftptest.Start(t)
	uploader, err := sftp.New(sftp.Options{
		Addr: srv.Addr, User: srv.User, PrivateKeyPEM: []byte(srv.ClientKeyPEM),
		KnownHostsFile: srv.WriteKnownHosts(t, t.TempDir(), false), Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := blob.NewFS(filepath.Join(t.TempDir(), "blob"))
	if err != nil {
		t.Fatal(err)
	}
	signer, err := blob.NewHMACSigner("http://export.test", []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}

	from, to := day.Window(hcm)
	seed := []export.Trip{
		{EngagementID: 11, PartnerUserID: "SKY11", BookingTime: from.Add(-time.Hour), TripEndTime: from.Add(5 * time.Hour), ServiceType: "CAR", PickupLocation: "Quận 1", DropoffLocation: "Sân bay", TotalFare: 150000, Discount: 10000, CustomerPay: 140000, Currency: "VND", PaymentMethod: "CARD", Status: "SUCCESS"},
		{EngagementID: 12, PartnerUserID: "SKY12", BookingTime: from, TripEndTime: to.Add(-time.Minute), ServiceType: "BIKE", TotalFare: 30000, CustomerPay: 30000, Currency: "VND", PaymentMethod: "CASH", Status: "SUCCESS"},
		{EngagementID: 13, PartnerUserID: "SKY13", BookingTime: to, TripEndTime: to, ServiceType: "CAR", TotalFare: 1, CustomerPay: 1, Currency: "VND"}, // next day
	}
	if err := trips.Upsert(ctx, seed); err != nil {
		t.Fatal(err)
	}

	p, err := New(trips, runs, store, signer, uploader, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
		Naming: export.Naming{Prefix: "PartnerAccrual", Version: "v1"}, Format: export.Format{Delimiter: '|'},
		Location: hcm, BlobPrefix: "partner-export", RemoteDir: "acme/input", SignedURLTTL: time.Hour,
		Retry: retry.Policy{Attempts: 2, Backoff: time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}

	run, err := p.Run(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != export.StatePushed || run.Rows != 2 {
		t.Fatalf("run = %+v", run)
	}

	// The file on the partner's server is the file in the archive.
	remote, err := os.ReadFile(filepath.Join(srv.Root, "acme", "input", "PartnerAccrual_v1_20261028_001.csv"))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := store.Open(ctx, run.BlobKey)
	if err != nil {
		t.Fatal(err)
	}
	archived, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(remote) != string(archived) {
		t.Error("remote file differs from the archived copy")
	}
	lines := strings.Split(strings.TrimSpace(string(remote)), "\n")
	if len(lines) != 3 || !strings.Contains(lines[1], "SKY11|") || !strings.Contains(lines[1], "|Quận 1|") || !strings.Contains(lines[2], "SKY12|") {
		t.Errorf("file content:\n%s", remote)
	}

	// The run is in PostgreSQL with the link.
	saved, err := runs.Get(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	if saved.State != export.StatePushed || saved.Rows != 2 || saved.DownloadURL != run.DownloadURL ||
		saved.RemotePath != "acme/input/PartnerAccrual_v1_20261028_001.csv" {
		t.Errorf("saved = %+v", saved)
	}

	// Re-run after a correction: same file name, new content, attempt 2.
	seed[0].CustomerPay = 1
	if err := trips.Upsert(ctx, seed[:1]); err != nil {
		t.Fatal(err)
	}
	run2, err := p.Run(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	remote, _ = os.ReadFile(filepath.Join(srv.Root, "acme", "input", "PartnerAccrual_v1_20261028_001.csv"))
	if run2.Attempts != 2 || !strings.Contains(string(remote), "|1|VND|") {
		t.Errorf("re-run attempts=%d content:\n%s", run2.Attempts, remote)
	}

	// Repush after the partner lost the file.
	if err := os.Remove(filepath.Join(srv.Root, "acme", "input", "PartnerAccrual_v1_20261028_001.csv")); err != nil {
		t.Fatal(err)
	}
	run3, err := p.Repush(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(srv.Root, "acme", "input", "PartnerAccrual_v1_20261028_001.csv")); err != nil || run3.Attempts != 3 {
		t.Errorf("repush: %v, attempts=%d", err, run3.Attempts)
	}

	// A partner outage is recorded as a failed push and nothing is delivered.
	srv.Close()
	if err := os.Remove(filepath.Join(srv.Root, "acme", "input", "PartnerAccrual_v1_20261028_001.csv")); err != nil {
		t.Fatal(err)
	}
	run4, err := p.Run(ctx, day)
	if err == nil || run4.State != export.StateFailed || run4.Stage != export.StagePush {
		t.Errorf("outage run = %+v, %v", run4, err)
	}
	saved, _ = runs.Get(ctx, day)
	if saved.State != export.StateFailed || saved.Attempts != 4 || saved.BlobKey == "" {
		t.Errorf("saved after outage = %+v", saved)
	}
}
