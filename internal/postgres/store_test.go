package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/Nguyenngoctruong274/go-partner-export/internal/export"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/testdb"
)

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	return testdb.New(t, Migrate)
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := testdb.NewEmpty(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := Migrate(ctx, db); err != nil {
			t.Fatalf("migrate run %d: %v", i+1, err)
		}
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("schema_migrations has %d rows, want 2", n)
	}
}

func TestMigrateRejectsBrokenFiles(t *testing.T) {
	db := testdb.NewEmpty(t)
	err := migrate(context.Background(), db, brokenFS{})
	if err == nil {
		t.Fatal("expected an error for a broken migration")
	}
}

func trip(id int64, end time.Time) export.Trip {
	return export.Trip{
		EngagementID: id, PartnerUserID: "SKY1",
		BookingTime: end.Add(-30 * time.Minute), TripEndTime: end,
		ServiceType: "CAR", PickupLocation: "A", DropoffLocation: "B",
		TotalFare: 100, CustomerPay: 90, Discount: 10, Currency: "VND", PaymentMethod: "CARD", Status: "SUCCESS",
	}
}

func TestTripsUpsertAndListWindow(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := NewTripStore(db)
	hcm, _ := time.LoadLocation("Asia/Ho_Chi_Minh")
	day := export.Day{Year: 2026, Month: time.October, Date: 28}
	from, to := day.Window(hcm)

	trips := []export.Trip{
		trip(3, from.Add(time.Hour)),       // inside
		trip(1, from),                      // first instant: inside
		trip(2, to.Add(-time.Microsecond)), // last instant: inside (timestamptz has microsecond precision)
		trip(4, to),                        // end is exclusive: outside
		trip(5, from.Add(-time.Second)),    // outside
		trip(6, from.Add(time.Hour)),       // same end time as 3: ordered by id after it
	}
	if err := store.Upsert(ctx, trips); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, nil); err != nil {
		t.Fatalf("empty upsert: %v", err)
	}

	got, err := store.ListEnded(ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, g := range got {
		ids = append(ids, g.EngagementID)
	}
	want := []int64{1, 3, 6, 2}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
	}
	if !got[0].TripEndTime.Equal(from) || got[0].PickupLocation != "A" || got[0].CustomerPay != 90 {
		t.Errorf("row 1 = %+v", got[0])
	}
	n, err := store.CountEnded(ctx, from, to)
	if err != nil || n != 4 {
		t.Errorf("CountEnded = %d, %v", n, err)
	}

	// Replaying id 3 with new values updates the row instead of duplicating it.
	changed := trip(3, from.Add(2*time.Hour))
	changed.CustomerPay = 1
	changed.Status = "REFUNDED"
	if err := store.Upsert(ctx, []export.Trip{changed}); err != nil {
		t.Fatal(err)
	}
	got, err = store.ListEnded(ctx, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("after replay: %d rows", len(got))
	}
	var seen bool
	for _, g := range got {
		if g.EngagementID == 3 {
			seen = true
			if g.CustomerPay != 1 || g.Status != "REFUNDED" || !g.TripEndTime.Equal(from.Add(2*time.Hour)) {
				t.Errorf("replayed row = %+v", g)
			}
		}
	}
	if !seen {
		t.Error("replayed row missing")
	}
}

func TestRunsSaveGetList(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	store := NewRunStore(db)
	day := export.Day{Year: 2026, Month: time.October, Date: 28}
	from := time.Date(2026, 10, 27, 17, 0, 0, 0, time.UTC)

	if _, err := store.Get(ctx, day); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("Get missing = %v, want ErrRunNotFound", err)
	}

	r := export.Run{
		Day: day, From: from, To: from.Add(24 * time.Hour),
		State: export.StatePending, Attempts: 1, StartedAt: time.Now().UTC().Truncate(time.Microsecond),
	}
	if err := store.Save(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	if got.Day != day || got.State != export.StatePending || !got.FinishedAt.IsZero() || !got.From.Equal(from) {
		t.Errorf("Get = %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Error("timestamps not set")
	}

	// Saving the same day again replaces the row.
	r.State = export.StateFailed
	r.Stage = export.StagePush
	r.Error = "dial tcp: connection refused"
	r.Rows = 42
	r.Bytes = 4096
	r.FileName = "PartnerAccrual_v1_20261028_001.csv"
	r.BlobKey = "partner-export/PartnerAccrual_v1_20261028_001.csv"
	r.DownloadURL = "https://storage.googleapis.com/x?sig=1"
	r.FinishedAt = r.StartedAt.Add(time.Second)
	if err := store.Save(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err = store.Get(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != export.StateFailed || got.Stage != export.StagePush || got.Rows != 42 || got.Bytes != 4096 ||
		got.FileName != r.FileName || got.BlobKey != r.BlobKey || got.DownloadURL != r.DownloadURL ||
		!got.FinishedAt.Equal(r.FinishedAt) || got.Error != r.Error {
		t.Errorf("after update = %+v", got)
	}

	if err := store.Save(ctx, export.Run{Day: day, State: "weird", StartedAt: time.Now()}); err == nil {
		t.Error("invalid state accepted")
	}

	// Two more days, one pushed, for listing.
	for i, st := range []export.State{export.StatePushed, export.StateFailed} {
		d := day.AddDays(i + 1)
		if err := store.Save(ctx, export.Run{Day: d, From: from, To: from, State: st, Attempts: 1, StartedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := store.List(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Day != day.AddDays(2) || all[2].Day != day {
		t.Errorf("List all = %v", days(all))
	}
	failed, err := store.List(ctx, ListFilter{State: export.StateFailed})
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 2 {
		t.Errorf("List failed = %v", days(failed))
	}
	page, err := store.List(ctx, ListFilter{Before: day.AddDays(2), Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].Day != day.AddDays(1) {
		t.Errorf("List page = %v", days(page))
	}
}

func days(runs []export.Run) []string {
	out := make([]string, len(runs))
	for i, r := range runs {
		out[i] = r.Day.String()
	}
	return out
}
