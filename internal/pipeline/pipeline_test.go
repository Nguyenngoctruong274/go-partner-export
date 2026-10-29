package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Nguyenngoctruong274/go-partner-export/internal/blob"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/export"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/postgres"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/retry"
)

// --- fakes -----------------------------------------------------------------

type fakeTrips struct {
	trips []export.Trip
	err   error
}

func (f *fakeTrips) ListEnded(_ context.Context, from, to time.Time) ([]export.Trip, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []export.Trip
	for _, t := range f.trips {
		if !t.TripEndTime.Before(from) && t.TripEndTime.Before(to) {
			out = append(out, t)
		}
	}
	// The real store orders by (trip_end_time, engagement_id).
	sort.Slice(out, func(i, j int) bool {
		if !out[i].TripEndTime.Equal(out[j].TripEndTime) {
			return out[i].TripEndTime.Before(out[j].TripEndTime)
		}
		return out[i].EngagementID < out[j].EngagementID
	})
	return out, nil
}

type fakeRuns struct {
	mu    sync.Mutex
	runs  map[export.Day]export.Run
	saves []export.Run
	err   error
}

func newFakeRuns() *fakeRuns { return &fakeRuns{runs: map[export.Day]export.Run{}} }

func (f *fakeRuns) Save(_ context.Context, r export.Run) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.runs[r.Day] = r
	f.saves = append(f.saves, r)
	return nil
}

func (f *fakeRuns) Get(_ context.Context, day export.Day) (export.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[day]
	if !ok {
		return export.Run{}, postgres.ErrRunNotFound
	}
	return r, nil
}

type fakeUploader struct {
	mu       sync.Mutex
	files    map[string][]byte
	failures int // Upload fails this many times before succeeding
	calls    int
	err      error
}

func newFakeUploader() *fakeUploader { return &fakeUploader{files: map[string][]byte{}} }

func (f *fakeUploader) Upload(_ context.Context, dir, name string, r io.Reader, size int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	if f.failures > 0 {
		f.failures--
		return "", errors.New("connection reset")
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	if int64(len(data)) != size {
		return "", errors.New("size mismatch")
	}
	remote := dir + "/" + name
	f.files[remote] = data
	return remote, nil
}

type failingStore struct {
	blob.Store
	failures int
	calls    int
}

func (s *failingStore) Put(ctx context.Context, key, ct string, r io.Reader, size int64) error {
	s.calls++
	if s.failures > 0 {
		s.failures--
		return &blob.APIError{Status: 503, Body: "backend"}
	}
	return s.Store.Put(ctx, key, ct, r, size)
}

type recordingObserver struct {
	mu     sync.Mutex
	stages []string
	runs   []export.Run
}

func (o *recordingObserver) StageDone(stage string, _ time.Duration, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if err != nil {
		stage += "!"
	}
	o.stages = append(o.stages, stage)
}

func (o *recordingObserver) RunDone(r export.Run) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.runs = append(o.runs, r)
}

// --- fixture ---------------------------------------------------------------

type fixture struct {
	trips    *fakeTrips
	runs     *fakeRuns
	store    *failingStore
	uploader *fakeUploader
	obs      *recordingObserver
	clock    time.Time
	opts     Options
}

var hcm = mustLoad("Asia/Ho_Chi_Minh")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

