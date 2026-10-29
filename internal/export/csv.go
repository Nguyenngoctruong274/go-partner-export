package export

import (
	"bufio"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"
)

// Trip is one exported row. Amounts are integers in the currency's minor
// unit (VND has none, so 15000 is fifteen thousand dong); nothing in the
// pipeline converts them to floating point.
type Trip struct {
	EngagementID    int64
	PartnerUserID   string
	BookingTime     time.Time
	TripEndTime     time.Time
	ServiceType     string
	PickupLocation  string
	DropoffLocation string
	TotalFare       int64
	Discount        int64
	TollFee         int64
	InsuranceFee    int64
	CustomerPay     int64
	Currency        string
	PaymentMethod   string
	Status          string
}

// Columns is the header row, in the order the partner specified.
var Columns = []string{
	"PARTNER_USER_ID",
	"BOOKING_DATE",
	"TRIP_ID",
	"TRIP_SERVICE_TYPE",
	"PICKUP_LOCATION",
	"DROPOFF_LOCATION",
	"DROPOFF_TIME",
	"TOTAL_TRIP_FARE",
	"DISCOUNT_AMOUNT",
	"TOLL_FEE",
	"INSURANCE_FEE",
	"CUSTOMER_PAY",
	"CURRENCY",
	"PAYMENT_METHOD",
	"TRIP_STATUS",
}

// utf8BOM marks the file as UTF-8 for spreadsheet tools that otherwise
// guess a legacy code page for Vietnamese text.
const utf8BOM = "\xEF\xBB\xBF"

// Format fixes how rows are serialised.
type Format struct {
	// Delimiter separates fields. Partners often ask for '|' because
	// addresses contain commas.
	Delimiter rune
	// Location is the zone timestamps are written in.
	Location *time.Location
}

// Validate checks that the format can be written and read back.
func (f Format) Validate() error {
	switch {
	case f.Delimiter == 0:
		return errors.New("export: delimiter is required")
	case f.Delimiter == '"' || f.Delimiter == '\n' || f.Delimiter == '\r' || f.Delimiter == utf8ReplacementRune:
		return fmt.Errorf("export: delimiter %q cannot be a quote, a line break or invalid UTF-8", f.Delimiter)
	case f.Location == nil:
		return errors.New("export: location is required")
	}
	return nil
}

const utf8ReplacementRune = '�'

// Record returns the fields of t in Columns order.
func (f Format) Record(t Trip) []string {
	return []string{
		t.PartnerUserID,
		f.Timestamp(t.BookingTime),
		strconv.FormatInt(t.EngagementID, 10),
		t.ServiceType,
		t.PickupLocation,
		t.DropoffLocation,
		f.Timestamp(t.TripEndTime),
		strconv.FormatInt(t.TotalFare, 10),
		strconv.FormatInt(t.Discount, 10),
		strconv.FormatInt(t.TollFee, 10),
		strconv.FormatInt(t.InsuranceFee, 10),
		strconv.FormatInt(t.CustomerPay, 10),
		t.Currency,
		t.PaymentMethod,
		t.Status,
	}
}

// Timestamp writes t as RFC 3339 with a numeric offset in the format's
// zone, e.g. 2026-10-28T23:59:59+07:00.
func (f Format) Timestamp(t time.Time) string {
	return t.In(f.Location).Format("2006-01-02T15:04:05-07:00")
}

// Write serialises the header and rows to w and returns the number of
// bytes written. Fields containing the delimiter, a quote or a line break
// are quoted per RFC 4180, so a free-text address never breaks a row.
func (f Format) Write(w io.Writer, trips []Trip) (int64, error) {
	if err := f.Validate(); err != nil {
		return 0, err
	}
	cw := &countingWriter{w: w}
	bw := bufio.NewWriter(cw)
	if _, err := bw.WriteString(utf8BOM); err != nil {
		return cw.n, err
	}
	enc := csv.NewWriter(bw)
	enc.Comma = f.Delimiter
	if err := enc.Write(Columns); err != nil {
		return cw.n, fmt.Errorf("export: write header: %w", err)
	}
	for i, t := range trips {
		if err := enc.Write(f.Record(t)); err != nil {
			return cw.n, fmt.Errorf("export: write row %d: %w", i, err)
		}
	}
	enc.Flush()
	if err := enc.Error(); err != nil {
		return cw.n, fmt.Errorf("export: write rows: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return cw.n, fmt.Errorf("export: flush: %w", err)
	}
	return cw.n, nil
}

// Read parses a file written by Write back into records (header excluded).
// It is the other half of the round trip the tests and the re-push check
// rely on.
func (f Format) Read(r io.Reader) ([][]string, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	br := bufio.NewReader(r)
	if b, err := br.Peek(len(utf8BOM)); err == nil && string(b) == utf8BOM {
		_, _ = br.Discard(len(utf8BOM))
	}
	dec := csv.NewReader(br)
	dec.Comma = f.Delimiter
	dec.FieldsPerRecord = len(Columns)
	records, err := dec.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("export: read: %w", err)
	}
	if len(records) == 0 {
		return nil, errors.New("export: read: missing header")
	}
	for i, c := range Columns {
		if records[0][i] != c {
			return nil, fmt.Errorf("export: read: header column %d is %q, want %q", i, records[0][i], c)
		}
	}
	return records[1:], nil
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
