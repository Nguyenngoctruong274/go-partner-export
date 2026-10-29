// Package export holds the pure domain of a partner export: which day a
// report covers, how its file is named and how its rows are written. It
// does no I/O, so every rule here is covered by fast table tests.
package export

import (
	"fmt"
	"time"
)

// Day is a calendar day in the partner's time zone. It is the unit of
// reporting: one run, one file, one row in the run history.
type Day struct {
	Year  int
	Month time.Month
	Date  int
}

// DayOf returns the calendar day that t falls on in loc.
func DayOf(t time.Time, loc *time.Location) Day {
	y, m, d := t.In(loc).Date()
	return Day{Year: y, Month: m, Date: d}
}

// ParseDay reads a day written as YYYY-MM-DD.
func ParseDay(s string) (Day, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return Day{}, fmt.Errorf("export: day must be YYYY-MM-DD: %w", err)
	}
	return DayOf(t, time.UTC), nil
}

// Previous returns the day before.
func (d Day) Previous() Day {
	return d.AddDays(-1)
}

// AddDays returns the day n days later (earlier for negative n). It goes
// through time.Date, so month and year roll over correctly.
func (d Day) AddDays(n int) Day {
	t := time.Date(d.Year, d.Month, d.Date+n, 0, 0, 0, 0, time.UTC)
	return DayOf(t, time.UTC)
}

// Window returns the half-open interval [from, to) that the day covers in
// loc. It is built with time.Date, so a daylight-saving change makes the
// window 23 or 25 hours long instead of shifting its edges.
func (d Day) Window(loc *time.Location) (from, to time.Time) {
	from = time.Date(d.Year, d.Month, d.Date, 0, 0, 0, 0, loc)
	to = time.Date(d.Year, d.Month, d.Date+1, 0, 0, 0, 0, loc)
	return from, to
}

// Contains reports whether t falls on the day in loc.
func (d Day) Contains(t time.Time, loc *time.Location) bool {
	from, to := d.Window(loc)
	return !t.Before(from) && t.Before(to)
}

// String formats the day as YYYY-MM-DD.
func (d Day) String() string {
	return fmt.Sprintf("%04d-%02d-%02d", d.Year, d.Month, d.Date)
}

// Compact formats the day as YYYYMMDD, the form used in file names.
func (d Day) Compact() string {
	return fmt.Sprintf("%04d%02d%02d", d.Year, d.Month, d.Date)
}

// IsZero reports whether d was never set.
func (d Day) IsZero() bool {
	return d == Day{}
}

// Before reports whether d is earlier than other.
func (d Day) Before(other Day) bool {
	if d.Year != other.Year {
		return d.Year < other.Year
	}
	if d.Month != other.Month {
		return d.Month < other.Month
	}
	return d.Date < other.Date
}

// ReportDay returns the day a run started at now should export: the
// previous local day, so that a run shortly after midnight covers the
// whole day that just ended.
func ReportDay(now time.Time, loc *time.Location) Day {
	return DayOf(now, loc).Previous()
}

// NextRunAt returns the first instant after now at which the local clock in
// loc reads hour:minute. A missing local time (the hour skipped by a
// daylight-saving jump) resolves to the following valid instant, as
// time.Date normalises it.
func NextRunAt(now time.Time, loc *time.Location, hour, minute int) time.Time {
	local := now.In(loc)
	next := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
	if !next.After(now) {
		next = time.Date(local.Year(), local.Month(), local.Day()+1, hour, minute, 0, 0, loc)
	}
	return next
}
