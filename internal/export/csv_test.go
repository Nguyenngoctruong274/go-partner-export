package export

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func hcmFormat(t testing.TB) Format {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		t.Fatal(err)
	}
	return Format{Delimiter: '|', Location: loc}
}

func sampleTrips() []Trip {
	return []Trip{
		{
			EngagementID: 9001, PartnerUserID: "SKY123",
			BookingTime: time.Date(2026, 10, 28, 1, 2, 3, 0, time.UTC),
			TripEndTime: time.Date(2026, 10, 28, 2, 3, 4, 0, time.UTC),
			ServiceType: "CAR", PickupLocation: "1 Lê Lợi, Q1", DropoffLocation: "Sân bay Tân Sơn Nhất",
			TotalFare: 150000, Discount: 20000, TollFee: 10000, InsuranceFee: 2000, CustomerPay: 142000,
			Currency: "VND", PaymentMethod: "CARD", Status: "SUCCESS",
		},
		{
			EngagementID: 9002, PartnerUserID: "SKY456",
			BookingTime: time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC),
			TripEndTime: time.Date(2026, 10, 28, 10, 30, 0, 0, time.UTC),
			ServiceType: "BIKE", PickupLocation: `Quán "Cơm" | Ngon`, DropoffLocation: "Line1\nLine2",
			TotalFare: 30000, CustomerPay: 30000, Currency: "VND", PaymentMethod: "CASH", Status: "SUCCESS",
		},
	}
}

func TestWriteProducesTheAgreedLayout(t *testing.T) {
	f := hcmFormat(t)
	var buf bytes.Buffer
	n, err := f.Write(&buf, sampleTrips())
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(buf.Len()) {
		t.Errorf("reported %d bytes, wrote %d", n, buf.Len())
	}
	out := buf.String()
	if !strings.HasPrefix(out, utf8BOM) {
		t.Error("missing UTF-8 BOM")
	}
	lines := strings.Split(strings.TrimSuffix(strings.TrimPrefix(out, utf8BOM), "\n"), "\n")
	if lines[0] != strings.Join(Columns, "|") {
		t.Errorf("header = %q", lines[0])
	}
	want := "SKY123|2026-10-28T08:02:03+07:00|9001|CAR|1 Lê Lợi, Q1|Sân bay Tân Sơn Nhất|2026-10-28T09:03:04+07:00|150000|20000|10000|2000|142000|VND|CARD|SUCCESS"
	if lines[1] != want {
		t.Errorf("row 1 = %q\n     want %q", lines[1], want)
	}
	// A field holding the delimiter, a quote or a newline is quoted, with
	// the quote doubled, so the row survives a round trip.
	if !strings.Contains(out, `"Quán ""Cơm"" | Ngon"`) {
		t.Errorf("field with quote and delimiter not escaped: %q", out)
	}
	if !strings.Contains(out, "\"Line1\nLine2\"") {
		t.Errorf("field with newline not quoted: %q", out)
	}
	if !utf8.ValidString(out) {
		t.Error("output is not valid UTF-8")
	}
}

func TestWriteEmptyStillHasHeader(t *testing.T) {
	f := hcmFormat(t)
	var buf bytes.Buffer
	if _, err := f.Write(&buf, nil); err != nil {
		t.Fatal(err)
	}
	records, err := f.Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Errorf("got %d records from an empty export", len(records))
	}
}

func TestReadRoundTrip(t *testing.T) {
	f := hcmFormat(t)
	trips := sampleTrips()
	var buf bytes.Buffer
	if _, err := f.Write(&buf, trips); err != nil {
		t.Fatal(err)
	}
	records, err := f.Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != len(trips) {
		t.Fatalf("got %d records, want %d", len(records), len(trips))
	}
	for i, tr := range trips {
		want := f.Record(tr)
		if strings.Join(records[i], "\x00") != strings.Join(want, "\x00") {
			t.Errorf("record %d = %q, want %q", i, records[i], want)
		}
	}
}

