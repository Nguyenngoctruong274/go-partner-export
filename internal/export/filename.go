package export

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Naming describes the file-name scheme agreed with the partner:
// <Prefix>_<Version>_<YYYYMMDD>_<NNN>.csv.
type Naming struct {
	Prefix  string
	Version string
}

var namePartRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Validate reports whether both parts can appear in a file name.
func (n Naming) Validate() error {
	var errs []error
	if !namePartRe.MatchString(n.Prefix) {
		errs = append(errs, fmt.Errorf("export: prefix %q must match %s", n.Prefix, namePartRe))
	}
	if !namePartRe.MatchString(n.Version) {
		errs = append(errs, fmt.Errorf("export: version %q must match %s", n.Version, namePartRe))
	}
	return errors.Join(errs...)
}

// FileName returns the name of the file for day with the given sequence
// number. The sequence distinguishes several files for one day; the daily
// run always sends sequence 1. Sequences below 1 are treated as 1.
func (n Naming) FileName(day Day, sequence int) string {
	if sequence < 1 {
		sequence = 1
	}
	return fmt.Sprintf("%s_%s_%s_%03d.csv", n.Prefix, n.Version, day.Compact(), sequence)
}

// ParsedName is a file name taken apart again.
type ParsedName struct {
	Prefix   string
	Version  string
	Day      Day
	Sequence int
}

// ParseFileName takes a file name apart. It is the inverse of FileName
// and is used to recognise files already on the partner's server.
func ParseFileName(name string) (ParsedName, error) {
	base, ok := strings.CutSuffix(name, ".csv")
	if !ok {
		return ParsedName{}, fmt.Errorf("export: %q: not a .csv file", name)
	}
	parts := strings.Split(base, "_")
	if len(parts) < 4 {
		return ParsedName{}, fmt.Errorf("export: %q: want <prefix>_<version>_<YYYYMMDD>_<NNN>.csv", name)
	}
	// The prefix itself may contain underscores; the last three parts are fixed.
	seqPart, dayPart, verPart := parts[len(parts)-1], parts[len(parts)-2], parts[len(parts)-3]
	prefix := strings.Join(parts[:len(parts)-3], "_")

	seq, err := strconv.Atoi(seqPart)
	if err != nil || seq < 1 || len(seqPart) != 3 {
		return ParsedName{}, fmt.Errorf("export: %q: sequence must be three digits from 001", name)
	}
	if len(dayPart) != 8 {
		return ParsedName{}, fmt.Errorf("export: %q: day must be YYYYMMDD", name)
	}
	day, err := ParseDay(dayPart[:4] + "-" + dayPart[4:6] + "-" + dayPart[6:])
	if err != nil {
		return ParsedName{}, fmt.Errorf("export: %q: %w", name, err)
	}
	p := ParsedName{Prefix: prefix, Version: verPart, Day: day, Sequence: seq}
	if err := (Naming{Prefix: prefix, Version: verPart}).Validate(); err != nil {
		return ParsedName{}, err
	}
	return p, nil
}