var day = export.Day{Year: 2026, Month: time.October, Date: 28}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	from, to := day.Window(hcm)
	fs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		trips: &fakeTrips{trips: []export.Trip{
			{EngagementID: 2, PartnerUserID: "SKY2", BookingTime: from, TripEndTime: from.Add(time.Hour), ServiceType: "CAR", TotalFare: 100, CustomerPay: 100, Currency: "VND", Status: "SUCCESS"},
			{EngagementID: 1, PartnerUserID: "SKY1", BookingTime: from, TripEndTime: from, ServiceType: "BIKE", TotalFare: 50, CustomerPay: 50, Currency: "VND", Status: "SUCCESS"},
			{EngagementID: 3, PartnerUserID: "SKY3", BookingTime: to, TripEndTime: to, ServiceType: "CAR", TotalFare: 1, CustomerPay: 1}, // next day
		}},
		runs:     newFakeRuns(),
		store:    &failingStore{Store: fs},
		uploader: newFakeUploader(),
		obs:      &recordingObserver{},
		clock:    time.Date(2026, 10, 29, 1, 0, 0, 0, hcm),
	}
	f.opts = Options{
		Naming:       export.Naming{Prefix: "PartnerAccrual", Version: "v1"},
		Format:       export.Format{Delimiter: '|'},
		Location:     hcm,
		BlobPrefix:   "partner-export",
		RemoteDir:    "input",
		SignedURLTTL: time.Hour,
		Retry:        retry.Policy{Attempts: 3, Backoff: time.Millisecond},
		Now: func() time.Time {
			f.clock = f.clock.Add(time.Second)
			return f.clock
		},
		Observer: f.obs,
	}
	return f
}

func (f *fixture) pipeline(t *testing.T) *Pipeline {
	t.Helper()
	signer, err := blob.NewHMACSigner("http://export.test", []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(f.trips, f.runs, f.store, signer, f.uploader, slog.New(slog.NewTextHandler(io.Discard, nil)), f.opts)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// --- tests -----------------------------------------------------------------

func TestRunHappyPath(t *testing.T) {
	f := newFixture(t)
	p := f.pipeline(t)
	run, err := p.Run(context.Background(), day)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != export.StatePushed || run.Stage != export.StagePush || run.Error != "" {
		t.Errorf("run = %+v", run)
	}
	if run.Rows != 2 || run.Attempts != 1 || run.FileName != "PartnerAccrual_v1_20261028_001.csv" ||
		run.BlobKey != "partner-export/PartnerAccrual_v1_20261028_001.csv" ||
		run.RemotePath != "input/PartnerAccrual_v1_20261028_001.csv" || run.FinishedAt.IsZero() {
		t.Errorf("run = %+v", run)
	}
	if !strings.HasPrefix(run.DownloadURL, "http://export.test/v1/blob/partner-export/PartnerAccrual_v1_20261028_001.csv?") {
		t.Errorf("download url = %s", run.DownloadURL)
	}

	// The pushed bytes are exactly the archived bytes, and both are the CSV.
	pushed := f.uploader.files[run.RemotePath]
	rc, err := f.store.Open(context.Background(), run.BlobKey)
	if err != nil {
		t.Fatal(err)
	}
	archived, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(pushed, archived) || int64(len(pushed)) != run.Bytes {
		t.Errorf("pushed %d bytes, archived %d, recorded %d", len(pushed), len(archived), run.Bytes)
	}
	records, err := export.Format{Delimiter: '|', Location: hcm}.Read(bytes.NewReader(pushed))
	if err != nil || len(records) != 2 || records[0][2] != "1" || records[1][2] != "2" {
		t.Errorf("records = %v, %v", records, err)
	}

	// The run record was written after every stage, in order.
	var states []export.State
	for _, s := range f.runs.saves {
		states = append(states, s.State)
	}
	want := []export.State{export.StatePending, export.StateBuilt, export.StateArchived, export.StatePushed}
	if len(states) != len(want) {
		t.Fatalf("saved states = %v", states)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("saved states = %v", states)
		}
	}
	if got := strings.Join(f.obs.stages, ","); got != "select,build,archive,sign,push" {
		t.Errorf("observed stages = %s", got)
	}
	if len(f.obs.runs) != 1 || f.obs.runs[0].State != export.StatePushed {
		t.Errorf("observed runs = %+v", f.obs.runs)
	}
}

func TestRunRetriesArchiveAndPush(t *testing.T) {
	f := newFixture(t)
	f.store.failures = 2
	f.uploader.failures = 2
	p := f.pipeline(t)
	run, err := p.Run(context.Background(), day)
	if err != nil {
		t.Fatal(err)
	}
	if run.State != export.StatePushed || f.store.calls != 3 || f.uploader.calls != 3 {
		t.Errorf("state=%s store calls=%d upload calls=%d", run.State, f.store.calls, f.uploader.calls)
	}
}

func TestRunFailsAtEachStage(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(f *fixture)
		stage   string
		state   export.State
		saves   int
	}{
		{"select", func(f *fixture) { f.trips.err = errors.New("db down") }, export.StageSelect, export.StateFailed, 2},
		{"archive", func(f *fixture) { f.store.failures = 10 }, export.StageArchive, export.StateFailed, 3},
		{"push", func(f *fixture) { f.uploader.err = errors.New("auth failed") }, export.StagePush, export.StateFailed, 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			tt.arrange(f)
			p := f.pipeline(t)
			run, err := p.Run(context.Background(), day)
			var se *StageError
			if !errors.As(err, &se) || se.Stage != tt.stage {
				t.Fatalf("err = %v, want stage %s", err, tt.stage)
			}
			if run.State != tt.state || run.Stage != tt.stage || run.Error == "" || run.FinishedAt.IsZero() {
				t.Errorf("run = %+v", run)
			}
			saved := f.runs.runs[day]
			if saved.State != export.StateFailed || saved.Stage != tt.stage {
				t.Errorf("saved = %+v", saved)
			}
			if len(f.runs.saves) != tt.saves {
				t.Errorf("saves = %d, want %d", len(f.runs.saves), tt.saves)
			}
			if tt.stage == export.StagePush && len(f.uploader.files) != 0 {
				t.Error("file was delivered despite the failure")
			}
			if f.obs.runs[len(f.obs.runs)-1].State != export.StateFailed {
				t.Error("observer did not see the failure")
			}
		})
	}
}

