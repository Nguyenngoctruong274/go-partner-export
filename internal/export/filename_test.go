package export

import (
	"testing"
	"time"
)

func TestFileName(t *testing.T) {
	n := Naming{Prefix: "PartnerAccrual", Version: "v1"}
	day := Day{2026, time.October, 28}
	tests := []struct {
		seq  int
		want string
	}{
		{1, "PartnerAccrual_v1_20261028_001.csv"},
		{0, "PartnerAccrual_v1_20261028_001.csv"},
		{-3, "PartnerAccrual_v1_20261028_001.csv"},
		{12, "PartnerAccrual_v1_20261028_012.csv"},
		{1000, "PartnerAccrual_v1_20261028_1000.csv"},
	}
	for _, tt := range tests {
		if got := n.FileName(day, tt.seq); got != tt.want {
			t.Errorf("FileName(seq=%d) = %q, want %q", tt.seq, got, tt.want)
		}
	}
	if got := (Naming{Prefix: "Be_Affiliate_Accrual", Version: "v0"}).FileName(Day{2026, time.January, 5}, 2); got != "Be_Affiliate_Accrual_v0_20260105_002.csv" {
		t.Errorf("prefix with underscores: %q", got)
	}
}

func TestNamingValidate(t *testing.T) {
	ok := []Naming{{"PartnerAccrual", "v1"}, {"a-b_c", "2"}, {"X", "v1-beta"}}
	for _, n := range ok {
		if err := n.Validate(); err != nil {
			t.Errorf("%+v rejected: %v", n, err)
		}
	}
	bad := []Naming{{"", "v1"}, {"Partner Accrual", "v1"}, {"Partner", ""}, {"Partner", "v1/2"}, {"Partner.", "v1"}, {"Pártner", "v1"}}
	for _, n := range bad {
		if err := n.Validate(); err == nil {
			t.Errorf("%+v accepted", n)
		}
	}
	if err := (Naming{"", ""}).Validate(); err == nil || len(err.Error()) < 40 {
		t.Errorf("both errors should be reported, got %v", err)
	}
}

func TestParseFileNameRoundTrip(t *testing.T) {
	for _, n := range []Naming{{"PartnerAccrual", "v1"}, {"Be_Affiliate_Accrual", "v0"}, {"x-y", "2026-10"}} {
		for _, seq := range []int{1, 7, 999} {
			day := Day{2026, time.February, 28}
			name := n.FileName(day, seq)
			p, err := ParseFileName(name)
			if err != nil {
				t.Fatalf("ParseFileName(%q): %v", name, err)
			}
			want := ParsedName{Prefix: n.Prefix, Version: n.Version, Day: day, Sequence: seq}
			if p != want {
				t.Errorf("ParseFileName(%q) = %+v, want %+v", name, p, want)
			}
		}
	}
}

func TestParseFileNameRejects(t *testing.T) {
	bad := []string{
		"",
		"PartnerAccrual_v1_20261028_001.txt",
		"PartnerAccrual_v1_20261028.csv",
		"PartnerAccrual_v1_2026102_001.csv",
		"PartnerAccrual_v1_20261340_001.csv",
		"PartnerAccrual_v1_20261028_000.csv",
		"PartnerAccrual_v1_20261028_1.csv",
		"PartnerAccrual_v1_20261028_abc.csv",
		"Partner Accrual_v1_20261028_001.csv",
	}
	for _, name := range bad {
		if _, err := ParseFileName(name); err == nil {
			t.Errorf("ParseFileName(%q) accepted", name)
		}
	}
}

func FuzzParseFileNameNeverPanics(f *testing.F) {
	f.Add("PartnerAccrual_v1_20261028_001.csv")
	f.Add("a_b_c_d.csv")
	f.Add("___.csv")
	f.Add(".csv")
	f.Fuzz(func(t *testing.T, name string) {
		p, err := ParseFileName(name)
		if err != nil {
			return
		}
		// Whatever parses must re-serialise to the same name.
		got := (Naming{Prefix: p.Prefix, Version: p.Version}).FileName(p.Day, p.Sequence)
		if got != name {
			t.Errorf("round trip of %q gave %q", name, got)
		}
	})
}
