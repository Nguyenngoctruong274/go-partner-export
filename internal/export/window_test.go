package export

import (
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestDayOfUsesTheZone(t *testing.T) {
	hcm := mustLoc(t, "Asia/Ho_Chi_Minh")
	// 2026-10-28 23:30 UTC is already 2026-10-29 06:30 in Ho Chi Minh.
	at := time.Date(2026, 10, 28, 23, 30, 0, 0, time.UTC)
	if got := DayOf(at, time.UTC); got != (Day{2026, time.October, 28}) {
		t.Errorf("UTC day = %v", got)
	}
	if got := DayOf(at, hcm); got != (Day{2026, time.October, 29}) {
		t.Errorf("HCM day = %v", got)
	}
}

func TestReportDayIsThePreviousLocalDay(t *testing.T) {
	hcm := mustLoc(t, "Asia/Ho_Chi_Minh")
	tests := []struct {
		name string
		now  time.Time
		want Day
	}{
		{"01:00 local, the scheduled run", time.Date(2026, 10, 28, 18, 0, 0, 0, time.UTC), Day{2026, time.October, 28}},
		{"just before local midnight", time.Date(2026, 10, 28, 16, 59, 59, 0, time.UTC), Day{2026, time.October, 27}},
		{"exactly local midnight", time.Date(2026, 10, 28, 17, 0, 0, 0, time.UTC), Day{2026, time.October, 28}},
		{"first day of a month", time.Date(2026, 11, 1, 1, 0, 0, 0, hcm), Day{2026, time.October, 31}},
		{"first day of a year", time.Date(2027, 1, 1, 1, 0, 0, 0, hcm), Day{2026, time.December, 31}},
		{"day after leap day", time.Date(2028, 3, 1, 1, 0, 0, 0, hcm), Day{2028, time.February, 29}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReportDay(tt.now, hcm); got != tt.want {
				t.Errorf("ReportDay = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWindowIsHalfOpenInTheZone(t *testing.T) {
	hcm := mustLoc(t, "Asia/Ho_Chi_Minh")
	day := Day{2026, time.October, 28}
	from, to := day.Window(hcm)
	if !from.Equal(time.Date(2026, 10, 27, 17, 0, 0, 0, time.UTC)) {
		t.Errorf("from = %v", from)
	}
	if !to.Equal(time.Date(2026, 10, 28, 17, 0, 0, 0, time.UTC)) {
		t.Errorf("to = %v", to)
	}
	if to.Sub(from) != 24*time.Hour {
		t.Errorf("window length = %v", to.Sub(from))
	}
	if !day.Contains(from, hcm) {
		t.Error("window start is excluded")
	}
	if day.Contains(to, hcm) {
		t.Error("window end is included")
	}
	if day.Contains(from.Add(-time.Nanosecond), hcm) {
		t.Error("instant before the window is included")
	}
}

func TestWindowAcrossDaylightSaving(t *testing.T) {
	// Europe/Berlin leaves DST on 2026-10-25: that local day has 25 hours.
	berlin := mustLoc(t, "Europe/Berlin")
	from, to := (Day{2026, time.October, 25}).Window(berlin)
	if got := to.Sub(from); got != 25*time.Hour {
		t.Errorf("DST-end day is %v long, want 25h", got)
	}
	from, to = (Day{2026, time.March, 29}).Window(berlin)
	if got := to.Sub(from); got != 23*time.Hour {
		t.Errorf("DST-start day is %v long, want 23h", got)
	}
}

func TestDayArithmeticAndFormatting(t *testing.T) {
	d := Day{2026, time.December, 31}
	if got := d.AddDays(1); got != (Day{2027, time.January, 1}) {
		t.Errorf("AddDays(1) = %v", got)
	}
	if got := d.AddDays(-365); got != (Day{2025, time.December, 31}) {
		t.Errorf("AddDays(-365) = %v", got)
	}
	if d.String() != "2026-12-31" || d.Compact() != "20261231" {
		t.Errorf("String/Compact = %q/%q", d.String(), d.Compact())
	}
	if !(Day{2026, time.January, 31}).Before(Day{2026, time.February, 1}) {
		t.Error("Before across months")
	}
	if !(Day{2025, time.December, 31}).Before(Day{2026, time.January, 1}) {
		t.Error("Before across years")
	}
	if (Day{2026, time.March, 2}).Before(Day{2026, time.March, 1}) {
		t.Error("Before within a month")
	}
	if !(Day{}).IsZero() || d.IsZero() {
		t.Error("IsZero")
	}
}

func TestParseDay(t *testing.T) {
	if got, err := ParseDay("2026-02-28"); err != nil || got != (Day{2026, time.February, 28}) {
		t.Errorf("ParseDay = %v, %v", got, err)
	}
	for _, bad := range []string{"", "2026-2-28", "28/02/2026", "2026-02-30", "20260228"} {
		if _, err := ParseDay(bad); err == nil {
			t.Errorf("ParseDay(%q) accepted", bad)
		}
	}
}

func TestNextRunAt(t *testing.T) {
	hcm := mustLoc(t, "Asia/Ho_Chi_Minh")
	berlin := mustLoc(t, "Europe/Berlin")
	tests := []struct {
		name         string
		now          time.Time
		loc          *time.Location
		hour, minute int
		want         time.Time
	}{
		{
			name: "later today",
			now:  time.Date(2026, 10, 29, 0, 30, 0, 0, hcm), loc: hcm, hour: 1, minute: 0,
			want: time.Date(2026, 10, 29, 1, 0, 0, 0, hcm),
		},
		{
			name: "already passed, so tomorrow",
			now:  time.Date(2026, 10, 29, 1, 0, 0, 0, hcm), loc: hcm, hour: 1, minute: 0,
			want: time.Date(2026, 10, 30, 1, 0, 0, 0, hcm),
		},
		{
			name: "now given in another zone",
			now:  time.Date(2026, 10, 28, 17, 30, 0, 0, time.UTC), loc: hcm, hour: 1, minute: 0, // 00:30 HCM
			want: time.Date(2026, 10, 29, 1, 0, 0, 0, hcm),
		},
		{
			name: "skipped local hour resolves forward",
			now:  time.Date(2026, 3, 29, 1, 0, 0, 0, berlin), loc: berlin, hour: 2, minute: 30, // 02:30 does not exist
			want: time.Date(2026, 3, 29, 3, 30, 0, 0, berlin),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NextRunAt(tt.now, tt.loc, tt.hour, tt.minute)
			if !got.Equal(tt.want) {
				t.Errorf("NextRunAt = %v, want %v", got, tt.want)
			}
			if !got.After(tt.now) {
				t.Errorf("NextRunAt = %v is not after now %v", got, tt.now)
			}
		})
	}
}