func TestRerunReplacesAndCountsAttempts(t *testing.T) {
	f := newFixture(t)
	f.uploader.err = errors.New("partner down")
	p := f.pipeline(t)
	if _, err := p.Run(context.Background(), day); err == nil {
		t.Fatal("first run should fail")
	}

	// New data arrives, the partner is back: the re-run picks up both.
	from, _ := day.Window(hcm)
	f.trips.trips = append(f.trips.trips, export.Trip{EngagementID: 9, TripEndTime: from.Add(2 * time.Hour), BookingTime: from, Currency: "VND"})
	f.uploader.err = nil
	run, err := p.Run(context.Background(), day)
	if err != nil {
		t.Fatal(err)
	}
	if run.Attempts != 2 || run.Rows != 3 || run.State != export.StatePushed || run.Error != "" {
		t.Errorf("re-run = %+v", run)
	}
	if run.FileName != "PartnerAccrual_v1_20261028_001.csv" {
		t.Errorf("file name changed on re-run: %s", run.FileName)
	}
	if len(f.uploader.files) != 1 {
		t.Errorf("remote files = %d, want 1 (replaced)", len(f.uploader.files))
	}
}

func TestRepush(t *testing.T) {
	f := newFixture(t)
	p := f.pipeline(t)
	ctx := context.Background()
	if _, err := p.Repush(ctx, day); err == nil {
		t.Fatal("repush of an unknown day accepted")
	}
	first, err := p.Run(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	delete(f.uploader.files, first.RemotePath)

	// Data changes after the run; repush must send the archived bytes, not new ones.
	f.trips.trips = nil
	run, err := p.Repush(ctx, day)
	if err != nil {
		t.Fatal(err)
	}
	if run.Attempts != 2 || run.State != export.StatePushed || run.Rows != 2 {
		t.Errorf("repush = %+v", run)
	}
	if !bytes.Contains(f.uploader.files[run.RemotePath], []byte("SKY1")) {
		t.Error("repush did not send the archived file")
	}

	f.uploader.err = errors.New("down")
	run, err = p.Repush(ctx, day)
	if err == nil || run.State != export.StateFailed || run.Stage != export.StagePush {
		t.Errorf("failed repush = %+v, %v", run, err)
	}

	// A run that never got archived cannot be re-pushed.
	f.runs.runs[day] = export.Run{Day: day, State: export.StateFailed, Stage: export.StageSelect}
	if _, err := p.Repush(ctx, day); err == nil {
		t.Error("repush without an archive accepted")
	}
}

func TestRunRecordsWhenCancelled(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.trips.err = context.Canceled
	cancel()
	p := f.pipeline(t)
	_, err := p.Run(ctx, day)
	if err == nil {
		t.Fatal("expected an error")
	}
	// The failure is still written down although the context is gone.
	if saved := f.runs.runs[day]; saved.State != export.StateFailed {
		t.Errorf("saved = %+v", saved)
	}
}

func TestNewValidates(t *testing.T) {
	f := newFixture(t)
	signer, _ := blob.NewHMACSigner("http://x", []byte("0123456789abcdef0123456789abcdef"))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := New(nil, f.runs, f.store, signer, f.uploader, logger, f.opts); err == nil {
		t.Error("nil dependency accepted")
	}
	bad := f.opts
	bad.Naming.Prefix = "no spaces allowed"
	if _, err := New(f.trips, f.runs, f.store, signer, f.uploader, logger, bad); err == nil {
		t.Error("bad naming accepted")
	}
	bad = f.opts
	bad.Location = nil
	if _, err := New(f.trips, f.runs, f.store, signer, f.uploader, logger, bad); err == nil {
		t.Error("missing location accepted")
	}
	bad = f.opts
	bad.Format.Delimiter = '"'
	if _, err := New(f.trips, f.runs, f.store, signer, f.uploader, logger, bad); err == nil {
		t.Error("bad format accepted")
	}
	// Defaults fill in.
	minimal := Options{Naming: f.opts.Naming, Format: export.Format{Delimiter: ','}, Location: hcm}
	p, err := New(f.trips, f.runs, f.store, signer, f.uploader, nil, minimal)
	if err != nil {
		t.Fatal(err)
	}
	if p.opts.SignedURLTTL != 24*time.Hour || p.opts.Format.Location != hcm || p.opts.Now == nil {
		t.Errorf("defaults = %+v", p.opts)
	}
}

func TestStageError(t *testing.T) {
	base := errors.New("boom")
	err := &StageError{Stage: "push", Err: base}
	if err.Error() != "pipeline: push: boom" || !errors.Is(err, base) {
		t.Errorf("err = %v", err)
	}
}

// BenchmarkRun measures the pipeline over fakes with the fs archive: the
// cost of select-build-archive-sign-push-record without the network.
func BenchmarkRun(b *testing.B) {
	for _, rows := range []int{100, 10_000} {
		b.Run(fmt.Sprintf("rows=%d", rows), func(b *testing.B) {
			from, _ := day.Window(hcm)
			trips := make([]export.Trip, 0, rows)
			for i := 0; i < rows; i++ {
				trips = append(trips, export.Trip{
					EngagementID: int64(i), PartnerUserID: "SKY", BookingTime: from, TripEndTime: from.Add(time.Duration(i) * time.Millisecond),
					ServiceType: "CAR", PickupLocation: "1 Lê Lợi, Quận 1", DropoffLocation: "Sân bay Tân Sơn Nhất",
					TotalFare: 150000, CustomerPay: 140000, Discount: 10000, Currency: "VND", PaymentMethod: "CARD", Status: "SUCCESS",
				})
			}
			fs, err := blob.NewFS(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			signer, _ := blob.NewHMACSigner("http://x", []byte("0123456789abcdef0123456789abcdef"))
			f := &fixture{trips: &fakeTrips{trips: trips}, runs: newFakeRuns(), store: &failingStore{Store: fs}, uploader: newFakeUploader()}
			p, err := New(f.trips, f.runs, f.store, signer, f.uploader, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
				Naming: export.Naming{Prefix: "PartnerAccrual", Version: "v1"}, Format: export.Format{Delimiter: '|'},
				Location: hcm, BlobPrefix: "partner-export", RemoteDir: "input", SignedURLTTL: time.Hour,
			})
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := p.Run(context.Background(), day); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