func TestReadRejectsForeignFiles(t *testing.T) {
	f := hcmFormat(t)
	tests := []struct{ name, in string }{
		{"empty", ""},
		{"wrong header", "A|B|C\n"},
		{"wrong column count", strings.Join(Columns[:3], "|") + "\n"},
		{"ragged row", strings.Join(Columns, "|") + "\nx|y\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := f.Read(strings.NewReader(tt.in)); err == nil {
				t.Error("accepted")
			}
		})
	}
}

func TestFormatValidate(t *testing.T) {
	loc := time.UTC
	bad := []Format{{}, {Delimiter: '"', Location: loc}, {Delimiter: '\n', Location: loc}, {Delimiter: '|'}}
	for _, f := range bad {
		if err := f.Validate(); err == nil {
			t.Errorf("%+v accepted", f)
		}
		if _, err := f.Write(&bytes.Buffer{}, nil); err == nil {
			t.Errorf("Write with %+v accepted", f)
		}
	}
	for _, d := range []rune{'|', ',', ';', '\t'} {
		if err := (Format{Delimiter: d, Location: loc}).Validate(); err != nil {
			t.Errorf("delimiter %q rejected: %v", d, err)
		}
	}
}

func TestTimestampUsesTheFormatZone(t *testing.T) {
	f := hcmFormat(t)
	at := time.Date(2026, 10, 28, 17, 0, 0, 0, time.UTC)
	if got := f.Timestamp(at); got != "2026-10-29T00:00:00+07:00" {
		t.Errorf("Timestamp = %q", got)
	}
	f.Location = time.UTC
	if got := f.Timestamp(at); got != "2026-10-28T17:00:00+00:00" {
		t.Errorf("UTC Timestamp = %q", got)
	}
}

func TestWriteReportsWriterErrors(t *testing.T) {
	f := hcmFormat(t)
	_, err := f.Write(failingWriter{}, sampleTrips())
	if err == nil {
		t.Fatal("expected a write error")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWriter }

var errWriter = &writeError{}

type writeError struct{}

func (*writeError) Error() string { return "disk full" }

func FuzzCSVRoundTrip(f *testing.F) {
	f.Add("SKY1", "CAR", "1 Lê Lợi", "Airport", "VND", "CARD", "SUCCESS", int64(150000), int64(0))
	f.Add("a|b", `q"uote`, "new\nline", "", "", "", "", int64(-1), int64(1<<62))
	f.Add("", "", "", "", "", "", "", int64(0), int64(0))
	f.Fuzz(func(t *testing.T, user, service, pickup, dropoff, currency, method, status string, fare, pay int64) {
		if !utf8.ValidString(user + service + pickup + dropoff + currency + method + status) {
			t.Skip("encoding/csv does not round-trip invalid UTF-8")
		}
		if strings.Contains(user+service+pickup+dropoff+currency+method+status, "\r") {
			t.Skip("encoding/csv normalises \\r\\n on read")
		}
		format := Format{Delimiter: '|', Location: time.UTC}
		trip := Trip{
			EngagementID: 1, PartnerUserID: user, ServiceType: service,
			PickupLocation: pickup, DropoffLocation: dropoff,
			TotalFare: fare, CustomerPay: pay, Currency: currency, PaymentMethod: method, Status: status,
			BookingTime: time.Unix(1_700_000_000, 0), TripEndTime: time.Unix(1_700_003_600, 0),
		}
		var buf bytes.Buffer
		if _, err := format.Write(&buf, []Trip{trip}); err != nil {
			t.Fatal(err)
		}
		records, err := format.Read(&buf)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if len(records) != 1 {
			t.Fatalf("got %d records", len(records))
		}
		want := format.Record(trip)
		for i := range want {
			if records[0][i] != want[i] {
				t.Errorf("field %s: got %q, want %q", Columns[i], records[0][i], want[i])
			}
		}
	})
}

func BenchmarkWrite(b *testing.B) {
	f := hcmFormat(b)
	trips := make([]Trip, 0, 10_000)
	for i := 0; i < 10_000; i++ {
		tr := sampleTrips()[i%2]
		tr.EngagementID = int64(i)
		trips = append(trips, tr)
	}
	var buf bytes.Buffer
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf.Reset()
		if _, err := f.Write(&buf, trips); err != nil {
			b.Fatal(err)
		}
	}
	b.SetBytes(int64(buf.Len()))
}
